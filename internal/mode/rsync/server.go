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

// List returns snapshot directories (excluding the `latest` symlink), newest
// first. os.ReadDir hands entries back in filename order, which for `<ts>` names
// is oldest first, so the ordering Callers rely on (snaps[0] is the newest) has
// to be applied here rather than assumed.
//
// A directory that carries the incomplete marker is NOT returned: a failed run
// leaves one, and every consumer of this listing (the offsite worker, retention,
// the dashboard) would otherwise treat a half-copied tree as a backup. Their
// absence from this listing is the whole mechanism; see incomplete.go.
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
		// A directory a failed run left behind is not a snapshot. It holds
		// whatever rsync had copied when it stopped (four zero-byte files, in
		// the case that produced this), so handing it to the offsite worker
		// uploads an empty "backup" — and a listing that says the client is
		// covered is worse than no listing at all. See incomplete.go.
		if _, incomplete := incompleteReason(filepath.Join(dir, e.Name())); incomplete {
			continue
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
	mode.SortNewestFirst(out)
	return out, nil
}

// ListIncomplete returns the snapshot directories a failed run left behind —
// exactly the ones List refuses to hand out — so the lifecycle can reclaim them
// instead of leaving a half-copied tree on the backup volume forever.
//
// CreatedAt is the marker's mtime: when the failure was recorded. That is the
// age the caller should measure a grace period against, not the directory's own
// mtime (rsync -a preserves the source's) and not the id (a run's start).
func (Server) ListIncomplete(ctx context.Context, clientDir string) ([]mode.Snapshot, error) {
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
			continue
		}
		snapPath := filepath.Join(dir, e.Name())
		if _, incomplete := incompleteReason(snapPath); !incomplete {
			continue
		}
		at, ok := incompleteSince(snapPath)
		if !ok {
			at = time.Time{}
		}
		out = append(out, mode.Snapshot{ID: e.Name(), CreatedAt: at, Bytes: 0})
	}
	mode.SortNewestFirst(out)
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

// VerifySnapshot samples the snapshot on disk: the directory must still be
// there and must hold at least one entry, and `latest` must still resolve.
//
// Bytes is 0 for rsync by design (see List), so a size test says nothing about
// this mode — the lifecycle's old `latest.Bytes == 0` check therefore warned on
// every pass for every rsync client while nothing was wrong. The cheap check that
// does mean something for a directory tree is its shape: one ReadDir of the
// snapshot's top level, never a WalkDir of the tree (which is what List rightly
// refuses to do — a hardlinked tree can be huge and this runs on every pass).
func (Server) VerifySnapshot(ctx context.Context, clientDir string, snap mode.Snapshot) error {
	snapDir := filepath.Join(snapshotsDir(clientDir), snap.ID)
	// The directory a failed run left behind is not a snapshot, and naming that
	// is more use than the shape test below — "present, at least one entry" —
	// which four zero-byte files satisfy.
	if reason, incomplete := incompleteReason(snapDir); incomplete {
		return fmt.Errorf("snapshot %s is incomplete: %s", snapDir, reason)
	}
	entries, err := os.ReadDir(snapDir)
	switch {
	case os.IsNotExist(err):
		return fmt.Errorf("snapshot directory %s is missing", snapDir)
	case err != nil:
		return fmt.Errorf("read snapshot directory %s: %w", snapDir, err)
	case len(entries) == 0:
		return fmt.Errorf("snapshot directory %s is empty", snapDir)
	}
	if err := sampleHasContent(snapDir, entries); err != nil {
		return err
	}
	// `latest` is the base the client's next run hardlinks against
	// (--link-dest=/snapshots/latest). A dangling one does not fail that run —
	// rsync simply has no base and copies everything again — so nothing else
	// would tell the operator that hardlinking has silently stopped.
	if _, err := os.Stat(filepath.Join(snapshotsDir(clientDir), "latest")); err != nil {
		return fmt.Errorf("`latest` does not point at an existing snapshot (%v): the next run has no hardlink base", err)
	}
	return nil
}

// zeroByteSample bounds how many entries sampleHasContent stats. A snapshot with
// more top-level regular files than this is not judged: the check exists to
// recognise the total-failure shape, and a real snapshot of thousands of files
// must never be held responsible for one stat per file on a check that runs
// every pass.
const zeroByteSample = 64

// sampleHasContent rejects the shape a wholly failed transfer leaves: regular
// files, and every one of them 0 bytes.
//
// It is a sample by construction, and deliberately biased towards silence:
//
//   - A snapshot whose top level holds any directory is not judged at all. The
//     content may be inside it, and finding out would be a WalkDir, which this
//     mode's checks must never do (a hardlinked tree can be very large).
//   - More than zeroByteSample top-level files, or any file that cannot be
//     stat'ed: not judged.
//   - An empty directory is the "is empty" case above, not this one. A source
//     directory that really is empty produces one, and it stays a legitimate
//     snapshot — this check must not turn "the source was empty" into a failure.
//
// So a snapshot is only called contentless when everything visible at its top
// level is a zero-byte regular file, which no real backup of a non-empty source
// looks like. The reported case — four root-owned .qcow2 files rsync could not
// read — is exactly that shape.
func sampleHasContent(snapDir string, entries []os.DirEntry) error {
	files := 0
	for _, e := range entries {
		if e.IsDir() {
			return nil // content may be below this; judging it costs a WalkDir
		}
		if !e.Type().IsRegular() {
			continue // symlinks, sockets, devices: not content we can size
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if info.Size() > 0 {
			return nil // something has content: this is a snapshot with data
		}
		files++
		if files > zeroByteSample {
			return nil
		}
	}
	if files == 0 {
		return nil // nothing to judge (the empty case was handled above)
	}
	return fmt.Errorf("snapshot %s holds %d file(s) and every one is 0 bytes: "+
		"the shape a transfer leaves when nothing could be copied", snapDir, files)
}

// DeleteSnapshot removes one snapshot dir, refusing the one `latest` points at
// (deleting it would break the next --link-dest base). The lifecycle also
// protects the newest snapshot, but this is a belt-and-braces guard.
func (Server) DeleteSnapshot(ctx context.Context, clientDir, id string) error {
	if err := mode.ValidateSnapshotID(model.ModeRsync, id); err != nil {
		return err
	}
	dir := snapshotsDir(clientDir)
	path, err := mode.SnapshotPath(dir, id)
	if err != nil {
		return err
	}
	if keep, _ := os.Readlink(filepath.Join(dir, "latest")); keep == id {
		return fmt.Errorf("refusing to delete snapshot %q: it is the current `latest`", id)
	}
	return os.RemoveAll(path)
}
