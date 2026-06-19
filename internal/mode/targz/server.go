package targz

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// Server is the lifecycle-side behaviour for tar.gz: each archive in the client
// dir is one immutable snapshot.
type Server struct{}

func (Server) Mode() model.Mode { return model.ModeTarGz }

func init() { mode.RegisterServer(Server{}) }

// List returns the .tar.gz archives directly under clientDir. CreatedAt uses
// file mtime (decoupled from the filename format).
func (Server) List(ctx context.Context, clientDir string) ([]mode.Snapshot, error) {
	entries, err := os.ReadDir(clientDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []mode.Snapshot
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		out = append(out, mode.Snapshot{ID: e.Name(), CreatedAt: info.ModTime(), Bytes: info.Size()})
	}
	return out, nil
}

// PrepareOffsite returns the archive itself — it is already an immutable object
// (D8). Nothing to build, nothing to clean up.
func (Server) PrepareOffsite(ctx context.Context, clientDir string, snap mode.Snapshot) (string, error) {
	return filepath.Join(clientDir, snap.ID), nil
}

// DeleteSnapshot removes one archive from the hot store.
func (Server) DeleteSnapshot(ctx context.Context, clientDir, id string) error {
	return os.Remove(filepath.Join(clientDir, id))
}
