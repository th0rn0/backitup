package mode

import (
	"testing"
	"time"
)

// SortNewestFirst is the single definition of "which snapshot is newest" that
// both modes' List and the lifecycle's pruner use. It must be a total order: two
// entries carrying the same timestamp (the mtime fallback produces exactly that)
// must not be ordered by whatever the filesystem happened to return first.
func TestSortNewestFirst(t *testing.T) {
	ts := time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC)
	snaps := []Snapshot{
		{ID: "a", CreatedAt: ts.Add(-48 * time.Hour)}, // order in the slice is
		{ID: "b", CreatedAt: ts.Add(-24 * time.Hour)}, // deliberately not the
		{ID: "c", CreatedAt: ts},                      // order we expect back
		{ID: "y", CreatedAt: ts.Add(-24 * time.Hour)},
	}
	SortNewestFirst(snaps)

	want := []string{"c", "y", "b", "a"} // newest first; equal timestamps by id, descending
	for i, id := range want {
		if snaps[i].ID != id {
			t.Fatalf("position %d = %q, want %q (full: %+v)", i, snaps[i].ID, id, snaps)
		}
	}

	// The same input in a different order must produce the same output: the
	// result is a property of the set, not of the slice.
	reversed := []Snapshot{snaps[3], snaps[2], snaps[1], snaps[0]}
	SortNewestFirst(reversed)
	for i, id := range want {
		if reversed[i].ID != id {
			t.Fatalf("reversed input: position %d = %q, want %q", i, reversed[i].ID, id)
		}
	}
}
