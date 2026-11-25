package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/th0rn0/backitup/internal/model"
)

// The fidelity settings are per-client and opt-in. They must survive the store
// round trip on both read paths the server and the client use (ListClients for
// the pages, GetClientBySlug for /api/v1/config), and a client created without
// them must read back as all-off — the shape every existing client has.
func TestClientFidelityRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if _, err := st.CreateClient(ctx, model.Client{
		Name: "vm-host", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
		PreserveSparse: true, PreserveHardlinks: true, PreserveACLs: true, PreserveXattrs: true,
	}); err != nil {
		t.Fatalf("create client with every setting on: %v", err)
	}
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "plain-box", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	}); err != nil {
		t.Fatalf("create client with every setting off: %v", err)
	}

	byName, err := st.GetClientBySlug(ctx, model.Slug("vm-host"))
	if err != nil || byName == nil {
		t.Fatalf("load vm-host: %v", err)
	}
	if !byName.PreserveSparse || !byName.PreserveHardlinks || !byName.PreserveACLs || !byName.PreserveXattrs {
		t.Fatalf("fidelity settings did not survive the round trip: %+v", byName)
	}

	list, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	got := map[string]model.Client{}
	for _, c := range list {
		got[c.Name] = c
	}
	if plain := got["plain-box"]; plain.PreserveSparse || plain.PreserveHardlinks || plain.PreserveACLs || plain.PreserveXattrs {
		t.Fatalf("a client created with no settings reads back with some on: %+v", plain)
	}
	if vm := got["vm-host"]; !vm.PreserveSparse || !vm.PreserveXattrs {
		t.Fatalf("ListClients lost the settings: %+v", vm)
	}

	// The settings form: set them all, then clear them all.
	if err := st.UpdateClientFidelity(ctx, byName.ID, false, false, false, false); err != nil {
		t.Fatalf("clear fidelity: %v", err)
	}
	after, err := st.GetClient(ctx, byName.ID)
	if err != nil || after == nil {
		t.Fatalf("reload after clearing: %v", err)
	}
	if after.PreserveSparse || after.PreserveHardlinks || after.PreserveACLs || after.PreserveXattrs {
		t.Fatalf("clearing the settings left some on: %+v", after)
	}
	if err := st.UpdateClientFidelity(ctx, byName.ID, true, false, true, false); err != nil {
		t.Fatalf("set sparse+acls: %v", err)
	}
	after, err = st.GetClient(ctx, byName.ID)
	if err != nil || after == nil {
		t.Fatalf("reload after setting: %v", err)
	}
	if !after.PreserveSparse || after.PreserveHardlinks || !after.PreserveACLs || after.PreserveXattrs {
		t.Fatalf("the settings form wrote the wrong combination: %+v", after)
	}

	if err := st.UpdateClientFidelity(ctx, 424242, true, true, true, true); err == nil {
		t.Fatal("updating a client that does not exist reported success")
	}
}

// An upgrade must not change what an existing client's snapshots contain: the
// four columns are added defaulting to 0, so a database written by the previous
// build keeps the invocation it was already sending.
func TestV4DatabaseKeepsFidelityOff(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "v4.db")
	writeV4Database(t, dbPath)

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open a user_version 4 database: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := t.Context()

	if v := scalarInt(t, st.db, `PRAGMA user_version`); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	cols := columnNames(tableInfo(t, st.db, "clients"))
	for _, want := range []string{"preserve_sparse", "preserve_hardlinks", "preserve_acls", "preserve_xattrs"} {
		if !contains(cols, want) {
			t.Fatalf("clients.%s was not added by the migration (columns: %v)", want, cols)
		}
	}

	c, err := st.GetClientBySlug(ctx, "legacy-boxy")
	if err != nil || c == nil {
		t.Fatalf("load the pre-existing client: %v", err)
	}
	if c.PreserveSparse || c.PreserveHardlinks || c.PreserveACLs || c.PreserveXattrs {
		t.Fatalf("the upgrade turned a fidelity setting on for an existing client: %+v", c)
	}
	// The rest of the row is untouched by the rebuild.
	if c.Mode != model.ModeRsync || c.RetentionDays != 9 || len(c.Excludes) != 1 || c.Excludes[0] != "*.iso" {
		t.Fatalf("the migration altered the client row: %+v", c)
	}

	// An upgraded database is shaped exactly like a fresh one.
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open fresh db: %v", err)
	}
	defer func() { _ = fresh.Close() }()
	if got, want := cols, columnNames(tableInfo(t, fresh.db, "clients")); !reflect.DeepEqual(got, want) {
		t.Errorf("clients columns after the migration\ngot  %v\nwant %v", got, want)
	}
}

// writeV4Database recreates the database the previous build wrote: today's
// schema.sql with the four fidelity columns taken back out, stamped at
// user_version 4 so only the step that adds them runs.
func writeV4Database(t *testing.T, path string) {
	t.Helper()
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open v4 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, tbl := range sch.tables {
		ddl := tbl.ddl
		if tbl.name == "clients" {
			// The previous build's clients table: today's minus the four
			// columns, with the comma the last of them leaves behind.
			ddl = withoutColumns(t, ddl, "preserve_sparse", "preserve_hardlinks", "preserve_acls", "preserve_xattrs")
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
	if _, err := db.Exec(`INSERT INTO clients
		(name, slug, mode, source_label, excludes, retention_days, enabled, created_at)
		VALUES (?,?,?,?,?,?,?,?)`,
		"legacy-boxy", "legacy-boxy", "rsync", "a box from before the settings", `["*.iso"]`, 9, 1,
		"2026-06-01T00:00:00Z"); err != nil {
		t.Fatalf("insert the pre-existing client: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}

// withoutColumns removes whole column declarations from a CREATE TABLE
// statement and the now-dangling comma before ")", so a fixture can be written
// as today's schema.sql minus the columns a previous build did not have.
// (withoutColumn in migrate_v4_test.go trims the comma off the preceding line,
// which does not work when that line ends in a comment.)
func withoutColumns(t *testing.T, ddl string, columns ...string) string {
	t.Helper()
	drop := make(map[string]bool, len(columns))
	for _, c := range columns {
		drop[c] = true
	}
	var keep []string
	removed := 0
	for _, line := range strings.Split(ddl, "\n") {
		if f := strings.Fields(line); len(f) > 0 && drop[f[0]] {
			removed++
			continue
		}
		keep = append(keep, line)
	}
	if removed != len(columns) {
		t.Fatalf("removed %d of %d columns from:\n%s", removed, len(columns), ddl)
	}
	out := keep
	// The columns that are left may still be followed by the commas of the
	// removed ones (including one before a trailing comment), so drop the
	// dangling comma on the last real column line.
	for i := len(out) - 1; i >= 0; i-- {
		s := strings.TrimSpace(out[i])
		if s == "" || strings.HasPrefix(s, "--") || strings.HasPrefix(s, ")") {
			continue
		}
		out[i] = danglingComma.ReplaceAllString(out[i], "$1")
		break
	}
	return strings.Join(out, "\n")
}

var danglingComma = regexp.MustCompile(`,(\s*--.*|\s*)$`)
