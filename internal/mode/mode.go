// Package mode defines the per-mode behaviour seam (design doc D6/Premise 2):
// rsync and tar.gz are different architectures, so each implements these
// interfaces instead of the code branching on `if mode == ...` everywhere.
//
// There are two sides:
//
//	ClientMode  runs on the dumb client: capture the source, ship it.
//	ServerMode  runs in the lifecycle worker: list, prune hot, package for offsite.
//
// Lane 0 ships the interfaces + registry + stubs. Lanes C (client) and D
// (lifecycle) fill in the rsync/targz implementations.
package mode

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// ErrNotImplemented marks a stub not yet filled in by its lane.
var ErrNotImplemented = errors.New("not implemented")

// BackupOpts is everything the client needs for one run. SourceDir is the
// read-only bind mount (never written; design doc non-destructiveness guards).
// The server-side directory is NOT here: it is baked into the forced command in
// authorized_keys, so the client can only ever write into its own directory.
type BackupOpts struct {
	SourceDir           string
	Excludes            []string
	SkipSymlinks        bool        // omit symlinks from the backup
	HasPreviousSnapshot bool        // rsync: skip --link-dest when false (first backup)
	Logger              *log.Logger // nil = use log.Default()
	SSHServer           string      // host:port of the sshd ingest (data channel)
	SSHUser             string      // ssh login user (the single ingest user, "backitup")
	SSHKey              string      // path to the client's private key
	KnownHosts          string      // path to known_hosts for host-key verification
	InsecureSSH         bool        // skip SSH host-key verification (dev/test only)
}

// Logger returns the configured logger, falling back to the default logger.
func (o BackupOpts) Log() *log.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return log.Default()
}

// HumanBytes formats a byte count as a human-readable string (e.g. "1.2 MB").
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// BackupResult is what the client reports back via POST /api/v1/status.
type BackupResult struct {
	SnapshotID string
	Bytes      int64
	Files      int64
	StartedAt  time.Time
	FinishedAt time.Time
}

// ClientMode is the client-side capture+ship behaviour for one Mode.
type ClientMode interface {
	Mode() model.Mode
	// Backup captures SourceDir and ships it to the server. It MUST NOT write to
	// SourceDir (read-only invariant). On rsync, it flips `latest` only on success.
	Backup(ctx context.Context, o BackupOpts) (BackupResult, error)
}

// Snapshot is one stored point-in-time on the hot store.
type Snapshot struct {
	ID        string
	CreatedAt time.Time
	Bytes     int64
}

// ServerMode is the lifecycle-side behaviour for one Mode, operating on a
// client's directory on the hot store.
type ServerMode interface {
	Mode() model.Mode
	// List returns the snapshots currently on the hot store for a client dir.
	List(ctx context.Context, clientDir string) ([]Snapshot, error)
	// PrepareOffsite returns a path to an immutable object to upload for the given
	// snapshot (D8): tar.gz -> the archive itself; rsync -> a tar of the snapshot
	// dir created once. Never an rclone sync of a hardlink tree. For modes that
	// build a temp object (rsync), the caller removes it after upload.
	PrepareOffsite(ctx context.Context, clientDir string, snap Snapshot) (objectPath string, err error)
	// DeleteSnapshot removes one snapshot from the hot store. Retention POLICY
	// (age, offsite-first, protect-newest) lives in the lifecycle worker, which
	// decides which snapshots are safe to delete and calls this per snapshot.
	DeleteSnapshot(ctx context.Context, clientDir, id string) error
}

// registry wires modes by name so the rest of the code stays mode-agnostic.
var (
	clientModes = map[model.Mode]ClientMode{}
	serverModes = map[model.Mode]ServerMode{}
)

// RegisterClient / RegisterServer are called from each mode implementation's init.
func RegisterClient(m ClientMode) { clientModes[m.Mode()] = m }
func RegisterServer(m ServerMode) { serverModes[m.Mode()] = m }

// Client / Server look up a mode implementation; ok is false for an unknown or
// not-yet-registered mode.
func Client(m model.Mode) (ClientMode, bool) { c, ok := clientModes[m]; return c, ok }
func Server(m model.Mode) (ServerMode, bool) { s, ok := serverModes[m]; return s, ok }
