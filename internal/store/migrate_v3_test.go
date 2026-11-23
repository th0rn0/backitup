package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/th0rn0/backitup/internal/model"
)

// A database written by the previous build (user_version 2) carries
// clients.offsite_retention_days and clients.disable_offsite_pruning. Offsite
// retention is now the B2 bucket's lifecycle rule, so the migration drops both
// columns — and the data around them must survive: the client row, its other
// settings, its runs and its offsite records.
func TestV2DatabaseDropsOffsiteRetentionColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v2.db")
	writeV2Database(t, dbPath)

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open a user_version 2 database: %v", err)
	}
	ctx := context.Background()

	cols := columnNames(tableInfo(t, st.db, "clients"))
	for _, gone := range []string{"offsite_retention_days", "disable_offsite_pruning"} {
		if contains(cols, gone) {
			t.Errorf("clients.%s still exists after the migration: %v", gone, cols)
		}
	}
	if v := scalarInt(t, st.db, `PRAGMA user_version`); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// The row and everything that is not the dropped setting are intact, and the
	// client is readable through the ordinary path (which no longer scans them).
	c, err := st.GetClientBySlug(ctx, "laptop-docs")
	if err != nil {
		t.Fatalf("get client after migration: %v", err)
	}
	if c == nil {
		t.Fatal("the client row did not survive the migration")
	}
	if c.Name != "laptop-docs" || c.RetentionDays != 21 || c.OffsiteRemote != "b2bucket" ||
		c.TokenHash != "argon2-hash" || !c.Enabled {
		t.Fatalf("client row was corrupted by the migration: %+v", c)
	}
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM runs WHERE client_id = 1`); n != 1 {
		t.Errorf("runs rows after migration = %d, want 1", n)
	}
	objs, err := st.ListOffsiteObjects(ctx, 1)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 1 || objs[0].SnapshotID != "backup-20260610T000000Z-000.tar.gz" || objs[0].Bytes != 1000 {
		t.Fatalf("offsite records were corrupted by the migration: %+v", objs)
	}

	// Running the server against it: a write that used to touch the dropped
	// columns still works, and the schema is now indistinguishable from a fresh one.
	if err := st.UpdateClientRetention(ctx, c.ID, 30, 86400); err != nil {
		t.Fatalf("update retention after migration: %v", err)
	}
	if _, err := st.RecordRun(ctx, model.Run{
		ClientID: c.ID, Status: model.StatusOK,
		StartedAt: objs[0].UploadedAt, FinishedAt: objs[0].UploadedAt,
	}); err != nil {
		t.Fatalf("record run after migration: %v", err)
	}
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	if got, want := columnNames(tableInfo(t, st.db, "clients")), columnNames(tableInfo(t, fresh.db, "clients")); !reflect.DeepEqual(got, want) {
		t.Errorf("clients columns after the drop\ngot  %v\nwant %v", got, want)
	}

	// A second Open must not run a step again.
	schemaCookie := scalarInt(t, st.db, `PRAGMA schema_version`)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	var steps []string
	migrationFault = func(point string) error {
		steps = append(steps, point)
		return nil
	}
	t.Cleanup(func() { migrationFault = nil })
	again, err := Open(dbPath)
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
}

// writeV2Database recreates the schema the previous build wrote: today's
// schema.sql with the two offsite retention columns still on clients, stamped at
// user_version 2 so the migration runs exactly the one step that drops them.
func writeV2Database(t *testing.T, path string) {
	t.Helper()
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open v2 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, tbl := range sch.tables {
		ddl := tbl.ddl
		if tbl.name == "clients" {
			ddl = strings.Replace(ddl, "    expected_interval_secs",
				"    offsite_retention_days  INTEGER NOT NULL DEFAULT 90,\n    expected_interval_secs", 1)
			ddl = strings.Replace(ddl, "offsite_upload_mode     TEXT    NOT NULL DEFAULT 'all'",
				"offsite_upload_mode     TEXT    NOT NULL DEFAULT 'all',", 1)
			ddl = strings.Replace(ddl, "which snapshots to push\n)",
				"which snapshots to push\n    disable_offsite_pruning INTEGER NOT NULL DEFAULT 0   -- keep the operator's remote objects in place\n)", 1)
			if !strings.Contains(ddl, "offsite_retention_days") || !strings.Contains(ddl, "disable_offsite_pruning INTEGER") {
				t.Fatalf("could not rebuild the user_version 2 clients table from schema.sql:\n%s", ddl)
			}
		}
		if _, err := db.Exec(ddl); err != nil {
			t.Fatalf("create table %s: %v", tbl.name, err)
		}
	}
	for _, idx := range sch.indexes {
		if _, err := db.Exec(idx); err != nil {
			t.Fatalf("create index: %v", err)
		}
	}

	const createdAt = "2026-06-01T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO clients
		(name, slug, mode, source_label, excludes, retention_days, offsite_retention_days,
		 expected_interval_secs, offsite_remote, offsite_dir, offsite_interval_secs, offsite_cron,
		 ssh_pubkey, token_hash, token_prefix, enabled, created_at, version, skip_symlinks,
		 offsite_upload_mode, disable_offsite_pruning)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"laptop-docs", "laptop-docs", "targz", "~/Documents", "[]", 21, 365,
		0, "b2bucket", "", 0, "",
		"ssh-ed25519 AAAA", "argon2-hash", "tok12345", 1, createdAt, 1, 0,
		"all", 1); err != nil {
		t.Fatalf("insert client: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO runs
		(client_id, started_at, finished_at, status, bytes, files, snapshot_id, log_tail)
		VALUES (1,?,?,?,?,?,?,?)`,
		createdAt, createdAt, "ok", 4096, 12, "backup-20260610T000000Z-000.tar.gz", ""); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO offsite_objects
		(client_id, snapshot_id, remote, bytes, uploaded_at)
		VALUES (1,?,?,?,?)`,
		"backup-20260610T000000Z-000.tar.gz", "b2bucket", 1000, createdAt); err != nil {
		t.Fatalf("insert offsite object: %v", err)
	}
	// Exactly the version the previous build stamped, so only step 3 runs.
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}
