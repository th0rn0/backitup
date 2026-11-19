package rsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// List must hand snapshots back newest first. It does not get that from the
// filesystem: os.ReadDir sorts by filename, and `<ts>` names sort oldest first,
// so an unsorted listing puts the OLDEST snapshot at index 0 — which is the one
// the hot pruner protects from deletion.
//
// The directory mtimes deliberately contradict the ids (the oldest snapshot was
// touched last), because `rsync -a` preserves source mtimes and the directory's
// own mtime therefore says nothing about when the snapshot was taken.
func TestServerListIsNewestFirst(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	ids := []string{
		"20260612T000000Z", // oldest
		"20260615T000000Z",
		"20260618T000000Z", // newest
	}
	for _, id := range ids {
		makeSnap(t, dir, id, id)
	}
	if err := os.Chtimes(filepath.Join(dir, "snapshots", "20260612T000000Z"), now, now); err != nil {
		t.Fatalf("chtimes oldest: %v", err)
	}
	if err := os.Chtimes(filepath.Join(dir, "snapshots", "20260618T000000Z"), now.Add(-96*time.Hour), now.Add(-96*time.Hour)); err != nil {
		t.Fatalf("chtimes newest: %v", err)
	}

	snaps, err := Server{}.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != len(ids) {
		t.Fatalf("got %d snapshots, want %d: %+v", len(snaps), len(ids), snaps)
	}
	for i, want := range []string{ids[2], ids[1], ids[0]} {
		if snaps[i].ID != want {
			t.Fatalf("position %d = %q, want %q (newest first) — full listing: %+v", i, snaps[i].ID, want, snaps)
		}
	}
	if got, want := snaps[0].CreatedAt, time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("newest CreatedAt = %s, want %s (parsed from the id, not the directory mtime)", got, want)
	}
}
