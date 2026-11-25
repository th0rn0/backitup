package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// group_readable is stored per client and defaults to off, which is what keeps a
// client that existed before the setting producing the snapshot it always did.
func TestClientGroupReadableRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if _, err := st.CreateClient(ctx, model.Client{
		Name: "default-off", Mode: model.ModeRsync, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA a", TokenHash: "h", TokenPrefix: "aaaa",
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	c, err := st.GetClientBySlug(ctx, "default-off")
	if err != nil || c == nil {
		t.Fatalf("load client: %v (client=%v)", err, c)
	}
	if c.GroupReadable {
		t.Fatalf("a client created without the setting has it on: %+v", c)
	}

	if err := st.UpdateClientGroupReadable(ctx, c.ID, true); err != nil {
		t.Fatalf("turn it on: %v", err)
	}
	c, err = st.GetClientBySlug(ctx, "default-off")
	if err != nil || c == nil {
		t.Fatalf("reload client: %v", err)
	}
	if !c.GroupReadable {
		t.Fatal("the setting did not persist")
	}

	if err := st.UpdateClientGroupReadable(ctx, c.ID, false); err != nil {
		t.Fatalf("turn it off: %v", err)
	}
	c, err = st.GetClientBySlug(ctx, "default-off")
	if err != nil || c == nil {
		t.Fatalf("reload client: %v", err)
	}
	if c.GroupReadable {
		t.Fatal("the setting did not clear")
	}

	// A client that does not exist is an error, not a silent no-op.
	if err := st.UpdateClientGroupReadable(ctx, c.ID+999, true); err == nil {
		t.Error("updating a missing client reported success")
	}
}

// The migration path: a database at the schema of the previous release reads the
// new column as 0 for every existing row.
func TestGroupReadableDefaultsOffOnUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	writeV5DB(t, path)

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open a v5 database: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if v := scalarInt(t, st.db, `PRAGMA user_version`); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("the upgrade left %d clients, want 1", len(clients))
	}
	if clients[0].GroupReadable {
		t.Error("an existing client was upgraded with group-readable snapshots on")
	}
}

// writeV5DB builds a database at the previous release's shape: today's
// schema.sql with clients.group_readable taken away, one client row, stamped
// user_version 5. That is the state an operator's store is in when they upgrade
// to the build that has the setting.
func writeV5DB(t *testing.T, path string) {
	t.Helper()
	sch, err := parseSchema(schemaSQL)
	if err != nil {
		t.Fatalf("parse schema.sql: %v", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open v5 db: %v", err)
	}
	defer func() { _ = db.Close() }()

	for _, tbl := range sch.tables {
		ddl := tbl.ddl
		if tbl.name == "clients" {
			ddl = withoutColumn(t, ddl, "group_readable")
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
	if _, err := db.Exec(`INSERT INTO clients (name, slug, mode, retention_days, expected_interval_secs,
		offsite_remote, offsite_dir, offsite_cron, ssh_pubkey, token_hash, token_prefix, enabled, created_at,
		skip_symlinks, offsite_upload_mode, preserve_sparse, preserve_hardlinks, preserve_acls, preserve_xattrs)
		VALUES ('laptop-docs', 'laptop-docs', 'rsync', 14, 0, '', '', '', 'ssh-ed25519 AAAA x', 'hash', 'abcd',
		1, '2026-01-01T00:00:00Z', 0, 'all', 1, 0, 1, 0)`); err != nil {
		t.Fatalf("insert v5 client: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
}
