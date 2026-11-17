package rsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/th0rn0/backitup/internal/archiveutil"
	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// Server is the lifecycle-side behaviour for rsync: snapshots live as
// hardlinked directories under <clientDir>/snapshots/<ts>, with a `latest`
// symlink. Offsite turns each snapshot into one immutable tar.gz (D8 — rclone
// can't preserve hardlinks, so we package per-snapshot).
type Server struct{}

func (Server) Mode() model.Mode { return model.ModeRsync }

func init() { mode.RegisterServer(Server{}) }

func snapshotsDir(clientDir string) string { return filepath.Join(clientDir, "snapshots") }

// List returns snapshot directories (excluding the `latest` symlink).
func (Server) List(ctx context.Context, clientDir string) ([]mode.Snapshot, error) {
	dir := snapshotsDir(clientDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []mode.Snapshot
	for _, e := range entries {
		if e.Name() == "latest" || !e.IsDir() {
			continue // skip the latest symlink and any stray files
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		// rsync -a preserves source mtime, so info.ModTime() reflects the
		// source directory age, not when the snapshot was taken. Parse the
		// timestamp from the snapshot ID instead; fall back to ModTime only
		// for entries that don't match the expected format.
		createdAt, err := time.Parse("20060102T150405Z", e.Name())
		if err != nil {
			createdAt = info.ModTime()
		}
		// Do not walk the snapshot directory to sum sizes — rsync snapshots use
		// hardlinks and can be very large; a full WalkDir blocks the caller.
		// Size is reported as 0 (displayed as "—" in the UI).
		out = append(out, mode.Snapshot{ID: e.Name(), CreatedAt: createdAt, Bytes: 0})
	}
	return out, nil
}

// StagingDirEnv names the directory PrepareOffsite builds its temporary
// archives in. Unset means <backup base dir>/staging — a sibling of the client
// directories, on the volume the operator sized for backups. The default used to
// be os.CreateTemp(""), i.e. the system temp dir: a copy of every snapshot on a
// tmpfs that was never sized for it (and, for a big client, a full disk that
// fails the backup rather than the staging step).
const StagingDirEnv = "BACKITUP_STAGING_DIR"

// stagingDirFor resolves (and creates) the directory to stage an archive in.
func stagingDirFor(clientDir string) (string, error) {
	dir := os.Getenv(StagingDirEnv)
	if dir == "" {
		dir = filepath.Join(filepath.Dir(clientDir), "staging")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create offsite staging dir: %w", err)
	}
	return dir, nil
}

// PrepareOffsite tars the snapshot directory into a temp .tar.gz (hardlinks
// resolved into normal files). The caller uploads it then removes it.
func (Server) PrepareOffsite(ctx context.Context, clientDir string, snap mode.Snapshot) (string, error) {
	snapPath := filepath.Join(snapshotsDir(clientDir), snap.ID)
	stage, err := stagingDirFor(clientDir)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(stage, "backitup-offsite-*.tar.gz")
	if err != nil {
		return "", err
	}
	if _, _, err := archiveutil.TarGz(ctx, tmp, snapPath, nil, false, nil); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return "", err
	}
	return tmp.Name(), nil
}

// DeleteSnapshot removes one snapshot dir, refusing the one `latest` points at
// (deleting it would break the next --link-dest base). The lifecycle also
// protects the newest snapshot, but this is a belt-and-braces guard.
func (Server) DeleteSnapshot(ctx context.Context, clientDir, id string) error {
	dir := snapshotsDir(clientDir)
	if keep, _ := os.Readlink(filepath.Join(dir, "latest")); keep == id {
		return fmt.Errorf("refusing to delete snapshot %q: it is the current `latest`", id)
	}
	return os.RemoveAll(filepath.Join(dir, id))
}

