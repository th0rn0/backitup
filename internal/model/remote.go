package model

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RemoteBackend is the rclone backend type for a configured remote.
type RemoteBackend string

// Offsite storage is Backblaze B2 and nothing else (Thorno, 2026-11-19). The
// generic rclone providers (s3, s3-compat, drive, sftp, webdav, azureblob, ftp)
// are gone from this build: a second provider is a second set of credentials,
// endpoints and failure modes to support, and the project supports one.
const (
	// BackendB2 is the only supported offsite destination.
	BackendB2 RemoteBackend = "b2"
	// BackendCrypt encrypts another remote's contents on the way out. It is not
	// a provider — it wraps a B2 remote — and stays because B2 server-side
	// encryption does not protect the backup from the provider.
	BackendCrypt RemoteBackend = "crypt"
	// BackendLocal is rclone's `local` backend, kept for tests only: it is what
	// the end-to-end offsite proof writes to, so the upload → dedupe → verify →
	// decrypt path can run without a provider account. It is deliberately NOT in
	// Backends (so no form offers it) and the create handler refuses it (so the
	// API cannot configure one). A remote carrying it only ever exists in a test.
	BackendLocal RemoteBackend = "local"
)

type RemoteFieldType string

const (
	FieldText     RemoteFieldType = "text"
	FieldPassword RemoteFieldType = "password"
	FieldTextarea RemoteFieldType = "textarea"
	FieldSelect   RemoteFieldType = "select"
)

type SelectOpt struct{ Value, Label string }

// RemoteField describes one configuration field for a backend.
type RemoteField struct {
	Key      string
	Label    string
	Type     RemoteFieldType
	Options  []SelectOpt // for FieldSelect
	Required bool
	Hint     string
	Default  string
	// Obscure is true for pass fields that rclone requires to be scrambled with
	// `rclone obscure` (the crypt remote's password/password2). The stored value
	// is already obscured; the raw password is never persisted.
	Obscure bool
}

// AllowsValue reports whether v is acceptable for this field. Select fields
// carry a fixed vocabulary (rclone rejects a config line whose value is not one
// of its known options), so the handler checks the submitted value against it
// rather than trusting the form.
func (f RemoteField) AllowsValue(v string) bool {
	if f.Type != FieldSelect || len(f.Options) == 0 {
		return true
	}
	for _, o := range f.Options {
		if o.Value == v {
			return true
		}
	}
	return false
}

// BackendDef describes a supported rclone backend and all its config fields.
type BackendDef struct {
	ID     RemoteBackend
	Label  string
	Fields []RemoteField
	// PathHint is shown as the per-client offsite_dir field hint.
	PathHint string
	// PathLabel overrides the default "Backup path" label in the client form.
	PathLabel string
}

// Backends is the ordered list of backends a remote can be created as. It has
// exactly two entries and neither is a provider choice: B2 is the provider,
// crypt is the optional client-side encryption wrapper. Anything else is
// refused at the boundary (see server.postCreateRemote), and BackendLocal is
// intentionally absent — it exists for tests only.
var Backends = []BackendDef{
	{
		// crypt encrypts another remote's contents on the way out, so it is
		// listed first: "the provider only ever sees ciphertext" is the
		// posture the README promises, and this is the backend that delivers
		// it. It needs a B2 remote to wrap, created first.
		ID: BackendCrypt, Label: "Encrypted (rclone crypt) — recommended",
		PathHint:  "Path inside the encrypted remote — e.g. clients. Directory and file names are encrypted too.",
		PathLabel: "Encrypted path",
		Fields: []RemoteField{
			{
				Key: "remote", Label: "Plaintext remote", Type: FieldText, Required: true,
				Hint: "Name of an existing B2 remote that this one wraps — create the B2 remote first. Everything written through this remote is encrypted before it reaches B2.",
			},
			{
				Key: "filename_encryption", Label: "Filename encryption", Type: FieldSelect, Default: "standard",
				Hint: "Standard encrypts file names, obscure also pads them to hide their length, off leaves them readable (contents are still encrypted). Leaving this unset keeps rclone's default, which is standard.",
				Options: []SelectOpt{
					{Value: "standard", Label: "Standard — encrypt file names"},
					{Value: "obscure", Label: "Obscure — also pad names to hide their length"},
					{Value: "off", Label: "Off — keep file names readable (contents are still encrypted)"},
				},
			},
			{
				Key: "directory_name_encryption", Label: "Directory name encryption", Type: FieldSelect, Default: "true",
				Hint: "Encrypt directory names on the provider as well as file names. Turning this off leaves folder names readable — only do that when another tool has to read the folder layout.",
				Options: []SelectOpt{
					{Value: "true", Label: "On — encrypt directory names (rclone's default)"},
					{Value: "false", Label: "Off — leave directory names readable"},
				},
			},
			{
				Key: "password", Label: "Encryption password", Type: FieldPassword, Required: true, Obscure: true,
				Hint: "Never displayed again after saving. Losing it makes the offsite copies unreadable — store it in your password manager.",
			},
			{
				Key: "password2", Label: "Encryption salt (password 2)", Type: FieldPassword, Required: true, Obscure: true,
				Hint: "Second part of the key (rclone's password2). Use a different value from the password. Also never displayed again.",
			},
		},
	},
	{
		// B2 is the only provider. rclone addresses a B2 object as
		// <remote>:<bucket>/<path>, so the bucket (and the optional prefix) are
		// stored on the remote and prepended to every object path by
		// RootPath. They are written into the managed section too, so the file
		// says which bucket it feeds; rclone has no such options and ignores
		// unknown keys (verified against rclone 1.76 — a section carrying them
		// still authorises and addresses the bucket).
		ID: BackendB2, Label: "Backblaze B2",
		PathHint:  "Path inside the bucket — e.g. clients. Leave blank to use the client's slug.",
		PathLabel: "Bucket path",
		Fields: []RemoteField{
			{
				Key: "account", Label: "Application Key ID (keyID)", Type: FieldText, Required: true,
				Hint: "B2 console → Application Keys. The keyID identifies the key; the applicationKey below is its secret. Give the key access to this one bucket only.",
			},
			{
				Key: "key", Label: "Application Key (applicationKey)", Type: FieldPassword, Required: true,
				Hint: "Shown once when the application key is created. Without it rclone cannot upload, and it cannot be read back out of B2.",
			},
			{
				Key: "bucket", Label: "Bucket", Type: FieldText, Required: true,
				Hint: "The B2 bucket that receives the backups — e.g. my-backups. Every client on this remote writes under it.",
			},
			{
				Key: "path", Label: "Path prefix", Type: FieldText,
				Hint: "Optional prefix inside the bucket, e.g. backups — useful when several backitup servers share one bucket. Leave blank to write at the top of the bucket.",
			},
		},
	},
}

// FindBackend returns the BackendDef for id, or nil if not found.
func FindBackend(id RemoteBackend) *BackendDef {
	for i := range Backends {
		if Backends[i].ID == id {
			return &Backends[i]
		}
	}
	return nil
}

// Remote is one configured offsite destination stored in the database.
type Remote struct {
	ID        int64
	Name      string
	Backend   RemoteBackend
	Config    map[string]string // backend-specific key-value config
	CreatedAt time.Time
	// Unsupported marks a row whose backend this build no longer supports —
	// written by an older backitup that still offered S3, Drive, SFTP and so
	// on. The row is kept (deleting an operator's configured remote behind
	// their back is data loss) but nothing uses it: the lifecycle skips it, the
	// UI says to recreate it as B2, and the edit form is read-only. It is set
	// by migration v4 and never by the create path.
	Unsupported bool
}

// BackendLabel returns the human label for this remote's backend.
func (r Remote) BackendLabel() string {
	if b := FindBackend(r.Backend); b != nil {
		return b.Label
	}
	return string(r.Backend)
}

// IsCrypt reports whether this remote encrypts another remote's contents on the
// way out (rclone's crypt backend).
func (r Remote) IsCrypt() bool {
	return r.Backend == BackendCrypt
}

// CryptParent returns the name of the plaintext remote a crypt remote wraps —
// the remote rclone actually talks to, which therefore only ever receives
// ciphertext — or "" for any other backend. The trailing colon rclone.conf
// requires is not part of the name.
func (r Remote) CryptParent() string {
	if !r.IsCrypt() {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(r.Config["remote"]), ":")
}

// RootPath returns the path every object for this remote is written under:
// the B2 bucket plus the optional prefix ("my-bucket/backups"). A remote with
// no bucket configured — a crypt wrapper, a row written before buckets moved
// onto the remote, or a test-only local remote pointed at a directory — returns
// "" or its configured root and leaves paths exactly as they were.
//
// An absolute bucket (the test-only local backend addresses a directory this
// way) keeps its leading slash.
func (r Remote) RootPath() string {
	bucket := strings.TrimSpace(r.Config["bucket"])
	prefix := strings.TrimSpace(r.Config["path"])
	parts := make([]string, 0, 2)
	for _, p := range []string{bucket, prefix} {
		if p = strings.Trim(p, "/"); p != "" {
			parts = append(parts, p)
		}
	}
	root := strings.Join(parts, "/")
	if strings.HasPrefix(bucket, "/") {
		root = "/" + root
	}
	return root
}

// cryptRemoteSpec returns the `remote = <name>:<path>` value a crypt section
// needs. rclone wants the parent remote and, when the parent writes inside a
// bucket/prefix of its own, that path — otherwise the crypt would address the
// provider's top level and put the backup outside the bucket the parent names.
func (r Remote) cryptRemoteSpec(parents func(name string) (Remote, bool)) string {
	name := strings.TrimSuffix(strings.TrimSpace(r.Config["remote"]), ":")
	if i := strings.IndexByte(name, ':'); i >= 0 {
		// An operator's own value may already carry a path; keep it.
		name = strings.TrimSpace(name[:i])
	}
	spec := name + ":"
	if parents == nil || name == "" {
		return spec
	}
	p, ok := parents(name)
	if !ok {
		return spec
	}
	if root := p.RootPath(); root != "" {
		spec = name + ":" + strings.TrimSuffix(root, "/") + "/"
	}
	return spec
}

// RcloneSection returns the [name]\ntype=...\n… block for rclone.conf.
func (r Remote) RcloneSection() string {
	return r.RcloneSectionFor(nil)
}

// RcloneSectionFor is RcloneSection with a way to look up a remote by name.
//
// A crypt section names the plaintext remote rclone writes through, and that
// reference has to carry the parent's own root path: the bucket (and prefix) a
// B2 remote was configured with live on the remote, not in rclone.conf, so a
// bare `remote = b2name:` would make rclone write at the top of the account,
// ignoring the bucket. parents resolves that; nil (or an unknown name, e.g. a
// section the operator wrote by hand) leaves the bare `<name>:` as before.
func (r Remote) RcloneSectionFor(parents func(name string) (Remote, bool)) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s]\n", r.Name)

	switch r.Backend {
	case BackendB2:
		sb.WriteString("type = b2\n")
		// Without hard_delete, rclone only creates a hide marker; the file
		// remains stored (and billed) on B2. hard_delete permanently removes
		// the file and all its versions on deletefile.
		sb.WriteString("hard_delete = true\n")
	case BackendCrypt:
		// crypt does not talk to a provider itself: `remote` names the
		// plaintext remote whose contents it encrypts, so that remote — and
		// whatever sits behind it — only ever sees ciphertext.
		sb.WriteString("type = crypt\n")
	default:
		fmt.Fprintf(&sb, "type = %s\n", string(r.Backend))
	}

	// Keys are written in a fixed order: this file is now preserved around
	// rather than rewritten wholesale, so a needlessly reshuffled section would
	// show up as a spurious change to whoever reads the diff of their config.
	keys := make([]string, 0, len(r.Config))
	for k := range r.Config {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := r.Config[k]
		if v == "" {
			continue
		}
		if r.Backend == BackendCrypt && k == "remote" {
			v = r.cryptRemoteSpec(parents)
		}
		// One value per line: a newline in a value would inject config lines,
		// and a lone \r is a line break to some INI readers even though
		// rclone 1.76 reads it as part of the value.
		v = strings.ReplaceAll(v, "\r\n", " ")
		v = strings.ReplaceAll(v, "\r", " ")
		fmt.Fprintf(&sb, "%s = %s\n", k, strings.ReplaceAll(v, "\n", " "))
	}
	return sb.String()
}
