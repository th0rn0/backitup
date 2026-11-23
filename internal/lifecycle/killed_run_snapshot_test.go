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
	"github.com/th0rn0/backitup/internal/store"
)

// killedRunNote is what the server writes into a run's log_tail when it has to
// declare the run dead itself — the one a killed client leaves behind.
const killedRunNote = "interrupted: client started a new backup without completing the previous run"

// mkKilledRun plants the two things a run that was killed before it could report
// leaves: the directory rsync was writing into, and the run row it opened and
// never finished. Nothing marks the directory and nothing names it — a killed
// run reports neither a failure (there is no code left to run) nor a snapshot id
// (it never got that far) — which is exactly why it used to pass as a backup.
func mkKilledRun(t *testing.T, st *store.Store, clientID int64, clientDir string, start, finished time.Time) string {
	t.Helper()
	id := start.UTC().Format(rsyncTS)
	dir := filepath.Join(clientDir, "snapshots", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"drone-runner.qcow2", "gitlab-runner.qcow2"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.RecordRun(context.Background(), model.Run{
		ClientID: clientID, StartedAt: start, FinishedAt: finished,
		Status: model.StatusFailed, LogTail: killedRunNote,
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// mkRun records the run row for a snapshot the client reported as ok.
func mkRun(t *testing.T, st *store.Store, clientID int64, id string, start, finished time.Time) {
	t.Helper()
	if _, err := st.RecordRun(context.Background(), model.Run{
		ClientID: clientID, StartedAt: start, FinishedAt: finished,
		Status: model.StatusOK, SnapshotID: id,
	}); err != nil {
		t.Fatal(err)
	}
}

// mkRunningRun records the row a run leaves while it is still going.
func mkRunningRun(t *testing.T, st *store.Store, clientID int64, start time.Time) {
	t.Helper()
	if _, err := st.RecordRun(context.Background(), model.Run{
		ClientID: clientID, StartedAt: start, Status: model.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
}

// The offsite worker tars whatever the listing hands it. A directory a killed
// run left behind is unmarked, so the only thing that can keep it off B2 is the
// run record — and it must, while the client's real snapshots still upload in
// the same pass.
func TestOffsiteSkipsSnapshotFromKilledRun(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	id, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 0, Enabled: true,
	})
	oldStart := now.Add(-30 * 24 * time.Hour)
	newStart := now.Add(-2 * time.Hour)
	oldID := oldStart.UTC().Format(rsyncTS)
	newID := newStart.UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, oldID, newID)
	mkRun(t, st, id, oldID, oldStart, oldStart.Add(20*time.Minute))
	mkRun(t, st, id, newID, newStart, newStart.Add(20*time.Minute))
	if err := os.Symlink(newID, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	// The killed run started 90 minutes ago and was declared failed 60 minutes
	// ago: old enough to be past the incomplete grace, so a pass would already be
	// reclaiming it. The upload must not have happened first.
	badID := mkKilledRun(t, st, id, dir, now.Add(-90*time.Minute), now.Add(-60*time.Minute))

	off := &fakeOffsite{}
	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	if len(off.up) != 2 {
		t.Fatalf("uploads = %d, want 2 (the real snapshots only): %v", len(off.up), off.up)
	}
	for obj := range off.up {
		if strings.Contains(obj, badID) {
			t.Fatalf("the killed run's directory was uploaded as %q", obj)
		}
	}
	objs, err := st.ListOffsiteObjects(context.Background(), id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	for _, o := range objs {
		if o.SnapshotID == badID {
			t.Fatalf("the killed run was recorded as offsited: %+v", o)
		}
	}
}

// Hiding is not enough on its own: a directory nothing lists is also a directory
// nothing deletes, and a half-copied 375 GB tree is how a backup volume fills up
// with nothing to show for it. The grace is measured from the run being recorded
// as not-ok, so a run the server has only just declared dead still gets its hour.
func TestLifecycleReclaimsSnapshotFromKilledRun(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	id, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 0, Enabled: true,
	})
	goodStart := now.Add(-2 * time.Hour)
	goodID := goodStart.UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, goodID)
	mkRun(t, st, id, goodID, goodStart, goodStart.Add(20*time.Minute))
	if err := os.Symlink(goodID, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	oldBadID := mkKilledRun(t, st, id, dir, now.Add(-5*time.Hour), now.Add(-3*time.Hour))
	freshBadID := mkKilledRun(t, st, id, dir, now.Add(-10*time.Minute), now.Add(-5*time.Minute))

	logs := runOnceCapturingLog(t, Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }})

	if _, err := os.Stat(filepath.Join(dir, "snapshots", oldBadID)); err == nil {
		t.Fatalf("the killed run's directory, recorded not-ok %s ago, should have been reclaimed", 3*time.Hour)
	}
	// Hidden at once (it is out of every listing and offsite pass from the moment
	// the run is known dead), deleted only after the grace.
	if _, err := os.Stat(filepath.Join(dir, "snapshots", freshBadID)); err != nil {
		t.Fatalf("a directory whose run was recorded not-ok 5m ago must survive the pass: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", goodID)); err != nil {
		t.Fatalf("the real snapshot must not be touched: %v", err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "snapshots", "latest")); target != goodID {
		t.Fatalf("latest -> %q, want %q", target, goodID)
	}
	// The operator's record: why each directory is not a backup, naming it.
	for _, want := range []string{
		"removed incomplete snapshot " + oldBadID,
		"left by a run that never reached ok",
		"snapshot " + freshBadID + " is not a backup",
		"interrupted: client started a new backup without completing the previous run",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("the pass must log %q, got:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "removed incomplete snapshot "+freshBadID) {
		t.Errorf("the fresh directory must not be logged as removed:\n%s", logs)
	}
	if strings.Contains(logs, "INTEGRITY FAIL") {
		t.Errorf("an unfinished snapshot is not an integrity failure of the backup:\n%s", logs)
	}
}

// Retention counts snapshots. The killed run's directory carries the newest
// timestamp of all, so if it were counted it would be protected as "the newest"
// and the client's real newest snapshot would be pruned instead.
func TestKilledRunLeftoversAreNotCountedByRetention(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	id, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	oldStart := now.Add(-40 * 24 * time.Hour)
	newestStart := now.Add(-2 * time.Hour)
	oldGoodID := oldStart.UTC().Format(rsyncTS)
	newestGoodID := newestStart.UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, oldGoodID, newestGoodID)
	mkRun(t, st, id, oldGoodID, oldStart, oldStart.Add(20*time.Minute))
	mkRun(t, st, id, newestGoodID, newestStart, newestStart.Add(20*time.Minute))
	if err := os.Symlink(newestGoodID, filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	// Five minutes ago: too fresh to be reclaimed in this pass, so the only
	// thing under test is what retention counts.
	badID := mkKilledRun(t, st, id, dir, now.Add(-10*time.Minute), now.Add(-5*time.Minute))

	if err := RunOnce(context.Background(), Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", newestGoodID)); err != nil {
		t.Fatalf("the newest REAL snapshot must be protected, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", oldGoodID)); err == nil {
		t.Fatal("the snapshot past the 14-day horizon should still be pruned")
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", badID)); err != nil {
		t.Fatalf("a directory whose run was recorded not-ok 5m ago must survive this pass: %v", err)
	}
}

// A run that is still going is not a killed run: its directory may be being
// written into right now, whatever its age, and this pass must leave both the
// directory and the run alone.
func TestInProgressRunKeepsItsSnapshotVisible(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	id, base, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 0, Enabled: true,
	})
	// A long backup: started 90 minutes ago (longer than the incomplete grace)
	// and still running (shorter than the 2h stale timeout).
	inFlightStart := now.Add(-90 * time.Minute)
	inFlightID := inFlightStart.UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, inFlightID)
	mkRunningRun(t, st, id, inFlightStart)

	sm, ok := mode.Server(model.ModeRsync)
	if !ok {
		t.Fatal("rsync server mode is not registered")
	}
	clientDir := filepath.Join(base, model.Slug("vmhost"))
	snaps, err := sm.List(context.Background(), clientDir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	visible, unbacked, err := VisibleSnapshots(context.Background(), st, model.Client{ID: id, Name: "vmhost", Mode: model.ModeRsync}, sm, snaps)
	if err != nil {
		t.Fatalf("VisibleSnapshots: %v", err)
	}
	if len(unbacked) != 0 {
		t.Fatalf("a run still in progress is not evidence of anything: %+v", unbacked)
	}
	if len(visible) != 1 || visible[0].ID != inFlightID {
		t.Fatalf("the in-progress snapshot must stay visible, got %+v", visible)
	}

	logs := runOnceCapturingLog(t, Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }})
	if strings.Contains(logs, "is not a backup") {
		t.Errorf("an in-progress run must not have its snapshot called incomplete:\n%s", logs)
	}
	if _, err := os.Stat(filepath.Join(dir, "snapshots", inFlightID)); err != nil {
		t.Fatalf("the in-progress snapshot must not be deleted: %v", err)
	}
}

// The inverse case the card asks about: a snapshot whose run history is gone must
// not be hidden forever. "No row" is not the same fact as "a row that never
// reached ok", and the two are told apart by the presence of the row, not by its
// age — so a pruned history leaves the snapshot exactly where it was.
func TestSnapshotWithNoRunHistoryIsLeftAlone(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	id, _, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 0, Enabled: true,
	})
	prunedStart := now.Add(-200 * 24 * time.Hour) // older than run-history retention
	prunedID := prunedStart.UTC().Format(rsyncTS)
	// The snapshot exists; the client's run rows for that era were deleted by
	// run-history retention, or it was placed here outside backitup entirely.
	mkRsyncSnapshots(t, dir, prunedID)

	sm, _ := mode.Server(model.ModeRsync)
	clientDir := filepath.Join(dir)
	snaps, err := sm.List(context.Background(), clientDir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	visible, unbacked, err := VisibleSnapshots(context.Background(), st, model.Client{ID: id, Name: "vmhost", Mode: model.ModeRsync}, sm, snaps)
	if err != nil {
		t.Fatalf("VisibleSnapshots: %v", err)
	}
	if len(unbacked) != 0 {
		t.Fatalf("no run row is not evidence that a directory is not a backup: %+v", unbacked)
	}
	if len(visible) != 1 || visible[0].ID != prunedID {
		t.Fatalf("a snapshot with no run history must stay visible, got %+v", visible)
	}
}

// An ok run whose row is old — the case where the run *is* still there but its
// log was cleared by BACKITUP_LOG_RETENTION_DAYS — is still a backup. The row is
// the evidence; the log is a convenience.
func TestOkRunWithClearedLogStillBacksItsSnapshot(t *testing.T) {
	st := openStore(t)
	now := time.Now().UTC()
	id, _, dir := newClient(t, st, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 0, Enabled: true,
	})
	start := now.Add(-30 * 24 * time.Hour)
	snapID := start.UTC().Format(rsyncTS)
	mkRsyncSnapshots(t, dir, snapID)
	// RecordRun with no log tail is what the log-retention pass leaves behind:
	// status, ids and timestamps intact, log_tail emptied.
	mkRun(t, st, id, snapID, start, start.Add(20*time.Minute))

	sm, _ := mode.Server(model.ModeRsync)
	snaps, err := sm.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	visible, unbacked, err := VisibleSnapshots(context.Background(), st, model.Client{ID: id, Name: "vmhost", Mode: model.ModeRsync}, sm, snaps)
	if err != nil {
		t.Fatalf("VisibleSnapshots: %v", err)
	}
	if len(unbacked) != 0 {
		t.Fatalf("an ok run backs its snapshot whatever its age or log state: %+v", unbacked)
	}
	if len(visible) != 1 {
		t.Fatalf("visible = %+v, want the snapshot", visible)
	}
}
