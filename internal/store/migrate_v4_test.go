package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/th0rn0/backitup/internal/model"
)

// A database written before offsite storage became B2-only carries remotes of
// backends this build no longer supports (s3, drive, sftp, …). The migration
// must leave those rows in place and mark them unsupported: deleting an
// operator's configured remote would be indistinguishable from a bug that lost
// it, and leaving it unmarked would keep a provider this build cannot honour in
// service.
func TestV3DatabaseMarksUnsupportedRemoteBackends(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v3.db")
	writeV3Database(t, dbPath)

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open a user_version 3 database: %v", err)
	}
	ctx := t.Context()

	if v := scalarInt(t, st.db, `PRAGMA user_version`); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	if !contains(columnNames(tableInfo(t, st.db, "remotes")), "unsupported") {
		t.Fatal("remotes.unsupported was not added by the migration")
	}

	want := map[string]bool{"b2-bucket": false, "encrypted": false, "old-s3": true, "old-drive": true}
	remotes, err := st.ListRemotes(ctx)
	if err != nil {
		t.Fatalf("list remotes: %v", err)
	}
	if len(remotes) != len(want) {
		t.Fatalf("after the migration there are %d remotes, want %d (none may be deleted): %+v",
			len(remotes), len(want), remotes)
	}
	for _, r := range remotes {
		unsupported, ok := want[r.Name]
		if !ok {
			t.Fatalf("unexpected remote %q survived the migration", r.Name)
		}
		if r.Unsupported != unsupported {
			t.Errorf("remote %q (backend %q): unsupported = %v, want %v",
				r.Name, r.Backend, r.Unsupported, unsupported)
		}
	}

	// The config of an unsupported remote is untouched: the operator still sees
	// what they configured (and what to recreate as B2).
	s3, err := st.GetRemoteByName(ctx, "old-s3")
	if err != nil || s3 == nil {
		t.Fatalf("load old-s3: %v (remote=%v)", err, s3)
	}
	if s3.Backend != "s3" || s3.Config["bucket"] != "s3-bucket" || s3.Config["region"] != "eu-west-2" {
		t.Fatalf("the migration altered the unsupported remote: %+v", s3)
	}

	// A remote created by this build is never marked unsupported, and a second
	// Open must not run a step again.
	if err := st.CreateRemote(ctx, model.Remote{
		Name: "fresh-b2", Backend: model.BackendB2,
		Config:    map[string]string{"account": "keyid", "key": "appkey", "bucket": "my-backups"},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create remote after migration: %v", err)
	}
	fresh, err := st.GetRemoteByName(ctx, "fresh-b2")
	if err != nil || fresh == nil {
		t.Fatalf("load fresh-b2: %v (remote=%v)", err, fresh)
	}
	if fresh.Unsupported {
		t.Fatal("a remote created after the migration is marked unsupported")
	}

	schemaCookie := scalarInt(t, st.db, `PRAGMA schema_version`)
	version := scalarInt(t, st.db, `PRAGMA user_version`)
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
	if got := scalarInt(t, again.db, `PRAGMA user_version`); got != version {
		t.Errorf("re-open changed user_version (%d -> %d)", version, got)
	}

	// An upgraded database is shaped exactly like a fresh one.
	freshDB, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	defer func() { _ = freshDB.Close() }()
	if got, wantCols := columnNames(tableInfo(t, again.db, "remotes")), columnNames(tableInfo(t, freshDB.db, "remotes")); !reflect.DeepEqual(got, wantCols) {
		t.Errorf("remotes columns after the migration\ngot  %v\nwant %v", got, wantCols)
	}
}

// writeV3Database recreates the schema the previous build wrote: today's
// schema.sql with remotes.unsupported taken back out, stamped at user_version 3
// so only the step that adds it runs.
func writeV3Database(t *testing.T, path string) {
	t.Helper()
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open v3 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, tbl := range sch.tables {
		ddl := tbl.ddl
		if tbl.name == "remotes" {
			ddl = withoutColumn(t, ddl, "unsupported")
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
	for _, r := range []struct{ name, backend, config string }{
		{"b2-bucket", "b2", `{"account":"keyid","key":"appkey","bucket":"my-backups"}`},
		{"encrypted", "crypt", `{"remote":"b2-bucket","password":"obscured","password2":"obscured"}`},
		{"old-s3", "s3", `{"access_key_id":"AKIA","secret_access_key":"secret","region":"eu-west-2","bucket":"s3-bucket"}`},
		{"old-drive", "drive", `{"service_account_credentials":"{}","team_drive":"0ABC"}`},
	} {
		if _, err := db.Exec(`INSERT INTO remotes (name, backend, config, created_at) VALUES (?,?,?,?)`,
			r.name, r.backend, r.config, createdAt); err != nil {
			t.Fatalf("insert remote %s: %v", r.name, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

// withoutColumn removes one column declaration from a CREATE TABLE statement,
// and the comma that separated it from the column before it, so a fixture can
// be written as today's schema.sql with a single column taken away rather than a
// hand-typed copy of an old table.
func withoutColumn(t *testing.T, ddl, column string) string {
	t.Helper()
	lines := strings.Split(ddl, "\n")
	out := make([]string, 0, len(lines))
	removed := false
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), column+" ") {
			removed = true
			for i := len(out) - 1; i >= 0; i-- {
				trimmed := strings.TrimSpace(out[i])
				if trimmed == "" || strings.HasPrefix(trimmed, "--") {
					continue
				}
				out[i] = strings.TrimSuffix(strings.TrimRight(out[i], " \t"), ",")
				break
			}
			continue
		}
		out = append(out, line)
	}
	if !removed {
		t.Fatalf("could not remove column %q from:\n%s", column, ddl)
	}
	return strings.Join(out, "\n")
}
