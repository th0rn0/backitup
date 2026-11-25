package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// clientWithStore creates one client and returns its id.
func clientWithStore(t *testing.T, st *Store, name string) int64 {
	t.Helper()
	id, err := st.CreateClient(t.Context(), model.Client{
		Name: name, Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client %q: %v", name, err)
	}
	return id
}

// TestClientStorageRoundTrip: the measurement is one row per client that is
// replaced on every lifecycle pass, and a client with no row must read as "never
// measured" (nil), not as a zero.
func TestClientStorageRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "storage.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := t.Context()
	id := clientWithStore(t, st, "vm-host")

	if got, err := st.GetClientStorage(ctx, id); err != nil || got != nil {
		t.Fatalf("a never-measured client reads as (%+v, %v), want (nil, nil)", got, err)
	}

	first := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	if err := st.SetClientStorage(ctx, id, 5<<20, false, first); err != nil {
		t.Fatalf("record measurement: %v", err)
	}
	got, err := st.GetClientStorage(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("load measurement: %v (got %+v)", err, got)
	}
	if got.Bytes != 5<<20 || got.Partial || !got.MeasuredAt.Equal(first) {
		t.Fatalf("measurement = %+v, want {5 MiB, exact, %v}", got, first)
	}

	// A second pass replaces the row rather than appending history.
	second := first.Add(5 * time.Minute)
	if err := st.SetClientStorage(ctx, id, 9<<20, true, second); err != nil {
		t.Fatalf("record second measurement: %v", err)
	}
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM client_storage`); n != 1 {
		t.Fatalf("client_storage holds %d rows after two passes, want 1", n)
	}
	got, err = st.GetClientStorage(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("load second measurement: %v (got %+v)", err, got)
	}
	if got.Bytes != 9<<20 || !got.Partial || !got.MeasuredAt.Equal(second) {
		t.Fatalf("measurement = %+v, want {9 MiB, lower bound, %v}", got, second)
	}

	all, err := st.ListClientStorage(ctx)
	if err != nil {
		t.Fatalf("list measurements: %v", err)
	}
	if got, ok := all[id]; !ok || got.Bytes != 9<<20 || !got.Partial {
		t.Fatalf("ListClientStorage = %+v, want the client's row present and current", all)
	}

	// Deleting the client takes its measurement with it: a row about a client
	// that no longer exists would keep a stale figure in the fleet total.
	if err := st.DeleteClient(ctx, id); err != nil {
		t.Fatalf("delete client: %v", err)
	}
	if n := scalarInt(t, st.db, `SELECT COUNT(*) FROM client_storage`); n != 0 {
		t.Fatalf("client_storage kept %d row(s) for a deleted client", n)
	}
}

// TestV6DatabaseGainsClientStorage: an operator's existing database is at
// user_version 6 and has no client_storage table. The migration that adds it must
// create the table, not just canonicalise the ones that are already there — a
// table added after v1 has no "columns in common" to rebuild from.
func TestV6DatabaseGainsClientStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// Make it a v6 database: the only difference between v6 and v7 is this table.
	if _, err := st.db.Exec(`DROP TABLE client_storage`); err != nil {
		t.Fatalf("drop client_storage: %v", err)
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 6`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("open a user_version 6 database: %v", err)
	}
	defer func() { _ = upgraded.Close() }()
	if v := scalarInt(t, upgraded.db, `PRAGMA user_version`); v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}
	if n := scalarInt(t, upgraded.db, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='client_storage'`); n != 1 {
		t.Fatal("the migration did not create client_storage")
	}
	cols := columnNames(tableInfo(t, upgraded.db, "client_storage"))
	for _, want := range []string{"client_id", "disk_bytes", "disk_partial", "measured_at"} {
		if !contains(cols, want) {
			t.Fatalf("client_storage columns = %v, want %q among them", cols, want)
		}
	}
	// The table is usable, and a second open is a no-op.
	ctx := t.Context()
	id := clientWithStore(t, upgraded, "after-upgrade")
	if err := upgraded.SetClientStorage(ctx, id, 1<<20, false, time.Now()); err != nil {
		t.Fatalf("record measurement after upgrade: %v", err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatalf("close upgraded: %v", err)
	}
	again, err := Open(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer func() { _ = again.Close() }()
	if v := scalarInt(t, again.db, `PRAGMA user_version`); v != schemaVersion {
		t.Errorf("after re-open user_version = %d, want %d", v, schemaVersion)
	}
	got, err := again.GetClientStorage(ctx, id)
	if err != nil || got == nil || got.Bytes != 1<<20 {
		t.Fatalf("measurement did not survive a re-open: %+v (%v)", got, err)
	}
}
