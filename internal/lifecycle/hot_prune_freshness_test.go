package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// The offsite guard on hot pruning is a freshness rule, not a per-snapshot
// record: a hot snapshot may be deleted when a STRICTLY NEWER snapshot is
// already confirmed offsite. Deleting an old copy is safe because a newer copy
// exists somewhere; anything at or newer than the newest offsite copy is kept,
// because nothing newer proves that content survives off the hot store.
//
// The tests below pin the rule at pruneHot, which is where the decision is made,
// and TestRunOnceOffsiteFirstRequiresANewerOffsiteCopy pins the same thing
// through a real pass (where offsiteOn and the offsited set are computed).

// pruneSnap is one snapshot as the tests set it up: an id, how old it is, and
// whether this client's offsite state claims it is already off the hot store.
type pruneSnap struct {
	id      string
	age     time.Duration
	offsite bool
}

// writePruneSnaps writes one file per snapshot into dir and returns the listing
// newest-first (what mode.SortNewestFirst hands pruneHot) plus the offsited set.
func writePruneSnaps(t *testing.T, dir string, now time.Time, specs []pruneSnap) ([]mode.Snapshot, map[string]bool) {
	t.Helper()
	snaps := make([]mode.Snapshot, 0, len(specs))
	offsited := make(map[string]bool, len(specs))
	for _, sp := range specs {
		if err := os.WriteFile(filepath.Join(dir, sp.id), []byte(sp.id), 0o644); err != nil {
			t.Fatalf("write %s: %v", sp.id, err)
		}
		snaps = append(snaps, mode.Snapshot{ID: sp.id, CreatedAt: now.Add(-sp.age)})
		if sp.offsite {
			offsited[sp.id] = true
		}
	}
	mode.SortNewestFirst(snaps)
	return snaps, offsited
}

// pruneHotIn runs one pruneHot call against dir for a tar.gz client with the
// given horizon, and returns what it deleted plus everything it logged.
func pruneHotIn(t *testing.T, dir string, now time.Time, specs []pruneSnap, retentionDays int, offsiteOn bool) (deleted int, logs string) {
	t.Helper()
	snaps, offsited := writePruneSnaps(t, dir, now, specs)
	logsBuf := captureLog(t)
	deleted, failed, err := pruneHot(context.Background(),
		Deps{Now: func() time.Time { return now }},
		model.Client{ID: 7, Name: "c", Mode: model.ModeTarGz, RetentionDays: retentionDays},
		mustServerMode(t, model.ModeTarGz), dir, snaps, offsiteOn, offsited)
	if err != nil || failed != 0 {
		t.Fatalf("pruneHot: err=%v failed=%d", err, failed)
	}
	return deleted, logsBuf.String()
}

// remaining reports which of the given snapshot ids are still on disk.
func remaining(t *testing.T, dir string, ids []string) map[string]bool {
	t.Helper()
	present := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(dir, id)); err == nil {
			present[id] = true
		}
	}
	return present
}

// Healthy uploads (criterion 1): the newest offsite copy sits in the middle of
// the listing. Everything strictly older than it goes; it and everything newer
// stays — including the newest snapshot, which has no offsite record of its own
// and is kept by the freshness rule rather than by its own row.
func TestHotPruneDeletesOnlyWhatIsOlderThanTheNewestOffsiteCopy(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	const (
		oldest    = "backup-20260509T000000Z-000.tar.gz" // 40d, offsite
		older     = "backup-20260519T000000Z-000.tar.gz" // 30d, offsite
		newestOff = "backup-20260613T000000Z-000.tar.gz" // 5d, offsite: the marker
		fresh     = "backup-20260618T110000Z-000.tar.gz" // 1h, no offsite record, newest
	)
	deleted, logs := pruneHotIn(t, dir, now, []pruneSnap{
		{oldest, 40 * 24 * time.Hour, true},
		{older, 30 * 24 * time.Hour, true},
		{newestOff, 5 * 24 * time.Hour, true},
		{fresh, time.Hour, false},
	}, 1, true)

	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (the two snapshots older than the newest offsite copy):\n%s", deleted, logs)
	}
	left := remaining(t, dir, []string{oldest, older, newestOff, fresh})
	if left[oldest] || left[older] {
		t.Errorf("snapshots older than the newest offsite copy survived: %v\n%s", left, logs)
	}
	if !left[newestOff] {
		t.Errorf("the newest offsite snapshot itself was pruned; the rule keeps anything at or newer than it:\n%s", logs)
	}
	if !left[fresh] {
		t.Errorf("a snapshot newer than the newest offsite copy was pruned:\n%s", logs)
	}
	// "Why was this pruned" and "why was that kept" both have to be in the log.
	if want := "pruning " + oldest + ": older than the newest offsite copy " + newestOff; !strings.Contains(logs, want) {
		t.Errorf("the log must name the newest offsite copy as the reason for a delete;\nwant a line containing: %s\ngot:\n%s", want, logs)
	}
	// The marker itself is past the horizon and is held back by the freshness
	// rule alone — the one held-back snapshot this pass has, and the line names it.
	// (fresh is inside the horizon, so the age rule already explains it and it is
	// not in this line.)
	if want := "keeping " + newestOff + ": not older than the newest offsite copy " + newestOff; !strings.Contains(logs, want) {
		t.Errorf("the log must explain what was held back and against which copy;\nwant a line containing: %s\ngot:\n%s", want, logs)
	}
	if n := strings.Count(logs, ": not older than the newest offsite copy "); n != 1 {
		t.Errorf("the hold-back line was printed %d times, want once per pass:\n%s", n, logs)
	}
}

// Stale offsite (criterion 2): uploads have been failing and the newest offsite
// copy is four snapshots behind. Nothing at or after it may be pruned — the four
// snapshots after it are the only copies of that data — and the pass must say
// which copy it is holding them against, in one line.
func TestHotPruneKeepsEverythingWhenTheNewestOffsiteCopyIsStale(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	const (
		marker = "backup-20260509T000000Z-000.tar.gz" // 40d, the last good offsite copy
		third  = "backup-20260514T000000Z-000.tar.gz" // 35d
		second = "backup-20260519T000000Z-000.tar.gz" // 30d
		first  = "backup-20260524T000000Z-000.tar.gz" // 25d
		newest = "backup-20260618T110000Z-000.tar.gz" // 1h
	)
	deleted, logs := pruneHotIn(t, dir, now, []pruneSnap{
		{marker, 40 * 24 * time.Hour, true},
		{third, 35 * 24 * time.Hour, false},
		{second, 30 * 24 * time.Hour, false},
		{first, 25 * 24 * time.Hour, false},
		{newest, time.Hour, false},
	}, 1, true)

	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0: nothing after the last offsite copy may be pruned:\n%s", deleted, logs)
	}
	left := remaining(t, dir, []string{marker, third, second, first, newest})
	for _, id := range []string{marker, third, second, first, newest} {
		if !left[id] {
			t.Errorf("%s was pruned although nothing newer than it is offsite:\n%s", id, logs)
		}
	}
	if !strings.Contains(logs, "keeping 4 snapshots: not older than the newest offsite copy "+marker) {
		t.Errorf("one line per pass must name how many were held back and against which copy;\nwant: keeping 4 snapshots: not older than the newest offsite copy %s\ngot:\n%s", marker, logs)
	}
	if !strings.Contains(logs, "(2026-05-09T12:00:00Z)") {
		t.Errorf("the hold-back line must name the offsite copy's timestamp:\n%s", logs)
	}
}

// Nothing offsited at all (criterion 3): the client has a remote but no snapshot
// is confirmed off it, so the pass has no permission to delete anything and says
// so once — not once per kept snapshot.
func TestHotPruneKeepsEverythingWhenNothingIsOffsited(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	const (
		oldest = "backup-20260509T000000Z-000.tar.gz" // 40d
		older  = "backup-20260514T000000Z-000.tar.gz" // 35d
		newest = "backup-20260618T110000Z-000.tar.gz" // 1h
	)
	deleted, logs := pruneHotIn(t, dir, now, []pruneSnap{
		{oldest, 40 * 24 * time.Hour, false},
		{older, 35 * 24 * time.Hour, false},
		{newest, time.Hour, false},
	}, 1, true)

	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0: with nothing offsited there is no copy elsewhere to justify a delete:\n%s", deleted, logs)
	}
	left := remaining(t, dir, []string{oldest, older, newest})
	for _, id := range []string{oldest, older, newest} {
		if !left[id] {
			t.Errorf("%s was pruned although nothing is offsited:\n%s", id, logs)
		}
	}
	want := "keeping 2 snapshots: nothing is offsited for this client"
	if !strings.Contains(logs, want) {
		t.Errorf("the pass must say why it pruned nothing;\nwant a line containing: %s\ngot:\n%s", want, logs)
	}
	if n := strings.Count(logs, "nothing is offsited"); n != 1 {
		t.Errorf("\"nothing is offsited\" appeared %d times, want once per pass:\n%s", n, logs)
	}
}

// Offsite off for this client (criterion 4): the guard does not exist, so the
// behaviour is age-only, exactly as before — even if offsite records happen to
// exist for some of the snapshots.
func TestHotPruneWithoutOffsiteIsAgeOnly(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	const (
		oldest = "backup-20260509T000000Z-000.tar.gz" // 40d
		older  = "backup-20260514T000000Z-000.tar.gz" // 35d
		newest = "backup-20260618T110000Z-000.tar.gz" // 1h
	)
	deleted, logs := pruneHotIn(t, dir, now, []pruneSnap{
		{oldest, 40 * 24 * time.Hour, true},
		{older, 35 * 24 * time.Hour, true},
		{newest, time.Hour, true},
	}, 1, false)

	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (age-only: everything past the horizon except the newest):\n%s", deleted, logs)
	}
	left := remaining(t, dir, []string{oldest, older, newest})
	if left[oldest] || left[older] {
		t.Errorf("age-only pruning did not delete the snapshots past the horizon: %v\n%s", left, logs)
	}
	if !left[newest] {
		t.Errorf("the newest snapshot was pruned:\n%s", logs)
	}
	if strings.Contains(logs, "offsite") {
		t.Errorf("offsite is not configured for this client's prune, so no line may cite it:\n%s", logs)
	}
}

// Dedupe rows (criterion 5, PR #42's semantics): an offsite record that names
// another snapshot's object counts — as offsite evidence, and therefore as the
// newest offsite copy — only while the object it points at is intact. Once the
// owner is remote_missing, the dependent row is not offsite evidence and cannot
// move the marker, so nothing is deleted on the strength of it.
func TestHotPruneDedupeRowsCountOnlyWhileTheirOwnerIsIntact(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	const (
		owner     = "backup-20260509T000000Z-000.tar.gz" // 40d, owns the object
		dependent = "backup-20260514T000000Z-000.tar.gz" // 35d, dedupe of the owner
		newest    = "backup-20260618T110000Z-000.tar.gz" // 1h
	)
	cases := []struct {
		name    string
		objs    []model.OffsiteObject
		deleted int
		gone    []string
		log     string
	}{
		{
			// The owner's object is intact, so the dependent IS offsite and is the
			// newest offsite copy: the owner is strictly older and goes. The
			// dependent stays (at the marker) — that is the rule, and it is what
			// makes a dedupe row observable in the outcome at all.
			name: "intact owner",
			objs: []model.OffsiteObject{
				{SnapshotID: owner, Remote: "local"},
				{SnapshotID: dependent, Remote: "local", DedupOf: owner},
			},
			deleted: 1,
			gone:    []string{owner},
			log:     "not older than the newest offsite copy " + dependent,
		},
		{
			// The owner is gone from the remote, so the dependent's claim is not
			// offsite evidence either (PR #42). Nothing is offsited at all, so
			// nothing may be deleted.
			name: "owner remote_missing",
			objs: []model.OffsiteObject{
				{SnapshotID: owner, Remote: "local", RemoteMissing: true},
				{SnapshotID: dependent, Remote: "local", DedupOf: owner},
			},
			deleted: 0,
			log:     "nothing is offsited for this client",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			snaps, _ := writePruneSnaps(t, dir, now, []pruneSnap{
				{owner, 40 * 24 * time.Hour, false},
				{dependent, 35 * 24 * time.Hour, false},
				{newest, time.Hour, false},
			})
			logsBuf := captureLog(t)
			deleted, failed, err := pruneHot(context.Background(),
				Deps{Now: func() time.Time { return now }},
				model.Client{ID: 7, Name: "c", Mode: model.ModeTarGz, RetentionDays: 1},
				mustServerMode(t, model.ModeTarGz), dir, snaps, true, offsitedSetFrom(tc.objs))
			if err != nil || failed != 0 {
				t.Fatalf("pruneHot: err=%v failed=%d", err, failed)
			}
			logs := logsBuf.String()
			if deleted != tc.deleted {
				t.Fatalf("deleted = %d, want %d:\n%s", deleted, tc.deleted, logs)
			}
			left := remaining(t, dir, []string{owner, dependent, newest})
			for _, id := range tc.gone {
				if left[id] {
					t.Errorf("%s should have been pruned:\n%s", id, logs)
				}
			}
			for _, id := range []string{owner, dependent, newest} {
				if !contains(tc.gone, id) && !left[id] {
					t.Errorf("%s must survive:\n%s", id, logs)
				}
			}
			if tc.log != "" && !strings.Contains(logs, tc.log) {
				t.Errorf("the pass must say %q:\n%s", tc.log, logs)
			}
		})
	}
}

// The case this card exists for: offsite_upload_mode=latest. Only the newest
// snapshot is ever offsited, so the marker is the newest snapshot itself and
// everything older is prunable. Under the per-snapshot guard none of them ever
// was, because none of them had a record of its own — retention blocked forever.
func TestHotPruneOnLatestUploadModePrunesEverythingOlderThanTheOneCopy(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	const (
		oldest = "backup-20260509T000000Z-000.tar.gz" // 40d
		mid    = "backup-20260514T000000Z-000.tar.gz" // 35d
		marker = "backup-20260618T110000Z-000.tar.gz" // 1h, the one copy latest mode uploaded
	)
	deleted, logs := pruneHotIn(t, dir, now, []pruneSnap{
		{oldest, 40 * 24 * time.Hour, false},
		{mid, 35 * 24 * time.Hour, false},
		{marker, time.Hour, true},
	}, 1, true)

	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2: with the newest snapshot offsite, everything older is prunable:\n%s", deleted, logs)
	}
	left := remaining(t, dir, []string{oldest, mid, marker})
	if left[oldest] || left[mid] {
		t.Errorf("snapshots older than the one offsite copy survived, which is the bug this card fixes: %v\n%s", left, logs)
	}
	if !left[marker] {
		t.Errorf("the newest snapshot was pruned:\n%s", logs)
	}
}

func contains(ids []string, id string) bool {
	for _, have := range ids {
		if have == id {
			return true
		}
	}
	return false
}
