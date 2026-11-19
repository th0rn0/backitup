package mode

import "sort"

// SortNewestFirst orders snapshots newest-first by the timestamp each snapshot
// itself carries, tie-breaking on the ID so the result is total and does not
// depend on the order the filesystem happened to hand the entries over.
//
// This is the only ordering in the codebase on purpose: callers that treat
// snaps[0] as "the newest" (the hot pruner's protect guard, verifyLatest, the
// offsite uploader) are only as correct as the listing they were given.
// os.ReadDir returns entries sorted by filename ascending — which, because both
// modes' snapshot IDs begin with the timestamp, is *oldest* first. A listing
// that is not sorted through here is sorted backwards, and the bug that causes
// (protecting the oldest snapshot and deleting the newest) is silent until the
// day it deletes the only backup anyone wanted.
func SortNewestFirst(snaps []Snapshot) {
	sort.Slice(snaps, func(i, j int) bool {
		if !snaps[i].CreatedAt.Equal(snaps[j].CreatedAt) {
			return snaps[i].CreatedAt.After(snaps[j].CreatedAt)
		}
		return snaps[i].ID > snaps[j].ID
	})
}
