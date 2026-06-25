package rsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
		// Do not walk the snapshot directory to sum sizes — rsync snapshots use
		// hardlinks and can be very large; a full WalkDir blocks the caller.
		// Size is reported as 0 (displayed as "—" in the UI).
		out = append(out, mode.Snapshot{ID: e.Name(), CreatedAt: info.ModTime(), Bytes: 0})
	}
	return out, nil
}

// PrepareOffsite tars the snapshot directory into a temp .tar.gz (hardlinks
// resolved into normal files). The caller uploads it then removes the temp file.
func (Server) PrepareOffsite(ctx context.Context, clientDir string, snap mode.Snapshot) (string, error) {
	snapPath := filepath.Join(snapshotsDir(clientDir), snap.ID)
	tmp, err := os.CreateTemp("", "backitup-offsite-*.tar.gz")
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

