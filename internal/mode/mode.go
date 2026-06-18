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
	SourceDir  string
	Excludes   []string
	SSHServer  string // host:port of the sshd ingest (data channel)
	SSHUser    string // ssh login user (the single ingest user, "backitup")
	SSHKey     string // path to the client's private key
	KnownHosts string // path to known_hosts for host-key verification
	Insecure   bool   // skip host-key verification (dev/test only)
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
	// dir created once. Never an rclone sync of a hardlink tree.
	PrepareOffsite(ctx context.Context, clientDir string, snap Snapshot) (objectPath string, err error)
	// PruneHot deletes snapshots older than retentionDays from the hot store.
	// Callers MUST ensure a snapshot is offsited before pruning it (offsite-first).
	PruneHot(ctx context.Context, clientDir string, retentionDays int) (pruned []string, err error)
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
