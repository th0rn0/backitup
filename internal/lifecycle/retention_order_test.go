package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// The two ends of the same defect, exercised through a real lifecycle pass.
//
// os.ReadDir returns a client directory (or snapshots/<...>) in filename order,
// and both modes' snapshot IDs begin with the timestamp, so an unsorted listing
// is OLDEST first. Index 0 is then the oldest snapshot, and a pruner that reads
// "position 0" as "the newest" protects the wrong one: it keeps a six-day-old
// snapshot because the file happens to sort first and deletes the one taken an
// hour ago.
//
// The two cases below are deliberately contradictory about mtime, because that
// is the second half of the defect: for tar.gz, CreatedAt used to BE the mtime,
// so the age that decided deletion was metadata that a cp, a volume copy, a
// restore or an extraction rewrites. An old archive copied onto the volume looks
// new forever (retention never fires); a new archive whose mtime was preserved
// from the source looks ancient (retention fires on it immediately).
//
//	ID says new, mtime says old  -> must survive
//	ID says old, mtime says new  -> must be pruned
func TestHotPruneAgesTarGzBySnapshotIDNotMtime(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, RetentionDays: 1, Enabled: true,
	})

	// 12 h old by its own timestamp, but its bytes were written six days ago —
	// the shape a restore or a `cp -a` leaves behind.
	newest := filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz")
	mkArchiveContent(t, newest, now.Add(-6*24*time.Hour), "newest-by-id")
	// Six days old by its own timestamp, but its bytes landed a moment ago —
	// the shape every copy, resync or extraction leaves behind.
	stale := filepath.Join(dir, "backup-20260612T000000Z-000.tar.gz")
	mkArchiveContent(t, stale, now.Add(-1*time.Minute), "old-by-id")

	_ = RunOnce(context.Background(), Deps{
		Store: st, BackupBaseDir: base, Now: func() time.Time { return now },
	})

	if _, err := os.Stat(newest); err != nil {
		t.Errorf("the newest snapshot (id 20260618, 12h old by its own timestamp) was deleted: %v\n"+
			"hot retention must never delete the newest snapshot", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("the six-day-old snapshot (id 20260612) survived because its mtime is fresh\n" +
			"tar.gz snapshots must be aged by the timestamp in their id, not by mtime")
	}
}

// The same two directions in rsync mode. rsync already parses its `<ts>`
// directory name, so this is the regression guard for the OTHER half of the
// contract: both listings must answer "which is newest" from the snapshot's own
// timestamp, and the shared ordering must not be re-derived from directory
// order anywhere on the way to the pruner.
func TestHotPruneAgesRsyncBySnapshotIDNotMtime(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, RetentionDays: 1, Enabled: true,
	})

	newest := filepath.Join(dir, "snapshots", "20260618T000000Z")
	stale := filepath.Join(dir, "snapshots", "20260612T000000Z")
	mkRsyncSnapshots(t, dir, "20260618T000000Z", "20260612T000000Z")
	// rsync -a preserves source mtimes, so a snapshot directory's own mtime says
	// nothing about when the snapshot was taken — here it says the opposite of
	// the truth in both directions.
	if err := os.Chtimes(newest, now.Add(-6*24*time.Hour), now.Add(-6*24*time.Hour)); err != nil {
		t.Fatalf("chtimes newest: %v", err)
	}
	if err := os.Chtimes(stale, now.Add(-1*time.Minute), now.Add(-1*time.Minute)); err != nil {
		t.Fatalf("chtimes stale: %v", err)
	}

	_ = RunOnce(context.Background(), Deps{
		Store: st, BackupBaseDir: base, Now: func() time.Time { return now },
	})

	if _, err := os.Stat(newest); err != nil {
		t.Errorf("the newest rsync snapshot (id 20260618) was deleted: %v", err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("the six-day-old rsync snapshot (id 20260612) survived")
	}
}

// pruneHot's "never the newest" guard must be a property of the snapshots it is
// given, not of the order they arrive in. Handed an OLDEST-first slice — what
// every mode's List produced before the ordering was fixed at the source — it
// must still keep the newest and delete the rest.
//
// The snapshots here are all past the horizon, so "protect the newest" is the
// only rule that can save any of them: a pruner that protects index 0 keeps the
// four-day-old archive and deletes the two-day-old one.
func TestPruneHotProtectsNewestWhateverTheSliceOrder(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()

	newestID := "backup-20260616T000000Z-000.tar.gz" // 2 days old
	midID := "backup-20260615T000000Z-000.tar.gz"    // 3 days old
	oldestID := "backup-20260614T000000Z-000.tar.gz" // 4 days old
	for _, id := range []string{newestID, midID, oldestID} {
		p := filepath.Join(dir, id)
		if err := os.WriteFile(p, []byte(id), 0o644); err != nil {
			t.Fatalf("write %s: %v", id, err)
		}
	}

	snaps := []mode.Snapshot{
		{ID: oldestID, CreatedAt: now.Add(-4 * 24 * time.Hour)},
		{ID: midID, CreatedAt: now.Add(-3 * 24 * time.Hour)},
		{ID: newestID, CreatedAt: now.Add(-2 * 24 * time.Hour)},
	}

	d := Deps{Now: func() time.Time { return now }}
	deleted, failed, err := pruneHot(context.Background(), d,
		model.Client{ID: 1, Name: "c", Mode: model.ModeTarGz, RetentionDays: 1},
		mode.ServerMode(mustServerMode(t, model.ModeTarGz)), dir, snaps, false, nil)
	if err != nil {
		t.Fatalf("pruneHot: %v (deleted=%d failed=%d)", err, deleted, failed)
	}
	if deleted != 2 || failed != 0 {
		t.Fatalf("pruneHot deleted=%d failed=%d, want 2 and 0: the oldest-first slice must still yield the two older snapshots", deleted, failed)
	}
	if _, err := os.Stat(filepath.Join(dir, newestID)); err != nil {
		t.Fatalf("the newest snapshot was deleted from an oldest-first slice: %v", err)
	}
	for _, id := range []string{midID, oldestID} {
		if _, err := os.Stat(filepath.Join(dir, id)); err == nil {
			t.Fatalf("snapshot %s survived a prune that should have removed it", id)
		}
	}
}

// newestSnapshot decides from the set, with the ID breaking ties so that the
// answer does not depend on iteration order.
func TestNewestSnapshot(t *testing.T) {
	ts := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	if _, ok := newestSnapshot(nil); ok {
		t.Fatal("newestSnapshot(nil) reported a snapshot")
	}
	// Equal timestamps are what a mode's mtime fallback produces for entries
	// written in the same instant; the ID decides, and it decides the same way
	// whatever order the entries arrive in.
	a := mode.Snapshot{ID: "backup-20260618T000000Z-001.tar.gz", CreatedAt: ts}
	b := mode.Snapshot{ID: "backup-20260618T000000Z-002.tar.gz", CreatedAt: ts}
	for _, snaps := range [][]mode.Snapshot{{a, b}, {b, a}} {
		got, ok := newestSnapshot(snaps)
		if !ok || got.ID != b.ID {
			t.Fatalf("newestSnapshot(%v) = %q, want %q", snaps, got.ID, b.ID)
		}
	}
}

func mustServerMode(t *testing.T, m model.Mode) mode.ServerMode {
	t.Helper()
	sm, ok := mode.Server(m)
	if !ok {
		t.Fatalf("no server mode registered for %q", m)
	}
	return sm
}
