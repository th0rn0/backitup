package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/mode/rsync"
	"github.com/th0rn0/backitup/internal/model"
)

const rsyncTS = "20060102T150405Z"

// mkIncompleteSnapshot plants what a failed rsync run leaves: a directory with a
// zero-byte file per unreadable source file, plus the marker the client writes.
// markedAt is the marker's mtime, which is the age the lifecycle measures.
func mkIncompleteSnapshot(t *testing.T, clientDir, id string, markedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(clientDir, "snapshots", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"drone-runner.qcow2", "gitlab-runner.qcow2"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(dir, rsync.IncompleteMarker)
	body := "# backitup: this snapshot directory is NOT a usable backup.\n" +
		"run failed: rsync: exit status 23: rsync: [sender] send_files failed to open \"/source/drone-runner.qcow2\": Permission denied (13)\n"
	if err := os.WriteFile(marker, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(marker, markedAt, markedAt); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The offsite worker tars whatever List returns, so a snapshot a failed run left
// behind must never reach it — while the previous good snapshots still upload
// normally in the same pass.
func TestOffsiteSkipsIncompleteSnapshot(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	id, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 0, Enabled: true,
	})
	oldID := now.Add(-30 * 24 * time.Hour).UTC().Format(rsyncTS)
	newID := now.Add(-2 * time.Hour).UTC().Format(rsyncTS)
	badID := now.Add(-3 * time.Hour).UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, oldID, newID)
	if err := os.Symlink(newID, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	mkIncompleteSnapshot(t, dir, badID, now.Add(-3*time.Hour))

	off := &fakeOffsite{}
	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	// Exactly the two real snapshots were uploaded; the failed run's directory is
	// not a backup and must not become one on B2.
	if len(off.up) != 2 {
		t.Fatalf("uploads = %d, want 2 (the good snapshots only): %v", len(off.up), off.up)
	}
	for obj := range off.up {
		if strings.Contains(obj, badID) {
			t.Fatalf("the incomplete snapshot was uploaded as %q", obj)
		}
	}
	objs, err := st.ListOffsiteObjects(context.Background(), id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 2 {
		t.Fatalf("offsite records = %d, want 2 (nothing recorded for the failed run): %+v", len(objs), objs)
	}
	for _, o := range objs {
		if o.SnapshotID == badID {
			t.Fatalf("the failed run was recorded as offsited: %+v", o)
		}
	}
}

// A marked directory is invisible to List by design, which is exactly why it
// needs reclaiming: nothing else would ever delete it, and a half-copied 375 GB
// tree that no listing or retention rule can see is how the backup volume fills
// up with nothing to show for it.
func TestLifecycleReclaimsIncompleteSnapshot(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	_, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 0, Enabled: true,
	})
	goodID := now.Add(-2 * time.Hour).UTC().Format(rsyncTS)
	oldBadID := now.Add(-3 * time.Hour).UTC().Format(rsyncTS)
	freshBadID := now.Add(-5 * time.Minute).UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, goodID)
	if err := os.Symlink(goodID, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	mkIncompleteSnapshot(t, dir, oldBadID, now.Add(-2*time.Hour))
	mkIncompleteSnapshot(t, dir, freshBadID, now.Add(-5*time.Minute))

	logs := runOnceCapturingLog(t, Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }})

	if _, err := os.Stat(filepath.Join(dir, "snapshots", oldBadID)); err == nil {
		t.Fatalf("the marked directory older than %s should have been reclaimed", DefaultIncompleteAge)
	}
	// Marked a moment ago: possibly a second run writing into the same second's
	// directory, so this pass leaves it.
	if _, err := os.Stat(filepath.Join(dir, "snapshots", freshBadID)); err != nil {
		t.Fatalf("a just-marked directory must be left for the next pass: %v", err)
	}
	// The real snapshot and the pointer the next run hardlinks against survive.
	if _, err := os.Stat(filepath.Join(dir, "snapshots", goodID)); err != nil {
		t.Fatalf("the good snapshot must not be touched: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "snapshots", "latest")); target != goodID {
		t.Fatalf("latest -> %q, want %q", target, goodID)
	}
	// The removal is not silent, and it names the directory it removed.
	if !strings.Contains(logs, "removed incomplete snapshot "+oldBadID) {
		t.Errorf("the reclaim must be logged, got:\n%s", logs)
	}
	if strings.Contains(logs, "removed incomplete snapshot "+freshBadID) {
		t.Errorf("the fresh directory must not be logged as removed:\n%s", logs)
	}
	// A failed run is not an integrity finding on the good snapshot: the pass
	// must not read the half-copied tree as damage to the real backup.
	if strings.Contains(logs, "INTEGRITY FAIL") {
		t.Errorf("an incomplete snapshot is not an integrity failure of the backup:\n%s", logs)
	}
}

// An incomplete snapshot must not be counted as a snapshot by retention: it is
// absent from List, so the newest REAL snapshot stays protected and the pruner
// does not treat the half-copied tree as the newest thing the client has.
func TestIncompleteSnapshotIsNotCountedByRetention(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	_, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	oldGoodID := now.Add(-40 * 24 * time.Hour).UTC().Format(rsyncTS)
	newestGoodID := now.Add(-2 * time.Hour).UTC().Format(rsyncTS)
	newestID := now.Add(-30 * time.Minute).UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, oldGoodID, newestGoodID)
	if err := os.Symlink(newestGoodID, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	// The failed run's directory has the newest timestamp of all: if it counted,
	// the pruner would protect IT as "the newest" and delete the real newest one.
	// It is left too recently marked to be reclaimed in this pass, so the only
	// thing being tested is what retention does with it.
	mkIncompleteSnapshot(t, dir, newestID, now.Add(-30*time.Minute))

	if err := RunOnce(context.Background(), Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", newestGoodID)); err != nil {
		t.Fatalf("the newest REAL snapshot must be protected, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", oldGoodID)); err == nil {
		t.Fatal("the snapshot past the 14-day horizon should still be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", newestID)); err != nil {
		t.Fatalf("a recently marked directory must survive this pass: %v", err)
	}
}
