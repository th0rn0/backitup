package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RemoteBackend is the rclone backend type for a configured remote.
type RemoteBackend string

const (
	BackendS3       RemoteBackend = "s3"        // Amazon S3
	BackendS3Compat RemoteBackend = "s3-compat"  // S3-compatible (Wasabi, MinIO, R2, …)
	BackendB2       RemoteBackend = "b2"         // Backblaze B2
	BackendGDrive   RemoteBackend = "drive"      // Google Drive (service account)
	BackendSFTP     RemoteBackend = "sftp"       // SFTP
	BackendWebDAV   RemoteBackend = "webdav"     // WebDAV
	BackendAzure    RemoteBackend = "azureblob"  // Azure Blob Storage
	BackendFTP      RemoteBackend = "ftp"        // FTP
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
	// Obscure is true for SFTP/FTP/WebDAV pass fields that rclone requires
	// to be scrambled with `rclone obscure`. The stored value is already
	// obscured; the raw password is never persisted.
	Obscure bool
}

// BackendDef describes a supported rclone backend and all its config fields.
type BackendDef struct {
	ID       RemoteBackend
	Label    string
	Fields   []RemoteField
	// PathHint is shown as the per-client offsite_dir field hint.
	PathHint string
	// PathLabel overrides the default "Backup path" label in the client form.
	PathLabel string
}

// Backends is the ordered list of all supported rclone backends.
var Backends = []BackendDef{
	{
		ID: BackendS3, Label: "Amazon S3",
		PathHint:  "Bucket and optional prefix — e.g. my-bucket or my-bucket/clients",
		PathLabel: "Bucket / path",
		Fields: []RemoteField{
			{Key: "access_key_id", Label: "Access Key ID", Type: FieldText, Required: true},
			{Key: "secret_access_key", Label: "Secret Access Key", Type: FieldPassword, Required: true},
			{Key: "region", Label: "Region", Type: FieldText, Required: true, Default: "us-east-1", Hint: "e.g. us-east-1, eu-west-1"},
		},
	},
	{
		ID: BackendS3Compat, Label: "S3-Compatible (Wasabi / MinIO / R2 / …) — untested",
		PathHint:  "Bucket and optional prefix — e.g. my-bucket or my-bucket/clients",
		PathLabel: "Bucket / path",
		Fields: []RemoteField{
			{Key: "access_key_id", Label: "Access Key ID", Type: FieldText, Required: true},
			{Key: "secret_access_key", Label: "Secret Access Key", Type: FieldPassword, Required: true},
			{Key: "endpoint", Label: "Endpoint URL", Type: FieldText, Required: true, Hint: "e.g. https://s3.wasabisys.com or https://<id>.r2.cloudflarestorage.com"},
			{Key: "region", Label: "Region", Type: FieldText, Hint: "Leave blank if the provider does not require one (Cloudflare R2, MinIO)"},
		},
	},
	{
		ID: BackendB2, Label: "Backblaze B2",
		PathHint:  "Bucket name — e.g. my-backups",
		PathLabel: "Bucket / path",
		Fields: []RemoteField{
			{Key: "account", Label: "Application Key ID", Type: FieldText, Required: true},
			{Key: "key", Label: "Application Key", Type: FieldPassword, Required: true},
		},
	},
	{
		ID: BackendGDrive, Label: "Google Drive",
		PathHint:  "Folder path relative to root — leave blank to write directly to the root folder",
		PathLabel: "Folder path (optional)",
		Fields: []RemoteField{
			{
				Key: "service_account_credentials", Label: "Service Account JSON",
				Type: FieldTextarea, Required: true,
				Hint: "Paste the full contents of the downloaded service-account key file (.json).",
			},
			{
				Key: "team_drive", Label: "Shared Drive ID", Type: FieldText, Required: true,
				Hint: "Required for service accounts — they have no My Drive quota. Create a Shared Drive in Google Drive, share it with the service account email (client_email in the JSON), then paste the Shared Drive ID here (find it in the URL: /drive/folders/<ID>).",
			},
		},
	},
	{
		ID: BackendSFTP, Label: "SFTP — untested",
		PathHint:  "Absolute path on the server — e.g. /srv/backups/clients",
		PathLabel: "Remote path",
		Fields: []RemoteField{
			{Key: "host", Label: "Hostname / IP", Type: FieldText, Required: true},
			{Key: "user", Label: "Username", Type: FieldText, Required: true},
			{Key: "port", Label: "Port", Type: FieldText, Default: "22"},
			{Key: "pass", Label: "Password", Type: FieldPassword, Obscure: true, Hint: "Leave blank when authenticating with a key file."},
			{Key: "key_file", Label: "Private Key Path", Type: FieldText, Hint: "Absolute path to the SSH private key on the server. Leave blank when using password auth."},
		},
	},
	{
		ID: BackendWebDAV, Label: "WebDAV — untested",
		PathHint:  "Path on the WebDAV server — e.g. backups/clients",
		PathLabel: "Remote path",
		Fields: []RemoteField{
			{Key: "url", Label: "URL", Type: FieldText, Required: true, Hint: "e.g. https://cloud.example.com/remote.php/dav/files/user"},
			{
				Key: "vendor", Label: "Vendor", Type: FieldSelect, Required: true,
				Options: []SelectOpt{
					{Value: "nextcloud", Label: "Nextcloud"},
					{Value: "owncloud", Label: "ownCloud"},
					{Value: "sharepoint", Label: "SharePoint Online"},
					{Value: "other", Label: "Other"},
				},
			},
			{Key: "user", Label: "Username", Type: FieldText, Required: true},
			{Key: "pass", Label: "Password", Type: FieldPassword, Required: true, Obscure: true},
		},
	},
	{
		ID: BackendAzure, Label: "Azure Blob Storage — untested",
		PathHint:  "Container and optional prefix — e.g. backups or backups/clients",
		PathLabel: "Container / path",
		Fields: []RemoteField{
			{Key: "account", Label: "Storage account name", Type: FieldText, Required: true},
			{Key: "key", Label: "Storage account key", Type: FieldPassword, Required: true},
		},
	},
	{
		ID: BackendFTP, Label: "FTP — untested",
		PathHint:  "Path on the FTP server — e.g. /backups/clients",
		PathLabel: "Remote path",
		Fields: []RemoteField{
			{Key: "host", Label: "Hostname / IP", Type: FieldText, Required: true},
			{Key: "port", Label: "Port", Type: FieldText, Default: "21"},
			{Key: "user", Label: "Username", Type: FieldText},
			{Key: "pass", Label: "Password", Type: FieldPassword, Obscure: true},
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
}

// BackendLabel returns the human label for this remote's backend.
func (r Remote) BackendLabel() string {
	if b := FindBackend(r.Backend); b != nil {
		return b.Label
	}
	return string(r.Backend)
}

// RcloneSection returns the [name]\ntype=...\n… block for rclone.conf.
func (r Remote) RcloneSection() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s]\n", r.Name)

	switch r.Backend {
	case BackendS3:
		sb.WriteString("type = s3\n")
		sb.WriteString("provider = AWS\n")
		sb.WriteString("env_auth = false\n")
	case BackendS3Compat:
		sb.WriteString("type = s3\n")
		sb.WriteString("provider = Other\n")
		sb.WriteString("env_auth = false\n")
	case BackendB2:
		sb.WriteString("type = b2\n")
		// Without hard_delete, rclone only creates a hide marker; the file
		// remains stored (and billed) on B2. hard_delete permanently removes
		// the file and all its versions on deletefile.
		sb.WriteString("hard_delete = true\n")
	case BackendGDrive:
		sb.WriteString("type = drive\n")
		sb.WriteString("scope = drive\n")
	default:
		fmt.Fprintf(&sb, "type = %s\n", string(r.Backend))
	}

	for k, v := range r.Config {
		if v == "" {
			continue
		}
		if k == "service_account_credentials" {
			// Compact JSON to a single line so it fits on one config line.
			var raw json.RawMessage
			if err := json.Unmarshal([]byte(v), &raw); err == nil {
				if compact, err := json.Marshal(raw); err == nil {
					v = string(compact)
				}
			}
		}
		fmt.Fprintf(&sb, "%s = %s\n", k, strings.ReplaceAll(v, "\n", " "))
	}
	return sb.String()
}
