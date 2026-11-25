package store

// The write paths that only the server's handlers exercise had no test of their
// own: the account table, the run bookkeeping a client's status report drives,
// and the offsite-row replacement single mode depends on. They matter because
// each one is a statement the UI reads back — "this run is still going", "this
// object is in the bucket", "this account can log in" — and a wrong one is
// silent.
//
// Nothing here sleeps on the clock or reaches the network: every timestamp is
// one the test supplies, and every assertion is about what the store returns.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "writes.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// mkClient creates a client and returns its id.
func mkClient(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	id, err := st.CreateClient(t.Context(), model.Client{
		Name: name, Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client %q: %v", name, err)
	}
	return id
}

// ── accounts ────────────────────────────────────────────────────────────────

// The account table is what login reads. Every reader and writer is asserted
// here together because a mismatch between them is how a user logs in as a
// different user, or cannot log in at all.
func TestUserRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	if n, err := st.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("fresh store: CountUsers = %d, %v", n, err)
	}
	if u, err := st.GetUserByUsername(ctx, "alice"); err != nil || u != nil {
		t.Fatalf("missing user reads as (%+v, %v), want (nil, nil)", u, err)
	}

	id, err := st.CreateUser(ctx, "alice", "hash-1")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if id == 0 {
		t.Fatal("CreateUser returned id 0")
	}
	// The username is unique: a second account with the same name is refused
	// rather than silently producing two rows login would pick between.
	if _, err := st.CreateUser(ctx, "alice", "hash-2"); err == nil {
		t.Fatal("a duplicate username was accepted")
	}

	if _, err := st.CreateUser(ctx, "bob", "hash-b"); err != nil {
		t.Fatalf("create second user: %v", err)
	}

	// ListUsers is ordered by username, which is what the admin page renders.
	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "bob" {
		t.Fatalf("ListUsers = %+v, want alice then bob", users)
	}
	if users[0].ID != id || users[0].PasswordHash != "hash-1" {
		t.Errorf("alice came back as %+v", users[0])
	}
	if users[0].CreatedAt.IsZero() {
		t.Error("created_at was not parsed back")
	}

	// Reading by name and by id must agree.
	byName, err := st.GetUserByUsername(ctx, "alice")
	if err != nil || byName == nil {
		t.Fatalf("GetUserByUsername: %+v, %v", byName, err)
	}
	byID, err := st.GetUserByID(ctx, id)
	if err != nil || byID == nil {
		t.Fatalf("GetUserByID: %+v, %v", byID, err)
	}
	if byName.ID != byID.ID || byName.Username != byID.Username || byName.PasswordHash != byID.PasswordHash {
		t.Errorf("by name %+v and by id %+v disagree", byName, byID)
	}
	if u, err := st.GetUserByID(ctx, 9999); err != nil || u != nil {
		t.Errorf("unknown id reads as (%+v, %v), want (nil, nil)", u, err)
	}

	// UpsertUser is the create-or-reset-password path: the row is reused, not
	// duplicated, or a password reset would leave two accounts behind.
	if err := st.UpsertUser(ctx, "carol", "hash-c1"); err != nil {
		t.Fatalf("upsert new user: %v", err)
	}
	carol, err := st.GetUserByUsername(ctx, "carol")
	if err != nil || carol == nil {
		t.Fatalf("upserted user not found: %v", err)
	}
	if err := st.UpsertUser(ctx, "carol", "hash-c2"); err != nil {
		t.Fatalf("upsert existing user: %v", err)
	}
	again, err := st.GetUserByUsername(ctx, "carol")
	if err != nil || again == nil {
		t.Fatalf("re-read carol: %v", err)
	}
	if again.ID != carol.ID {
		t.Errorf("upsert created a second row: id %d -> %d", carol.ID, again.ID)
	}
	if again.PasswordHash != "hash-c2" {
		t.Errorf("password hash = %q, want the reset one", again.PasswordHash)
	}

	// Deletion: found accounts go, and a second delete reports "not found"
	// rather than pretending to have removed something.
	if err := st.DeleteUser(ctx, again.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if u, err := st.GetUserByID(ctx, again.ID); err != nil || u != nil {
		t.Errorf("deleted user still reads back: %+v, %v", u, err)
	}
	if err := st.DeleteUser(ctx, again.ID); err == nil {
		t.Error("deleting an already-deleted user reported success")
	}
	if n, err := st.CountUsers(ctx); err != nil || n != 2 {
		t.Errorf("CountUsers = %d, %v; want the two that are left", n, err)
	}
}

// ── runs ────────────────────────────────────────────────────────────────────

// A client's status report updates the running row it opened rather than
// inserting a second one, and the update is pinned to the client that owns the
// row — the guard against one client writing another's history.
func TestUpdateRunKeepsTheRowOnItsClient(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	owner := mkClient(t, st, "owner")
	other := mkClient(t, st, "other")

	started := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	runID, err := st.RecordRun(ctx, model.Run{
		ClientID: owner, StartedAt: started, FinishedAt: started,
		Status: model.StatusRunning,
	})
	if err != nil {
		t.Fatalf("record run: %v", err)
	}

	finished := started.Add(90 * time.Second)
	ok := model.Run{
		ClientID: owner, StartedAt: started, FinishedAt: finished,
		Status: model.StatusOK, Bytes: 4096, Files: 7,
		SnapshotID: "20260618T120000Z", LogTail: "synced 7 files",
	}
	if err := st.UpdateRun(ctx, runID, owner, ok); err != nil {
		t.Fatalf("update own run: %v", err)
	}

	got, err := st.GetRun(ctx, runID)
	if err != nil || got == nil {
		t.Fatalf("read back run: %+v, %v", got, err)
	}
	if got.Status != model.StatusOK || got.Bytes != 4096 || got.Files != 7 ||
		got.SnapshotID != "20260618T120000Z" || got.LogTail != "synced 7 files" {
		t.Fatalf("run after update = %+v", got)
	}
	if !got.FinishedAt.Equal(finished) {
		t.Errorf("finished_at = %s, want %s", got.FinishedAt, finished)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("started_at changed to %s; the report must not rewrite when the run began", got.StartedAt)
	}
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM runs`); n != 1 {
		t.Fatalf("the update added a row: runs holds %d", n)
	}

	// The same update sent by a different client must be refused, and must not
	// touch the row.
	if err := st.UpdateRun(ctx, runID, other, model.Run{
		ClientID: other, Status: model.StatusFailed, FinishedAt: finished,
	}); err == nil {
		t.Fatal("a client was allowed to update another client's run")
	}
	still, err := st.GetRun(ctx, runID)
	if err != nil || still == nil {
		t.Fatalf("re-read run: %v", err)
	}
	if still.Status != model.StatusOK {
		t.Errorf("the refused update still changed the row: status = %q", still.Status)
	}

	// An unknown id is an error, not a silent no-op.
	if err := st.UpdateRun(ctx, 9999, owner, ok); err == nil {
		t.Error("updating a run that does not exist reported success")
	}
	// And a missing run reads as (nil, nil) rather than an error.
	if r, err := st.GetRun(ctx, 9999); err != nil || r != nil {
		t.Errorf("missing run reads as (%+v, %v), want (nil, nil)", r, err)
	}
}

// The dashboard asks for one latest run per client in a single query; a client
// that has never run must be absent from the map rather than present and empty.
func TestLatestRunAllClientsOneRowPerClient(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	busy := mkClient(t, st, "busy")
	quiet := mkClient(t, st, "quiet")

	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	for i, status := range []model.RunStatus{model.StatusOK, model.StatusFailed, model.StatusOK} {
		at := base.Add(time.Duration(i) * time.Hour)
		if _, err := st.RecordRun(ctx, model.Run{
			ClientID: busy, StartedAt: at, FinishedAt: at.Add(time.Minute),
			Status: status, Files: int64(i),
		}); err != nil {
			t.Fatalf("record run %d: %v", i, err)
		}
	}

	latest, err := st.LatestRunAllClients(ctx)
	if err != nil {
		t.Fatalf("LatestRunAllClients: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("map holds %d clients, want 1 (the one with runs)", len(latest))
	}
	got := latest[busy]
	if got == nil {
		t.Fatal("the client with runs is missing from the map")
	}
	if !got.StartedAt.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("latest run started at %s, want the newest (%s)", got.StartedAt, base.Add(2*time.Hour))
	}
	if _, ok := latest[quiet]; ok {
		t.Error("a client that never ran is in the map")
	}
}

// ListRunsSince is the interrupted-run guard's query. It takes a prefix of
// log_tail — the full column is up to 64 KB per row — so the marker the server
// writes when it declares a silent run dead has to survive in that prefix.
func TestListRunsSinceBoundsTheLogTail(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")

	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	long := "interrupted: run exceeded maximum allowed duration " + strings.Repeat("x", 5000)
	for i := 0; i < 3; i++ {
		at := base.Add(time.Duration(i) * time.Minute)
		if _, err := st.RecordRun(ctx, model.Run{
			ClientID: id, StartedAt: at, FinishedAt: at.Add(time.Second),
			Status: model.StatusFailed, LogTail: long,
		}); err != nil {
			t.Fatalf("record run: %v", err)
		}
	}

	runs, err := st.ListRunsSince(ctx, id, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("ListRunsSince: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs since the cutoff, want 2", len(runs))
	}
	// Ordered by started_at ascending: the guard reads them in order.
	if !runs[0].StartedAt.Before(runs[1].StartedAt) {
		t.Errorf("runs are not ascending: %s then %s", runs[0].StartedAt, runs[1].StartedAt)
	}
	for _, r := range runs {
		if len(r.LogTail) > 512 {
			t.Errorf("log tail is %d bytes; the query is supposed to load a 512-byte prefix", len(r.LogTail))
		}
		if !strings.HasPrefix(r.LogTail, "interrupted: run exceeded") {
			t.Errorf("log tail prefix = %q, want the server's interrupted note", r.LogTail[:min(40, len(r.LogTail))])
		}
	}
}

// PruneRunLogs is the log-retention pass: it clears the text and keeps the row.
// A pruned row that vanished would take the run's status and byte count with it.
func TestPruneRunLogsKeepsTheRun(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")

	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	oldID, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: base.Add(-30 * 24 * time.Hour), FinishedAt: base.Add(-30 * 24 * time.Hour),
		Status: model.StatusOK, Bytes: 1234, Files: 5, LogTail: "ancient log",
	})
	if err != nil {
		t.Fatalf("record old run: %v", err)
	}
	recentID, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: base, FinishedAt: base, Status: model.StatusOK, LogTail: "today's log",
	})
	if err != nil {
		t.Fatalf("record recent run: %v", err)
	}

	n, err := st.PruneRunLogs(ctx, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("PruneRunLogs: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}

	oldRun, err := st.GetRun(ctx, oldID)
	if err != nil || oldRun == nil {
		t.Fatalf("the pruned run's row is gone: %+v, %v", oldRun, err)
	}
	if oldRun.LogTail != "" {
		t.Errorf("log tail = %q, want it cleared", oldRun.LogTail)
	}
	if oldRun.Status != model.StatusOK || oldRun.Bytes != 1234 || oldRun.Files != 5 {
		t.Errorf("pruning the log changed the run's facts: %+v", oldRun)
	}
	recent, err := st.GetRun(ctx, recentID)
	if err != nil || recent == nil {
		t.Fatalf("re-read recent run: %v", err)
	}
	if recent.LogTail != "today's log" {
		t.Errorf("a run inside the window lost its log tail: %q", recent.LogTail)
	}
	// Idempotent: nothing left to prune.
	if n, err := st.PruneRunLogs(ctx, base.Add(-24*time.Hour)); err != nil || n != 0 {
		t.Errorf("second pass pruned %d, %v", n, err)
	}
}

// Stale runs: the safety net for a client that dies mid-run. Only rows that are
// still `running` AND older than the cutoff are settled, and the row says what
// happened so the operator's log is not simply empty.
func TestStaleRunSweeps(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")

	base := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	staleID, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: base.Add(-48 * time.Hour), FinishedAt: base.Add(-48 * time.Hour),
		Status: model.StatusRunning,
	})
	if err != nil {
		t.Fatalf("record stale run: %v", err)
	}
	freshID, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: base, FinishedAt: base, Status: model.StatusRunning,
	})
	if err != nil {
		t.Fatalf("record fresh run: %v", err)
	}
	doneID, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: base.Add(-72 * time.Hour), FinishedAt: base.Add(-72 * time.Hour),
		Status: model.StatusOK, LogTail: "leave me alone",
	})
	if err != nil {
		t.Fatalf("record finished run: %v", err)
	}

	n, err := st.MarkStaleRunsFailed(ctx, base.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("MarkStaleRunsFailed: %v", err)
	}
	if n != 1 {
		t.Errorf("marked %d runs, want only the stale running one", n)
	}
	stale, _ := st.GetRun(ctx, staleID)
	if stale == nil || stale.Status != model.StatusFailed {
		t.Fatalf("stale run = %+v, want failed", stale)
	}
	if !strings.Contains(stale.LogTail, "interrupted") {
		t.Errorf("stale run's log tail = %q, want the interruption reason", stale.LogTail)
	}
	if fresh, _ := st.GetRun(ctx, freshID); fresh == nil || fresh.Status != model.StatusRunning {
		t.Errorf("a run inside the window was settled: %+v", fresh)
	}
	if done, _ := st.GetRun(ctx, doneID); done == nil || done.LogTail != "leave me alone" {
		t.Errorf("a finished run was rewritten: %+v", done)
	}

	// MarkClientRunningRunFailed is the per-client version the status endpoint
	// uses when the same client starts a new backup.
	if err := st.MarkClientRunningRunFailed(ctx, id); err != nil {
		t.Fatalf("MarkClientRunningRunFailed: %v", err)
	}
	fresh, err := st.GetRun(ctx, freshID)
	if err != nil || fresh == nil || fresh.Status != model.StatusFailed {
		t.Fatalf("running run after the per-client sweep = %+v, %v, want failed", fresh, err)
	}
	if !strings.Contains(fresh.LogTail, "started a new backup") {
		t.Errorf("log tail = %q, want it to name the cause", fresh.LogTail)
	}
	// A second sweep has nothing left to do.
	if err := st.MarkClientRunningRunFailed(ctx, id); err != nil {
		t.Errorf("idempotent sweep: %v", err)
	}
}

// ── offsite runs ────────────────────────────────────────────────────────────

// An adhoc upload opens a running row so the dashboard can show progress, and
// the sweep at startup settles any row a crash left behind.
func TestOffsiteRunLifecycle(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")

	started := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	runID, err := st.StartOffsiteRun(ctx, id, "adhoc", started)
	if err != nil {
		t.Fatalf("StartOffsiteRun: %v", err)
	}
	running, err := st.RunningOffsiteClientIDs(ctx)
	if err != nil {
		t.Fatalf("RunningOffsiteClientIDs: %v", err)
	}
	if !running[id] {
		t.Fatalf("a client with a running upload is not in %v", running)
	}

	finished := started.Add(2 * time.Minute)
	if err := st.FinishOffsiteRun(ctx, runID, "ok", 3, 12<<20, "", finished); err != nil {
		t.Fatalf("FinishOffsiteRun: %v", err)
	}
	running, err = st.RunningOffsiteClientIDs(ctx)
	if err != nil {
		t.Fatalf("RunningOffsiteClientIDs after finish: %v", err)
	}
	if running[id] {
		t.Error("a finished upload is still reported as running")
	}

	runs, err := st.ListOffsiteRuns(ctx, id, 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("ListOffsiteRuns = %d runs, %v", len(runs), err)
	}
	got := runs[0]
	if got.Status != "ok" || got.SnapshotsUploaded != 3 || got.BytesUploaded != 12<<20 {
		t.Errorf("finished run = %+v", got)
	}
	if got.TriggeredBy != "adhoc" {
		t.Errorf("triggered_by = %q, want adhoc", got.TriggeredBy)
	}
	if !got.FinishedAt.Equal(finished) {
		t.Errorf("finished_at = %s, want %s", got.FinishedAt, finished)
	}

	// A second row, still running when the server restarts: the startup sweep
	// has to settle it, or the dashboard shows an upload that will never end.
	if _, err := st.StartOffsiteRun(ctx, id, "adhoc", started.Add(time.Hour)); err != nil {
		t.Fatalf("second StartOffsiteRun: %v", err)
	}
	if err := st.MarkStaleOffsiteRunsFailed(ctx); err != nil {
		t.Fatalf("MarkStaleOffsiteRunsFailed: %v", err)
	}
	if r, _ := st.RunningOffsiteClientIDs(ctx); r[id] {
		t.Error("the startup sweep left an upload running")
	}
	runs, err = st.ListOffsiteRuns(ctx, id, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("after the sweep: %d runs, %v", len(runs), err)
	}
	if runs[0].Status != "failed" || !strings.Contains(runs[0].ErrorText, "interrupted by server restart") {
		t.Errorf("swept row = %+v, want failed and to say why", runs[0])
	}
	if runs[1].Status != "ok" {
		t.Errorf("the sweep rewrote a completed run: %+v", runs[1])
	}
}

// Two uploads in the same second share a started_at, so "which ran last" has to
// come from the row id — the single-generation prune guard trusts the newest row
// to decide whether the cold copy is current.
func TestListOffsiteRunsBreaksTiesOnID(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")

	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	// Inserted worse-outcome-first on purpose: with a shared started_at, an
	// implementation that orders by insertion (or ascending) hands back the
	// older row first, and this test then reads "failed" where it wants "ok".
	for _, status := range []string{"failed", "ok"} {
		if err := st.RecordOffsiteRun(ctx, model.OffsiteRun{
			ClientID: id, TriggeredBy: "scheduled", StartedAt: at, FinishedAt: at,
			Status: status, ErrorText: status,
		}); err != nil {
			t.Fatalf("RecordOffsiteRun: %v", err)
		}
	}

	runs, err := st.ListOffsiteRuns(ctx, id, 10)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %d, %v", len(runs), err)
	}
	if runs[0].Status != "ok" {
		t.Errorf("newest row = %+v, want the one inserted last (ok), not the first inserted, even though both started at %s", runs[0], at)
	}
	// The limit is honoured, newest first.
	one, err := st.ListOffsiteRuns(ctx, id, 1)
	if err != nil || len(one) != 1 || one[0].Status != "ok" {
		t.Errorf("limit=1 returned %+v, %v", one, err)
	}
}

// ── offsite objects ─────────────────────────────────────────────────────────

// Single mode keeps one object per client at a fixed name, so the row is a claim
// about what the bucket holds NOW: a second upload has to replace the first row,
// not sit beside it. Two rows would say the bucket still holds the earlier
// snapshot, and the hot pruner deletes on the strength of that claim.
func TestReplaceSingleOffsiteObjectLeavesOneCurrentClaim(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")
	other := mkClient(t, st, "other")

	first := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	if err := st.ReplaceSingleOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "20260618T120000Z", Remote: "b2",
		Bytes: 1 << 20, UploadedAt: first, Checksum: "sum-1",
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	// A different client's row must survive the replacement untouched.
	if err := st.ReplaceSingleOffsiteObject(ctx, model.OffsiteObject{
		ClientID: other, SnapshotID: "20260618T110000Z", Remote: "b2", Bytes: 7, UploadedAt: first,
	}); err != nil {
		t.Fatalf("other client: %v", err)
	}

	second := first.Add(24 * time.Hour)
	if err := st.ReplaceSingleOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "20260619T120000Z", Remote: "b2",
		Bytes: 2 << 20, UploadedAt: second, Checksum: "sum-2",
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}

	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM offsite_objects WHERE client_id = ?`, id); n != 1 {
		t.Fatalf("client has %d offsite rows after two single-mode uploads, want 1", n)
	}
	objs, err := st.ListOffsiteObjects(ctx, id)
	if err != nil || len(objs) != 1 {
		t.Fatalf("list = %d objs, %v", len(objs), err)
	}
	got := objs[0]
	if got.SnapshotID != "20260619T120000Z" || got.Bytes != 2<<20 || got.Checksum != "sum-2" {
		t.Errorf("current claim = %+v, want the newest upload", got)
	}
	if got.DedupOf != "" {
		t.Errorf("a single-mode row carries dedup_of = %q; the fixed name is the object", got.DedupOf)
	}
	if !got.UploadedAt.Equal(second) {
		t.Errorf("uploaded_at = %s, want %s", got.UploadedAt, second)
	}
	if o, _ := st.ListOffsiteObjects(ctx, other); len(o) != 1 {
		t.Errorf("the other client's row was disturbed: %+v", o)
	}

	// A zero UploadedAt is filled in rather than stored as the year 1: an
	// offsite horizon measured from year 1 would treat the object as ancient.
	if err := st.ReplaceSingleOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "20260620T120000Z", Remote: "b2", Bytes: 3,
	}); err != nil {
		t.Fatalf("replace with no timestamp: %v", err)
	}
	objs, _ = st.ListOffsiteObjects(ctx, id)
	if len(objs) != 1 || objs[0].UploadedAt.IsZero() || objs[0].UploadedAt.Year() < 2000 {
		t.Fatalf("uploaded_at was not defaulted to now: %+v", objs)
	}
}

// The dedupe query answers "does the bucket already hold this content?". A
// dedupe row is not an owner (it owns nothing) and a row the verification pass
// reported missing is not either — deduplicating into a hole loses the snapshot.
func TestOffsiteChecksumOwner(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")
	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	// No checksum means no content identity to match.
	if owner, ok, err := st.OffsiteChecksumOwner(ctx, id, "b2", ""); err != nil || ok || owner != "" {
		t.Fatalf("empty checksum = (%q, %v, %v), want no owner", owner, ok, err)
	}
	if _, ok, err := st.OffsiteChecksumOwner(ctx, id, "b2", "sum-a"); err != nil || ok {
		t.Fatalf("unknown checksum claimed an owner: %v, %v", ok, err)
	}

	// A real object owns its content.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "s-real", Remote: "b2", Bytes: 100, UploadedAt: at, Checksum: "sum-a",
	}); err != nil {
		t.Fatalf("record real object: %v", err)
	}
	owner, ok, err := st.OffsiteChecksumOwner(ctx, id, "b2", "sum-a")
	if err != nil || !ok || owner != "s-real" {
		t.Fatalf("owner = (%q, %v, %v), want s-real", owner, ok, err)
	}

	// A dedupe row sharing that checksum does not become an owner.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "s-dedupe", Remote: "b2", Bytes: 0, UploadedAt: at.Add(time.Minute),
		Checksum: "sum-b", DedupOf: "s-real",
	}); err != nil {
		t.Fatalf("record dedupe row: %v", err)
	}
	if owner, ok, err := st.OffsiteChecksumOwner(ctx, id, "b2", "sum-b"); err != nil || ok {
		t.Errorf("a dedupe row was reported as an owner: %q, %v, %v", owner, ok, err)
	}

	// A row the verification pass reported missing is not an owner either.
	realID := offsiteRowIDFor(t, st, id, "s-real")
	if err := st.UpdateOffsiteRemoteStatus(ctx, realID, true, at.Add(2*time.Minute)); err != nil {
		t.Fatalf("mark remote missing: %v", err)
	}
	if owner, ok, err := st.OffsiteChecksumOwner(ctx, id, "b2", "sum-a"); err != nil || ok {
		t.Errorf("an object reported missing still owns content: %q, %v, %v", owner, ok, err)
	}

	// TotalOffsiteBytes sums what is recorded; the dedupe row contributes 0.
	if total, err := st.TotalOffsiteBytes(ctx); err != nil || total != 100 {
		t.Errorf("TotalOffsiteBytes = %d, %v; want 100 (the dedupe row holds no bytes)", total, err)
	}
}

// The dashboard's "last offsite" column is one query over all clients; a client
// with no objects is absent, and each entry is that client's newest upload.
func TestLatestOffsiteAllClients(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	a := mkClient(t, st, "a")
	b := mkClient(t, st, "b")
	_ = mkClient(t, st, "never")

	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	for _, o := range []model.OffsiteObject{
		{ClientID: a, SnapshotID: "a-old", Remote: "b2", Bytes: 1, UploadedAt: at.Add(-24 * time.Hour)},
		{ClientID: a, SnapshotID: "a-new", Remote: "b2", Bytes: 1, UploadedAt: at},
		{ClientID: b, SnapshotID: "b-only", Remote: "b2", Bytes: 1, UploadedAt: at.Add(-time.Hour)},
	} {
		if err := st.RecordOffsiteObject(ctx, o); err != nil {
			t.Fatalf("record %s: %v", o.SnapshotID, err)
		}
	}

	latest, err := st.LatestOffsiteAllClients(ctx)
	if err != nil {
		t.Fatalf("LatestOffsiteAllClients: %v", err)
	}
	if len(latest) != 2 {
		t.Fatalf("map holds %d clients, want the 2 with objects", len(latest))
	}
	if latest[a] == nil || !latest[a].Equal(at) {
		t.Errorf("client a = %v, want its newest upload (%s)", latest[a], at)
	}
	if latest[b] == nil || !latest[b].Equal(at.Add(-time.Hour)) {
		t.Errorf("client b = %v", latest[b])
	}
}

// Forgetting a tracked object is an index operation: the row goes, the object
// stays in the bucket, and every dedupe row that pointed at it goes with it — a
// claim about an untracked object is a chain pointing at nothing.
func TestForgetOffsiteObjectDropsTheChain(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")
	at := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "s-owner", Remote: "b2", Bytes: 10, UploadedAt: at, Checksum: "sum-x",
	}); err != nil {
		t.Fatalf("record owner: %v", err)
	}
	for _, snap := range []string{"s-copy-1", "s-copy-2"} {
		if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: id, SnapshotID: snap, Remote: "b2", Bytes: 0, UploadedAt: at,
			Checksum: "sum-" + snap, DedupOf: "s-owner",
		}); err != nil {
			t.Fatalf("record dedupe %s: %v", snap, err)
		}
	}
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM offsite_objects`); n != 3 {
		t.Fatalf("setup holds %d rows, want 3", n)
	}

	dropped, err := st.ForgetOffsiteObject(ctx, id, "s-owner", "b2")
	if err != nil {
		t.Fatalf("ForgetOffsiteObject: %v", err)
	}
	if dropped != 2 {
		t.Errorf("reported %d dedupe rows dropped, want 2", dropped)
	}
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM offsite_objects`); n != 0 {
		t.Fatalf("%d rows left after forgetting the object they claimed to be in", n)
	}

	// Forgetting something that is not tracked is a no-op, not an error: the
	// route is idempotent by design.
	if dropped, err := st.ForgetOffsiteObject(ctx, id, "s-owner", "b2"); err != nil || dropped != 0 {
		t.Errorf("second forget = %d, %v", dropped, err)
	}
}

// ── remotes ─────────────────────────────────────────────────────────────────

// The edit and delete paths for a configured remote: the name and backend are
// immutable, the config is replaceable, and a deleted remote is gone.
func TestRemoteUpdateAndDelete(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()

	if err := st.CreateRemote(ctx, model.Remote{
		Name: "b2-main", Backend: model.BackendB2,
		Config: map[string]string{"account": "acct", "key": "key-1"},
	}); err != nil {
		t.Fatalf("CreateRemote: %v", err)
	}

	if err := st.UpdateRemote(ctx, "b2-main", map[string]string{"account": "acct", "key": "key-2"}); err != nil {
		t.Fatalf("UpdateRemote: %v", err)
	}
	got, err := st.GetRemoteByName(ctx, "b2-main")
	if err != nil || got == nil {
		t.Fatalf("GetRemoteByName: %+v, %v", got, err)
	}
	if got.Config["key"] != "key-2" {
		t.Errorf("config after update = %v, want the new key", got.Config)
	}
	if got.Backend != model.BackendB2 || got.Name != "b2-main" {
		t.Errorf("update changed the immutable fields: %+v", got)
	}
	// Updating a name nobody configured must neither invent a row nor fail: the
	// UPDATE simply matches nothing.
	if err := st.UpdateRemote(ctx, "not-there", map[string]string{"k": "v"}); err != nil {
		t.Errorf("UpdateRemote on an unknown name: %v", err)
	}
	if r, _ := st.GetRemoteByName(ctx, "not-there"); r != nil {
		t.Errorf("UpdateRemote created a row: %+v", r)
	}

	if err := st.DeleteRemote(ctx, "b2-main"); err != nil {
		t.Fatalf("DeleteRemote: %v", err)
	}
	if r, err := st.GetRemoteByName(ctx, "b2-main"); err != nil || r != nil {
		t.Errorf("deleted remote still reads back: %+v, %v", r, err)
	}
	if list, err := st.ListRemotes(ctx); err != nil || len(list) != 0 {
		t.Errorf("ListRemotes after delete = %+v, %v", list, err)
	}
}

// UpdateClientOffsite is the single place the offsite destination is written.
// An empty upload mode means "all" — storing "" would leave the client in a mode
// nothing reads.
func TestUpdateClientOffsite(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	id := mkClient(t, st, "c")

	if err := st.UpdateClientOffsite(ctx, id, "b2-main", "cold/c", "", ""); err != nil {
		t.Fatalf("UpdateClientOffsite: %v", err)
	}
	got, err := st.GetClient(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("GetClientByID: %+v, %v", got, err)
	}
	if got.OffsiteRemote != "b2-main" || got.OffsiteDir != "cold/c" {
		t.Errorf("offsite destination = %q/%q", got.OffsiteRemote, got.OffsiteDir)
	}
	if got.OffsiteUploadMode != model.OffsiteUploadAll {
		t.Errorf("upload mode = %q, want the default %q", got.OffsiteUploadMode, model.OffsiteUploadAll)
	}

	if err := st.UpdateClientOffsite(ctx, id, "b2-main", "cold/c", "0 3 * * *", model.OffsiteUploadSingle); err != nil {
		t.Fatalf("UpdateClientOffsite (single): %v", err)
	}
	got, _ = st.GetClient(ctx, id)
	if got.OffsiteUploadMode != model.OffsiteUploadSingle || got.OffsiteCron != "0 3 * * *" {
		t.Errorf("mode/cron = %q/%q, want single with the schedule", got.OffsiteUploadMode, got.OffsiteCron)
	}

	// Disabling offsite is an empty remote, not a deleted client.
	if err := st.UpdateClientOffsite(ctx, id, "", "", "", ""); err != nil {
		t.Fatalf("disable offsite: %v", err)
	}
	if got, _ = st.GetClient(ctx, id); got.OffsiteRemote != "" {
		t.Errorf("offsite remote = %q after disabling", got.OffsiteRemote)
	}

	if err := st.UpdateClientOffsite(ctx, 9999, "b2-main", "", "", ""); err == nil {
		t.Error("updating a client that does not exist reported success")
	}
}
