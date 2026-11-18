package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/th0rn0/backitup/internal/model"
)

// captureWarnings replaces the logging sink for one test and returns a pointer to
// the lines it collects, so a test asserts on exactly what an operator would see
// on startup.
func captureWarnings(t *testing.T) *[]string {
	t.Helper()
	var warnings []string
	prevWarnf := warnf
	warnf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { warnf = prevWarnf })
	return &warnings
}

// writeStrandedRunsNewDB builds the database the pre-transactional build left
// behind when it died between `DROP TABLE runs` and `ALTER TABLE runs_new RENAME
// TO runs`: one client, one offsite object, every run in `runs_new`, and no
// `runs` table at all. This is the measured shape from the card.
func writeStrandedRunsNewDB(t *testing.T, path string) {
	t.Helper()
	writePreRunningDB(t, path) // legacy shape: one client, three runs, one offsite object
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen legacy db: %v", err)
	}
	defer func() { _ = db.Close() }()
	// The old swap, interrupted at the one point that loses the history.
	for _, stmt := range []string{
		`PRAGMA foreign_keys=OFF`,
		`CREATE TABLE runs_new (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			client_id    INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
			started_at   TEXT    NOT NULL,
			finished_at  TEXT    NOT NULL,
			status       TEXT    NOT NULL CHECK (status IN ('ok','failed','overlap','running')),
			bytes        INTEGER NOT NULL DEFAULT 0,
			files        INTEGER NOT NULL DEFAULT 0,
			snapshot_id  TEXT    NOT NULL DEFAULT '',
			log_tail     TEXT    NOT NULL DEFAULT ''
		)`,
		`INSERT INTO runs_new SELECT * FROM runs`,
		`DROP TABLE runs`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("build stranded fixture (%s): %v", firstLine(stmt), err)
		}
	}
}

// The card's whole point: a database holding its run history in a stranded
// `runs_new` still starts, says so once, and does not quietly move the rows.
func TestStrandedRunsNewIsWarnedAboutNotRescued(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crashed.db")
	writeStrandedRunsNewDB(t, path)

	warnings := captureWarnings(t)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("a database with stranded rows must still start: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// ── exactly one line, and it says the three things an operator needs ────
	if len(*warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %d: %q", len(*warnings), *warnings)
	}
	got := (*warnings)[0]
	if !strings.HasPrefix(got, "WARNING: ") {
		t.Errorf("warning is not greppable as WARNING: %q", got)
	}
	for _, want := range []string{`"runs_new"`, "3 row(s)", "does not read"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q: %q", want, got)
		}
	}
	t.Logf("startup warning: %s", got)

	// ── not rescued: the rows are still only in runs_new ────────────────────
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM runs`); n != 0 {
		t.Errorf("runs holds %d rows: the stranded rows were copied into it", n)
	}
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM runs_new`); n != 3 {
		t.Errorf("runs_new holds %d rows, want the 3 it had: it was dropped, renamed or emptied", n)
	}
	runs, err := st.ListRuns(ctx, 1, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("the application reads %d runs, want 0: the warning is supposed to explain an empty history", len(runs))
	}

	// ── the app's own tables came through the migration untouched ───────────
	client, err := st.GetClientBySlug(ctx, "laptop-docs")
	if err != nil || client == nil {
		t.Fatalf("client lookup after the migration: %v (nil=%v)", err, client == nil)
	}
	if client.TokenHash != "argon2-hash" {
		t.Errorf("client row was corrupted: %+v", client)
	}
	objs, err := st.ListOffsiteObjects(ctx, 1)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 1 || objs[0].Bytes != 1000 {
		t.Errorf("offsite history was corrupted: %+v", objs)
	}
}

// The other debris shape: the swap's `<table>__canonical` name, with the live
// table intact. Runs must keep exactly the rows they had — a later change that
// turned the warning into a rescue would fail here.
func TestStrandedCanonicalTableIsWarnedAboutAndRunsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "debris.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	ctx := context.Background()
	clientID, err := st.CreateClient(ctx, model.Client{Name: "laptop-docs", Mode: model.ModeRsync})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i, snap := range []string{"20260101T000000Z", "20260102T000000Z"} {
		if _, err := st.RecordRun(ctx, model.Run{
			ClientID: clientID, Status: model.StatusOK,
			StartedAt: start.AddDate(0, 0, i), FinishedAt: start.AddDate(0, 0, i).Add(5 * time.Minute),
			Bytes: int64(1000 * (i + 1)), Files: int64(10 + i), SnapshotID: snap, LogTail: "log for " + snap,
		}); err != nil {
			t.Fatalf("record run %d: %v", i, err)
		}
	}
	// A half-built copy of the live table, holding three rows: what an interrupted
	// swap would leave if it were not transactional.
	if _, err := st.db.Exec(`CREATE TABLE runs__canonical AS SELECT * FROM runs`); err != nil {
		t.Fatalf("create stray table: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO runs__canonical (client_id, started_at, finished_at, status, snapshot_id)
		VALUES (?, '2026-01-03T00:00:00Z', '2026-01-03T00:05:00Z', 'ok', '20260103T000000Z')`, clientID); err != nil {
		t.Fatalf("insert stray row: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	warnings := captureWarnings(t)
	again, err := Open(path)
	if err != nil {
		t.Fatalf("re-open with a stranded table: %v", err)
	}
	defer func() { _ = again.Close() }()

	if len(*warnings) != 1 {
		t.Fatalf("expected exactly one warning, got %d: %q", len(*warnings), *warnings)
	}
	got := (*warnings)[0]
	for _, want := range []string{`"runs__canonical"`, "3 row(s)", `"runs"`} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not mention %q: %q", want, got)
		}
	}
	t.Logf("startup warning: %s", got)

	// ── the stray table is not renamed, dropped or emptied ──────────────────
	if n := scalarInt(t, again.db, `SELECT COUNT(*) FROM runs__canonical`); n != 3 {
		t.Errorf("runs__canonical holds %d rows, want 3: it was renamed, dropped or emptied", n)
	}

	// ── runs still has exactly the rows it had before the open ──────────────
	runs, err := again.ListRuns(context.Background(), clientID, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs holds %d rows after the open, want the 2 it had (a rescue would show 3): %+v", len(runs), runs)
	}
	if runs[0].SnapshotID != "20260102T000000Z" || runs[0].Bytes != 2000 || runs[0].Files != 11 ||
		runs[1].SnapshotID != "20260101T000000Z" || runs[1].Bytes != 1000 {
		t.Errorf("run rows changed while the stray table was only being reported: %+v", runs)
	}
}

// A clean database — fresh, already migrated, or upgraded from the legacy shape —
// must produce no such line at all: a warning on every start is noise, and noise
// gets filtered out of the log by the operator who needs to see the real one.
func TestCleanDatabaseLogsNoStrandedTableWarning(t *testing.T) {
	warnings := captureWarnings(t)

	fresh := filepath.Join(t.TempDir(), "fresh.db")
	first, err := Open(fresh)
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close fresh db: %v", err)
	}
	// Already migrated: a second start of the same file.
	second, err := Open(fresh)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Upgraded from a database an older backitup wrote.
	legacy := filepath.Join(t.TempDir(), "legacy.db")
	writePreRunningDB(t, legacy)
	third, err := Open(legacy)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(*warnings) != 0 {
		t.Fatalf("a database with no stranded table warned anyway: %q", *warnings)
	}
}

// The check is a diagnostic: a database that opened must not be refused, or left
// half-initialised, because the debris scan itself failed.
func TestStrandedTableCheckCannotFailTheOpen(t *testing.T) {
	warnings := captureWarnings(t)

	var logged strings.Builder
	prevOutput := log.Writer()
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(prevOutput) })

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gone.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Every query on a closed handle fails; the scan must swallow that.
	warnStrandedMigrationTables(context.Background(), db)

	if len(*warnings) != 0 {
		t.Errorf("a failed scan must not report stranded rows it never saw: %q", *warnings)
	}
	if !strings.Contains(logged.String(), "could not check") {
		t.Errorf("a failed scan was silent; the operator would never know the check did not run: %q", logged.String())
	}
}

// Only the two shapes a migration can leave behind count as debris; a table the
// operator created is not reported, because a warning that always fires is not a
// warning.
func TestMigrationDebrisNames(t *testing.T) {
	cases := []struct {
		name string
		live string
		ok   bool
	}{
		{"runs_new", "runs", true},
		{"runs__canonical", "runs", true},
		{"clients__canonical", "clients", true},
		{"__canonical", "", false}, // no base table: not a swap's temporary name
		{"runs", "", false},        // the live table, which schema.sql defines anyway
		{"runs_old", "", false},
		{"operator_notes", "", false},
	}
	for _, c := range cases {
		live, ok := migrationDebrisOf(c.name)
		if ok != c.ok || live != c.live {
			t.Errorf("migrationDebrisOf(%q) = (%q, %v), want (%q, %v)", c.name, live, ok, c.live, c.ok)
		}
	}
}
