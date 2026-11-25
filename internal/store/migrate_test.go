package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/th0rn0/backitup/internal/model"
)

// legacySchema is schema.sql as shipped in 6472853 — the last database shape
// before runs.status learned about 'running' (60ccff2). A deployment that has been
// running since then still has exactly this: no slug, no token_prefix, no
// offsite_cron, no upload mode, no checksum/dedup/verification columns on
// offsite_objects, and a runs CHECK that rejects a run in progress.
const legacySchema = `
CREATE TABLE IF NOT EXISTS clients (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    name                    TEXT    NOT NULL UNIQUE,
    mode                    TEXT    NOT NULL CHECK (mode IN ('targz','rsync')),
    source_label            TEXT    NOT NULL DEFAULT '',
    excludes                TEXT    NOT NULL DEFAULT '[]',
    retention_days          INTEGER NOT NULL DEFAULT 14,
    offsite_retention_days  INTEGER NOT NULL DEFAULT 90,
    expected_interval_secs  INTEGER NOT NULL DEFAULT 0,
    offsite_remote          TEXT    NOT NULL DEFAULT '',
    ssh_pubkey              TEXT    NOT NULL DEFAULT '',
    token_hash              TEXT    NOT NULL DEFAULT '',
    enabled                 INTEGER NOT NULL DEFAULT 1,
    created_at              TEXT    NOT NULL,
    version                 INTEGER NOT NULL DEFAULT 1,
    skip_symlinks           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id    INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    started_at   TEXT    NOT NULL,
    finished_at  TEXT    NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('ok','failed','overlap')),
    bytes        INTEGER NOT NULL DEFAULT 0,
    files        INTEGER NOT NULL DEFAULT 0,
    snapshot_id  TEXT    NOT NULL DEFAULT '',
    log_tail     TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_runs_client_started
    ON runs (client_id, started_at DESC);

CREATE TABLE IF NOT EXISTS offsite_objects (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id    INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    snapshot_id  TEXT    NOT NULL,
    remote       TEXT    NOT NULL,
    bytes        INTEGER NOT NULL DEFAULT 0,
    uploaded_at  TEXT    NOT NULL,
    UNIQUE (client_id, snapshot_id, remote)
);

CREATE INDEX IF NOT EXISTS idx_offsite_client
    ON offsite_objects (client_id, uploaded_at DESC);

CREATE TABLE IF NOT EXISTS admin (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);
`

// writePreRunningDB creates a database at path with the pre-'running' shape,
// holding one client, three finished runs and one offsite object.
func writePreRunningDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO clients (name, mode, created_at, token_hash)
		VALUES ('laptop-docs', 'rsync', '2026-01-01T00:00:00Z', 'argon2-hash')`); err != nil {
		t.Fatalf("insert legacy client: %v", err)
	}
	for i, snap := range []string{"20260101T000000Z", "20260102T000000Z", "20260103T000000Z"} {
		if _, err := db.Exec(`INSERT INTO runs (client_id, started_at, finished_at, status, bytes, files, snapshot_id, log_tail)
			VALUES (1, ?, ?, 'ok', ?, ?, ?, 'log for ' || ?)`,
			fmt.Sprintf("2026-01-0%dT00:00:00Z", i+1), fmt.Sprintf("2026-01-0%dT00:05:00Z", i+1),
			int64(1000*(i+1)), 10+i, snap, snap); err != nil {
			t.Fatalf("insert legacy run %d: %v", i, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO offsite_objects (client_id, snapshot_id, remote, bytes, uploaded_at)
		VALUES (1, '20260101T000000Z', 's3', 1000, '2026-01-01T01:00:00Z')`); err != nil {
		t.Fatalf("insert legacy offsite object: %v", err)
	}
}

// columnInfo is one row of PRAGMA table_info: everything a positional INSERT or a
// SELECT * depends on.
type columnInfo struct {
	Name    string
	Type    string
	NotNull int
	Default sql.NullString
	PK      int
}

func tableInfo(t *testing.T, db *sql.DB, table string) []columnInfo {
	t.Helper()
	rows, err := db.Query(`SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatalf("table_info %s: %v", table, err)
	}
	defer func() { _ = rows.Close() }()
	var out []columnInfo
	for rows.Next() {
		var c columnInfo
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &c.Default, &c.PK); err != nil {
			t.Fatalf("table_info %s: %v", table, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info %s: %v", table, err)
	}
	return out
}

func columnNames(cols []columnInfo) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return names
}

func indexNames(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("list indexes: %v", err)
		}
		out = append(out, n)
	}
	return out
}

func scalarInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// canonicalTableNames is schema.sql's table list, in file order. Kept explicit so
// that a table silently dropped from the file fails a test instead of shrinking
// what the other tests compare.
var canonicalTableNames = []string{"clients", "runs", "client_storage", "offsite_objects", "remotes", "offsite_runs", "users", "admin"}

// A database from an older backitup is rebuilt to exactly the schema a new one is
// created with: same columns, same column ORDER, same indexes — and no rows lost.
func TestLegacyDatabaseMigratesToTheFreshSchema(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	writePreRunningDB(t, legacyPath)

	st, err := Open(legacyPath)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// ── no data loss ────────────────────────────────────────────────────────
	runs, err := st.ListRuns(ctx, 1, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("migration kept %d runs, want 3", len(runs))
	}
	// Newest first: check the values, not just the count, so a migration that
	// shuffled columns into the wrong fields is caught too.
	if runs[0].SnapshotID != "20260103T000000Z" || runs[0].Bytes != 3000 || runs[0].Files != 12 ||
		runs[0].LogTail != "log for 20260103T000000Z" || runs[0].Status != model.StatusOK {
		t.Fatalf("run row was corrupted by the migration: %+v", runs[0])
	}
	client, err := st.GetClientBySlug(ctx, "laptop-docs")
	if err != nil || client == nil {
		t.Fatalf("client lookup after migration: %v (nil=%v)", err, client == nil)
	}
	if client.Name != "laptop-docs" || client.TokenHash != "argon2-hash" {
		t.Fatalf("client row was corrupted by the migration: %+v", client)
	}
	objs, err := st.ListOffsiteObjects(ctx, 1)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 1 || objs[0].Bytes != 1000 {
		t.Fatalf("offsite history was corrupted by the migration: %+v", objs)
	}

	// ── the whole schema, not just the columns the tests above touch ────────
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	for _, table := range canonicalTableNames {
		got := tableInfo(t, st.db, table)
		want := tableInfo(t, fresh.db, table)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("table %s differs between an upgraded and a fresh database\n upgraded: %s\n fresh   : %s",
				table, describeColumns(got), describeColumns(want))
		}
	}
	// The card's requirement, spelled out: the same names in the same order.
	if got, want := columnNames(tableInfo(t, st.db, "clients")), columnNames(tableInfo(t, fresh.db, "clients")); !reflect.DeepEqual(got, want) {
		t.Errorf("clients column order\ngot  %v\nwant %v", got, want)
	}
	if got, want := indexNames(t, st.db), indexNames(t, fresh.db); !reflect.DeepEqual(got, want) {
		t.Errorf("indexes\ngot  %v\nwant %v", got, want)
	}
	// A fresh database must already be exactly the canonical definition,
	// otherwise open() would rebuild it on every start.
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	for _, tbl := range sch.tables {
		stored, err := storedDDL(ctx, fresh.db, tbl.name)
		if err != nil {
			t.Fatalf("read stored ddl of %s: %v", tbl.name, err)
		}
		if normaliseDDL(stored) != normaliseDDL(strings.Replace(tbl.ddl, "IF NOT EXISTS ", "", 1)) {
			t.Errorf("schema.sql is not what SQLite records for %s\nstored:\n%s\ncanonical:\n%s", tbl.name, stored, tbl.ddl)
		}
	}

	// ── the constraint the old CHECK rejected ───────────────────────────────
	if _, err := st.RecordRun(ctx, model.Run{
		ClientID: 1, Status: model.StatusRunning,
		StartedAt: runs[0].StartedAt, FinishedAt: runs[0].FinishedAt,
	}); err != nil {
		t.Errorf("after migration the runs table still rejects status 'running': %v", err)
	}

	// ── version stamp, and a second Open is a no-op ─────────────────────────
	for name, db := range map[string]*sql.DB{"upgraded": st.db, "fresh": fresh.db} {
		if v := scalarInt(t, db, `PRAGMA user_version`); v != schemaVersion {
			t.Errorf("%s database is at user_version %d, want %d", name, v, schemaVersion)
		}
	}
	schemaCookie := scalarInt(t, st.db, `PRAGMA schema_version`)
	version := scalarInt(t, st.db, `PRAGMA user_version`)
	_ = st.Close()

	var steps []string
	migrationFault = func(point string) error {
		steps = append(steps, point)
		return nil
	}
	t.Cleanup(func() { migrationFault = nil })
	again, err := Open(legacyPath)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	defer func() { _ = again.Close() }()
	if len(steps) != 0 {
		t.Errorf("re-open of an up-to-date database ran migration work: %v", steps)
	}
	if got := scalarInt(t, again.db, `PRAGMA schema_version`); got != schemaCookie {
		t.Errorf("re-open changed the schema (cookie %d -> %d)", schemaCookie, got)
	}
	if got := scalarInt(t, again.db, `PRAGMA user_version`); got != version {
		t.Errorf("re-open changed user_version (%d -> %d)", version, got)
	}
}

func describeColumns(cols []columnInfo) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = fmt.Sprintf("%s %s", c.Name, c.Type)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// An interrupt in the middle of the table swap used to lose the run history: the
// old code ran its statements one by one, so a crash between DROP TABLE runs and
// ALTER TABLE runs_new RENAME TO runs left the rows stranded in runs_new and
// nothing in runs. The step is now one transaction, so the next Open() either
// finds the history untouched or finds the whole migration finished.
func TestInterruptedRebuildKeepsRunHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	writePreRunningDB(t, path)

	// Die exactly where the old code was most destructive: after the original
	// runs table has been dropped and its rows live only in the new table.
	migrationFault = func(point string) error {
		if point == "dropped:runs" {
			return errors.New("simulated crash")
		}
		return nil
	}
	t.Cleanup(func() { migrationFault = nil })

	if _, err := Open(path); err == nil {
		t.Fatal("the interrupted migration reported success")
	}

	// Read the database the way the next process start would find it, without
	// going through Open(): it must be the untouched legacy database.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen raw: %v", err)
	}
	defer func() { _ = db.Close() }()
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM runs`); n != 3 {
		t.Errorf("after the interrupted migration runs holds %d rows, want 3 (the old code lost them here)", n)
	} else {
		t.Logf("after the interrupted rebuild: runs = %d | runs__canonical tables = %d | user_version = %d",
			n,
			scalarInt(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='runs__canonical'`),
			scalarInt(t, db, `PRAGMA user_version`))
	}
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='runs__canonical'`); n != 0 {
		t.Errorf("half-built table runs__canonical survived the interrupted migration")
	}
	if v := scalarInt(t, db, `PRAGMA user_version`); v != 1 {
		// The fault fires inside step 2 (the canonical rebuild, the only step that
		// rebuilds `runs`), so only step 1 may stand: the interrupted step must not
		// have advanced the version, and a version stamped for work that rolled back
		// would make the next start skip it.
		t.Errorf("interrupted migration recorded user_version %d, want 1 (the last completed step)", v)
	}
	// The pre-migration client shape is intact too, index included.
	if n := scalarInt(t, db, `SELECT COUNT(*) FROM pragma_table_info('clients') WHERE name='slug'`); n != 0 {
		t.Errorf("interrupted migration left clients partially rebuilt")
	}

	// The next start finishes the job, with the history still there.
	migrationFault = nil
	_ = db.Close()
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open after the interrupted migration: %v", err)
	}
	defer func() { _ = st.Close() }()
	runs, err := st.ListRuns(context.Background(), 1, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("after the retried migration runs holds %d rows, want 3", len(runs))
	}
	if got := scalarInt(t, st.db, `PRAGMA user_version`); got != schemaVersion {
		t.Errorf("after the retried migration user_version = %d, want %d", got, schemaVersion)
	}
}

// An interrupt between statements of a step other than the runs rebuild is
// equally harmless, and a step that fails leaves the previous version in place so
// the next start retries it.
func TestFailedStepDoesNotAdvanceTheVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	writePreRunningDB(t, path)

	migrationFault = func(point string) error {
		if point == "copied:offsite_objects" {
			return errors.New("simulated crash")
		}
		return nil
	}
	t.Cleanup(func() { migrationFault = nil })
	if _, err := Open(path); err == nil {
		t.Fatal("the interrupted migration reported success")
	}
	migrationFault = nil

	st, err := Open(path)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	defer func() { _ = st.Close() }()
	objs, err := st.ListOffsiteObjects(context.Background(), 1)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 1 {
		t.Fatalf("offsite history lost across the failed migration: got %d rows, want 1", len(objs))
	}
}

// A database that already contains orphaned children (written while foreign_keys
// was off) still starts: the swap proves it did not create any, and reports the
// ones it inherited rather than locking the operator out.
func TestInheritedOrphansAreReportedNotFatal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	writePreRunningDB(t, path)
	// The fixture's connection has foreign_keys off, so this row is accepted:
	// exactly the kind of debris an old build could have left behind.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO runs (client_id, started_at, finished_at, status, snapshot_id)
		VALUES (99, '2026-01-04T00:00:00Z', '2026-01-04T00:05:00Z', 'ok', 'orphan')`); err != nil {
		t.Fatalf("insert orphan run: %v", err)
	}
	_ = db.Close()

	var warnings []string
	prevWarnf := warnf
	warnf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { warnf = prevWarnf })

	st, err := Open(path)
	if err != nil {
		t.Fatalf("an inherited orphan must not stop the server from starting: %v", err)
	}
	defer func() { _ = st.Close() }()
	runs, err := st.ListRuns(context.Background(), 99, 10)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("orphan row was dropped by the migration: got %d rows, want 1", len(runs))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "runs.rowid=") {
		t.Fatalf("expected one warning naming the orphaned row, got %q", warnings)
	}
}

// A database written by a newer backitup is refused, not "upgraded" backwards.
func TestOpenRefusesANewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := st.db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion+1)); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Open(path)
	if err == nil {
		t.Fatal("open accepted a database from a newer backitup")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error does not explain the version mismatch: %v", err)
	}
}

// schema.sql is the only definition of the schema, so the parse that turns it into
// migration input has to be exact.
func TestSchemaFileParsesIntoTheCanonicalTables(t *testing.T) {
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	var names []string
	for _, tbl := range sch.tables {
		names = append(names, tbl.name)
	}
	if !reflect.DeepEqual(names, canonicalTableNames) {
		t.Errorf("schema.sql defines tables %v, want %v", names, canonicalTableNames)
	}
	for _, tbl := range sch.tables {
		if !strings.HasPrefix(tbl.ddl, "CREATE TABLE IF NOT EXISTS "+tbl.name+" (") {
			t.Errorf("table %s: statement does not start with a plain CREATE TABLE IF NOT EXISTS:\n%s", tbl.name, firstLine(tbl.ddl))
		}
		if strings.Contains(tbl.ddl, ";\n") || strings.HasSuffix(tbl.ddl, ";") {
			t.Errorf("table %s: statement was split in the wrong place", tbl.name)
		}
	}
	if len(sch.indexes) != 4 {
		t.Errorf("parsed %d index statements, want 4: %v", len(sch.indexes), sch.indexes)
	}
	for _, idx := range sch.indexes {
		if !strings.Contains(idx, "idx_") {
			t.Errorf("index statement looks wrong: %q", idx)
		}
	}
}

func TestSplitSQL(t *testing.T) {
	script := `
-- a leading comment, with a semicolon; inside it
CREATE TABLE a (s TEXT NOT NULL DEFAULT 'x;y');  -- trailing comment
CREATE INDEX i ON a (s);
`
	got := splitSQL(script)
	if len(got) != 2 {
		t.Fatalf("split into %d statements, want 2: %q", len(got), got)
	}
	if !strings.HasPrefix(got[0], "CREATE TABLE a (") || !strings.Contains(got[0], `'x;y'`) {
		t.Errorf("first statement mangled: %q", got[0])
	}
	if strings.Contains(got[0], "leading comment") {
		t.Errorf("leading comment kept: %q", got[0])
	}
}

func TestNormaliseDDL(t *testing.T) {
	a := "CREATE TABLE t (\n    s TEXT NOT NULL DEFAULT 'a  b'   -- keep these spaces\n);"
	b := "CREATE TABLE t ( s TEXT NOT NULL DEFAULT 'a  b' );"
	if normaliseDDL(a) != normaliseDDL(b) {
		t.Errorf("formatting and comments should not matter:\n%q\n%q", normaliseDDL(a), normaliseDDL(b))
	}
	c := "CREATE TABLE t ( s TEXT NOT NULL DEFAULT 'a b' );"
	if normaliseDDL(a) == normaliseDDL(c) {
		t.Error("whitespace inside a string literal was collapsed")
	}
	if normaliseDDL("CREATE TABLE t (a INT)") == normaliseDDL("CREATE TABLE t (a INT, b INT)") {
		t.Error("a different column list compared equal")
	}
}

func TestSchemaVersionMatchesLastStep(t *testing.T) {
	if len(migrations) == 0 {
		t.Fatal("no migrations")
	}
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("migration %d has version %d; steps must be numbered from 1 without gaps", i, m.version)
		}
	}
	if last := migrations[len(migrations)-1].version; last != schemaVersion {
		t.Errorf("schemaVersion is %d but the last migration is %d", schemaVersion, last)
	}
}
