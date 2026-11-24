package mode

import (
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// runNamedTestMode is the smallest ServerMode that names its snapshots after the
// run start, which is all UnbackedSnapshots type-asserts for. Everything else is
// the package's own unimplemented fake: the policy never lists, verifies or
// deletes anything, it only judges entries it is handed.
type runNamedTestMode struct{ fakeServerMode }

func (runNamedTestMode) RunStart(s Snapshot) (time.Time, bool) {
	t, err := time.Parse("20060102T150405Z", s.ID)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

var _ RunStartNamed = runNamedTestMode{}

const unbackedTestTS = "20060102T150405Z"

// snapshotAt builds the mode.Snapshot a listing would return for a directory
// named at t.
func snapshotAt(t time.Time) Snapshot {
	return Snapshot{ID: t.UTC().Format(unbackedTestTS), CreatedAt: t.UTC()}
}

// The judgement the whole guard rests on: a directory whose run never reached ok
// is not a backup, and it is the *presence* of the row that says so, not its
// age, its log or anything the client reports.
func TestUnbackedSnapshots(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	snap := snapshotAt(start)

	tests := []struct {
		name  string
		mode  ServerMode
		runs  []model.Run
		want  bool   // is the snapshot unbacked?
		note  string // substring the reason must carry
		since time.Time
	}{
		{
			name: "ok run names it",
			mode: runNamedTestMode{},
			runs: []model.Run{{ID: 7, Status: model.StatusOK, SnapshotID: snap.ID, StartedAt: start, FinishedAt: start.Add(time.Hour)}},
		},
		{
			name: "ok run names it, row far older than the log retention window",
			// PruneRunLogs clears log_tail and leaves the row: the row is the
			// evidence, so an old snapshot backed by an ok run stays a backup.
			mode: runNamedTestMode{},
			runs: []model.Run{{ID: 7, Status: model.StatusOK, SnapshotID: snap.ID, StartedAt: start.AddDate(0, 0, -200), FinishedAt: start.AddDate(0, 0, -200)}},
		},
		{
			name: "no run row at all",
			// Absence is not evidence: a directory created outside backitup, or
			// whose run history was pruned, looks exactly like this.
			mode: runNamedTestMode{},
			runs: []model.Run{{ID: 1, Status: model.StatusOK, SnapshotID: "20250101T000000Z", StartedAt: start.AddDate(0, 0, -400)}},
		},
		{
			name: "run still running",
			// The backup may be writing into the directory right now.
			mode: runNamedTestMode{},
			runs: []model.Run{{ID: 9, Status: model.StatusRunning, StartedAt: start.Add(time.Second)}},
		},
		{
			name: "overlap only: that run created nothing, so it accuses nothing",
			mode: runNamedTestMode{},
			runs: []model.Run{{ID: 9, Status: model.StatusOverlap, StartedAt: start, FinishedAt: start}},
		},
		{
			name: "failed run started with the directory",
			mode: runNamedTestMode{},
			runs: []model.Run{{
				ID: 11, Status: model.StatusFailed, StartedAt: start, FinishedAt: start.Add(3 * time.Hour),
				LogTail: "interrupted: client started a new backup without completing the previous run",
			}},
			want:  true,
			note:  "run 11",
			since: start.Add(3 * time.Hour),
		},
		{
			name: "failed run and a later ok run for a different snapshot",
			// The ok run is real, but it did not create this directory.
			mode: runNamedTestMode{},
			runs: []model.Run{
				{ID: 11, Status: model.StatusFailed, StartedAt: start, FinishedAt: start.Add(time.Minute)},
				{ID: 12, Status: model.StatusOK, SnapshotID: "20260103T000000Z", StartedAt: start.Add(24 * time.Hour), FinishedAt: start.Add(25 * time.Hour)},
			},
			want:  true,
			note:  "recorded failed",
			since: start.Add(time.Minute),
		},
		{
			name: "failed run started after the directory is a later run",
			mode: runNamedTestMode{},
			runs: []model.Run{{ID: 13, Status: model.StatusFailed, StartedAt: start.Add(2 * time.Minute), FinishedAt: start.Add(3 * time.Minute)}},
		},
		{
			name: "failed run from a different second entirely",
			mode: runNamedTestMode{},
			runs: []model.Run{{ID: 14, Status: model.StatusFailed, StartedAt: start.Add(-30 * time.Minute), FinishedAt: start.Add(-29 * time.Minute)}},
		},
		{
			name: "a mode whose names are not the run start is never attributed",
			mode: fakeServerMode{m: model.ModeTarGz},
			runs: []model.Run{{ID: 15, Status: model.StatusFailed, StartedAt: start, FinishedAt: start.Add(time.Minute)}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := UnbackedSnapshots(tt.mode, []Snapshot{snap}, tt.runs)
			if !tt.want {
				if len(got) != 0 {
					t.Fatalf("UnbackedSnapshots = %+v, want nothing: this directory must stay a snapshot", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("UnbackedSnapshots = %+v, want exactly the one directory", got)
			}
			u := got[0]
			if u.Snapshot.ID != snap.ID {
				t.Errorf("snapshot = %q, want %q", u.Snapshot.ID, snap.ID)
			}
			if !u.Since.Equal(tt.since) {
				t.Errorf("Since = %s, want %s (the moment the run was recorded as not-ok)", u.Since, tt.since)
			}
			if !strings.Contains(u.Reason, tt.note) {
				t.Errorf("Reason = %q, want it to carry %q", u.Reason, tt.note)
			}
			if u.RunID == 0 || u.Status != model.StatusFailed {
				t.Errorf("RunID/Status = %d/%q, want the failed run that named it", u.RunID, u.Status)
			}
		})
	}
}

// The window between a run's started_at and the snapshot name it writes is one
// control-channel round trip; anything wider than RunStartWindow is a different
// run and must not be blamed for the directory.
func TestUnbackedSnapshotsMatchesOnlyWithinTheRunStartWindow(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	snap := snapshotAt(start)
	mk := func(lag time.Duration) []model.Run {
		return []model.Run{{ID: 20, Status: model.StatusFailed, StartedAt: start.Add(-lag), FinishedAt: start.Add(-lag).Add(time.Minute)}}
	}
	if got := UnbackedSnapshots(runNamedTestMode{}, []Snapshot{snap}, mk(RunStartWindow-time.Second)); len(got) != 1 {
		t.Fatalf("a run started just inside the window must be attributed: %+v", got)
	}
	if got := UnbackedSnapshots(runNamedTestMode{}, []Snapshot{snap}, mk(RunStartWindow+time.Second)); len(got) != 0 {
		t.Fatalf("a run started outside the window must not be blamed for the directory: %+v", got)
	}
}

// A name the mode cannot read as a run start (a hand-placed directory, an older
// layout) is left alone: there is nothing to attribute it to, so nothing may be
// said about it.
func TestUnbackedSnapshotsIgnoresUnattributableNames(t *testing.T) {
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	runs := []model.Run{{ID: 21, Status: model.StatusFailed, StartedAt: start, FinishedAt: start.Add(time.Minute)}}
	snaps := []Snapshot{{ID: "not-a-timestamp", CreatedAt: start}}
	if got := UnbackedSnapshots(runNamedTestMode{}, snaps, runs); len(got) != 0 {
		t.Fatalf("UnbackedSnapshots = %+v, want nothing for a name it cannot attribute", got)
	}
}
