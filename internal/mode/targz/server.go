package targz

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/archiveutil"
	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// snapshotIDTimeLayout is the timestamp every snapshot ID starts with, in both
// modes: `backup-<ts>-<seq>.tar.gz` here, `<ts>` for rsync.
const snapshotIDTimeLayout = "20060102T150405Z"

// targzIDTime matches the archive names docker/sshd/backitup-recv writes and
// captures the timestamp it stamps into them.
var targzIDTime = regexp.MustCompile(`^backup-([0-9]{8}T[0-9]{6}Z)-[0-9]+\.tar\.gz$`)

// Server is the lifecycle-side behaviour for tar.gz: each archive in the client
// dir is one immutable snapshot.
type Server struct{}

func (Server) Mode() model.Mode { return model.ModeTarGz }

func init() { mode.RegisterServer(Server{}) }

// List returns the .tar.gz archives directly under clientDir, newest first.
//
// CreatedAt is parsed from the timestamp in the archive's own name — the same
// source rsync uses — and NOT from the file's mtime. mtime only says when the
// bytes last landed on this filesystem: a cp, a volume copy, a restore, a
// file-level sync or an archive extraction rewrites it, so an old snapshot that
// has been through any of those looks new forever and retention never fires on
// it. backitup-recv stamps the name on the server's clock as it writes the
// upload, so the name is the age the snapshot actually has.
//
// mtime is the fallback for an archive whose name is not the shape recv writes
// (a hand-placed file, an older layout): there is nothing else to go on there,
// and it is the only case in which the filesystem gets a vote on age.
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
		createdAt := info.ModTime() // fallback: name is not the shape recv writes
		if m := targzIDTime.FindStringSubmatch(e.Name()); m != nil {
			if ts, perr := time.Parse(snapshotIDTimeLayout, m[1]); perr == nil {
				createdAt = ts
			}
		}
		out = append(out, mode.Snapshot{ID: e.Name(), CreatedAt: createdAt, Bytes: info.Size()})
	}
	// Newest first: os.ReadDir is filename-ascending, i.e. oldest first. Callers
	// treat snaps[0] as the newest (prune protection, verifyLatest), so this
	// ordering is part of the contract, not a display detail.
	mode.SortNewestFirst(out)
	return out, nil
}

// PrepareOffsite returns the archive itself — it is already an immutable object
// (D8). Nothing to build, nothing to clean up.
func (Server) PrepareOffsite(ctx context.Context, clientDir string, snap mode.Snapshot) (string, error) {
	return filepath.Join(clientDir, snap.ID), nil
}

// VerifySnapshot reads the whole archive so gzip/CRC damage inside it surfaces
// here rather than at restore time (D9). Bytes came from the file size at List
// time, so a size test would only prove the file was there then — the archive is
// opened and read.
func (Server) VerifySnapshot(ctx context.Context, clientDir string, snap mode.Snapshot) error {
	return archiveutil.VerifyGzip(filepath.Join(clientDir, snap.ID))
}

// DeleteSnapshot removes one archive from the hot store.
func (Server) DeleteSnapshot(ctx context.Context, clientDir, id string) error {
	if err := mode.ValidateSnapshotID(model.ModeTarGz, id); err != nil {
		return err
	}
	path, err := mode.SnapshotPath(clientDir, id)
	if err != nil {
		return err
	}
	return os.Remove(path)
}
