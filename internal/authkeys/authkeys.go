// Package authkeys renders and atomically writes the sshd authorized_keys file
// from the client list (design doc D4 + the outside-voice hardening).
//
// Each enabled client gets one line with a per-mode FORCED command, locked to
// that client's own directory, so the SSH key can do nothing but that client's
// backup into that client's directory:
//
//	rsync : restrict,command="rrsync <baseDir>/<slug>"        <pubkey> backitup:<name>
//	targz : restrict,command="backitup-recv <baseDir>/<slug>" <pubkey> backitup:<name>
//
// An rsync client with the record-ownership setting gets backitup-rrsync in
// place of rrsync — same jail, with --fake-super added to the rsync the server
// runs so the unprivileged receiver records the source's uid/gid in an xattr.
// That is why this file is the setting's real effect: the forced command is
// what the client is allowed to run, so the setting has to be written here.
//
// Confinement comes from the command itself (rrsync's path jail; the receiver
// script's hardcoded directory), NOT from client-supplied input. The directory
// comes from the client's persisted, UNIQUE slug column: it is computed once on
// create, never recomputed from the name here, and never sent by the client. A
// client whose slug is empty or is not a plain [a-z0-9-] component gets no line
// at all — filepath.Join(baseDir, "") is baseDir, i.e. every client's directory,
// and anything else (quotes, commas, spaces) would append options to the line.
//
// Injection defense: a public key is only emitted if it parses as a single,
// well-formed authorized_keys entry with no embedded newlines or quotes. A
// malformed/poisoned row is SKIPPED (that client loses access) rather than
// failing the whole file — one bad row must not break auth for the fleet.
package authkeys

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/th0rn0/backitup/internal/atomicfile"
	"github.com/th0rn0/backitup/internal/model"
)

// Header is written at the top of every generated file. The file is fully
// regenerated from the DB on each change; do not hand-edit.
const Header = "# Managed by backitup. Do not edit; regenerated from the database.\n"

// Skipped records a client whose key was rejected and why.
type Skipped struct {
	ClientID int64
	Reason   string
}

// Render builds the authorized_keys body for all ENABLED clients with a key,
// confining each to baseDir/<slug> — the client's PERSISTED slug. A client whose
// slug is empty or not a plain directory component is skipped rather than jailed
// to the base dir (which every client shares). Invalid entries are skipped and
// returned.
func Render(clients []model.Client, baseDir string) (content string, skipped []Skipped) {
	var b strings.Builder
	b.WriteString(Header)

	sorted := append([]model.Client(nil), clients...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	for _, c := range sorted {
		if !c.Enabled || strings.TrimSpace(c.SSHPubKey) == "" {
			continue
		}
		if reason := invalidPubKey(c.SSHPubKey); reason != "" {
			skipped = append(skipped, Skipped{ClientID: c.ID, Reason: reason})
			continue
		}
		if reason := InvalidSlug(c.Slug); reason != "" {
			skipped = append(skipped, Skipped{ClientID: c.ID, Reason: reason})
			continue
		}
		cmd, ok := forcedCommand(c, baseDir)
		if !ok {
			skipped = append(skipped, Skipped{ClientID: c.ID, Reason: "unknown mode " + string(c.Mode)})
			continue
		}
		// restrict = drop all forwarding/pty/agent; command = the only thing this
		// key may run. The pubkey is validated above, so it is safe to embed.
		fmt.Fprintf(&b, "restrict,command=\"%s\" %s\n", cmd, strings.TrimSpace(c.SSHPubKey))
	}
	return b.String(), skipped
}

// slugPattern is the ONLY shape a storage slug may take: a short, plain,
// lowercase directory component. It is an allowlist on purpose — the slug is
// persisted and embedded in an sshd authorized_keys forced command, so a value
// carrying a quote, comma, space, "$(...)", non-ASCII byte or a second
// "command=" would append options to that line. Filters that try to strip the
// bad characters keep missing one; permitting only [a-z0-9-] cannot.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// InvalidSlug returns a non-empty reason if slug cannot be used as a single
// directory component. An empty slug is the dangerous case: filepath.Join(base,
// "") is base itself, so every such client would share one read-write jail.
//
// Exported because it is the single definition of "a slug that may become a
// path": authkeys uses it before writing a forced command, and the server and
// lifecycle packages use it (via clientDir) before joining a base directory.
// Anything building a per-client path must go through it rather than re-deriving
// the rule.
func InvalidSlug(slug string) string {
	if slug == "" {
		return "empty slug"
	}
	if !slugPattern.MatchString(slug) {
		return fmt.Sprintf("unsafe slug %q: want ^[a-z0-9][a-z0-9-]{0,62}$", slug)
	}
	return ""
}

func forcedCommand(c model.Client, baseDir string) (string, bool) {
	dir := filepath.Join(baseDir, c.Slug)
	switch c.Mode {
	case model.ModeRsync:
		// rrsync confines reads+writes to dir (needed for --link-dest + latest).
		//
		// A client that records ownership gets backitup-rrsync instead: the same
		// jail, with --fake-super added to the rsync the server runs, so the
		// unprivileged receiver stores the source's uid/gid in an xattr rather
		// than silently writing its own. It is a different command, not a client
		// flag, because this forced command is the only thing the key may run —
		// the client cannot ask for anything else.
		//
		// Only for rsync mode: tar.gz's receiver is not rsync at all, and its
		// archive already records the numeric owner.
		if c.RecordOwnership {
			return "backitup-rrsync " + dir, true
		}
		return "rrsync " + dir, true
	case model.ModeTarGz:
		// Forced receiver writes the streamed archive into dir; no sftp/chroot
		// needed, and the client cannot touch any other path.
		return "backitup-recv " + dir, true
	default:
		return "", false
	}
}

// invalidPubKey returns a non-empty reason if key is not a safe single
// authorized_keys entry. Guards against ForceCommand injection via embedded
// newlines/quotes and against malformed keys.
func invalidPubKey(key string) string {
	if strings.ContainsAny(key, "\n\r") {
		return "embedded newline"
	}
	if strings.Contains(key, "\"") {
		return "embedded quote"
	}
	for _, r := range key {
		if r < 0x20 { // control characters
			return "control character"
		}
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(key)); err != nil {
		return "unparseable public key"
	}
	return ""
}

// WriteAtomic writes content to path via a temp file in the same directory and
// a rename, so sshd never reads a half-written authorized_keys file (D4). The
// mechanics live in internal/atomicfile, shared with rclone.conf.
func WriteAtomic(path, content string) error {
	return atomicfile.Write(path, []byte(content), 0o600)
}
