package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/th0rn0/backitup/internal/model"
)

func openSlugTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

// A client's storage slug is persisted on create, so every later read (and every
// jail path built from it) uses one stable value instead of re-deriving it.
func TestCreateClientPersistsSlug(t *testing.T) {
	st, _ := openSlugTestStore(t)
	ctx := context.Background()

	id, err := st.CreateClient(ctx, model.Client{Name: "Laptop Docs", Mode: model.ModeTarGz, Enabled: true})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	want := model.Slug("Laptop Docs")
	if want == "" {
		t.Fatal("test premise broken: Slug produced an empty slug")
	}

	got, err := st.GetClient(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get client: %v (nil=%v)", err, got == nil)
	}
	if got.Slug != want {
		t.Fatalf("persisted slug = %q, want %q", got.Slug, want)
	}

	bySlug, err := st.GetClientBySlug(ctx, want)
	if err != nil || bySlug == nil {
		t.Fatalf("get by slug: %v (nil=%v)", err, bySlug == nil)
	}
	if bySlug.ID != id {
		t.Fatalf("GetClientBySlug returned id %d, want %d", bySlug.ID, id)
	}
}

// S2/S3: two names that differ only in punctuation, spacing or case collapse to
// one slug — and one slug is one read-write jail. The second client must be
// refused at creation rather than silently sharing the first one's directory.
func TestCreateClientRejectsSlugCollision(t *testing.T) {
	st, _ := openSlugTestStore(t)
	ctx := context.Background()

	first, err := st.CreateClient(ctx, model.Client{Name: "dup a", Mode: model.ModeRsync, Enabled: true})
	if err != nil {
		t.Fatalf("create first client: %v", err)
	}

	for _, name := range []string{"dup-a", "Dup_A", "dup/a", "DUP  A"} {
		if _, err := st.CreateClient(ctx, model.Client{Name: name, Mode: model.ModeRsync, Enabled: true}); err == nil {
			t.Fatalf("client %q created despite slug collision with \"dup a\"", name)
		} else if !strings.Contains(err.Error(), "dup-a") {
			t.Fatalf("error for %q does not name the conflicting slug: %v", name, err)
		}
	}

	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 1 || clients[0].ID != first {
		t.Fatalf("expected only the original client, got %+v", clients)
	}
}

// A name made entirely of stripped characters yields an empty slug, which would
// point the jail at the shared base directory. Creation must reject it.
func TestCreateClientRejectsEmptySlug(t *testing.T) {
	st, _ := openSlugTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"..", ".", "/", "....", "  ", "---", "//..//"} {
		if _, err := st.CreateClient(ctx, model.Client{Name: name, Mode: model.ModeTarGz, Enabled: true}); err == nil {
			t.Fatalf("client named %q created with an empty slug", name)
		}
	}

	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("expected no clients, got %+v", clients)
	}
}

// legacyClientsDDL is the clients table as shipped in e9708bc (no slug column).
const legacyClientsDDL = `
CREATE TABLE clients (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    name                    TEXT    NOT NULL UNIQUE,
    mode                    TEXT    NOT NULL CHECK (mode IN ('targz','rsync')),
    source_label            TEXT    NOT NULL DEFAULT '',
    excludes                TEXT    NOT NULL DEFAULT '[]',
    retention_days          INTEGER NOT NULL DEFAULT 14,
    offsite_retention_days  INTEGER NOT NULL DEFAULT 90,
    expected_interval_secs  INTEGER NOT NULL DEFAULT 0,
    offsite_remote          TEXT    NOT NULL DEFAULT '',
    offsite_dir             TEXT    NOT NULL DEFAULT '',
    offsite_interval_secs   INTEGER NOT NULL DEFAULT 0,
    ssh_pubkey              TEXT    NOT NULL DEFAULT '',
    token_hash              TEXT    NOT NULL DEFAULT '',
    enabled                 INTEGER NOT NULL DEFAULT 1,
    created_at              TEXT    NOT NULL,
    version                 INTEGER NOT NULL DEFAULT 1,
    skip_symlinks           INTEGER NOT NULL DEFAULT 0
)`

// writeLegacyDB creates a pre-slug database at path holding names.
func writeLegacyDB(t *testing.T, path string, names []string) {
	t.Helper()
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(legacyClientsDDL); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	for _, n := range names {
		if _, err := legacy.Exec(
			`INSERT INTO clients (name, mode, created_at) VALUES (?, 'rsync', '2026-01-01T00:00:00Z')`, n); err != nil {
			t.Fatalf("insert legacy row %q: %v", n, err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}
}

// Legacy clients table as shipped in e9708bc (no slug column): the additive
// migration must add it, derive a value per row from the name, and leave no two
// rows sharing a slug (which the unique index then enforces).
func TestOpenBackfillsSlugForLegacyRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	names := []string{"dup a", "dup-a", "..", "Laptop Docs", "photos"}
	writeLegacyDB(t, path, names)

	// The migration separates future writes but leaves the shared directory
	// alone, so it must say so: capture what the operator would see on startup.
	var warnings []string
	prevWarnf := warnf
	warnf = func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() { warnf = prevWarnf })

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != len(names) {
		t.Fatalf("migration lost rows: got %d, want %d", len(clients), len(names))
	}

	seen := map[string]string{}
	for _, c := range clients {
		if c.Slug == "" {
			t.Errorf("client %q migrated with an empty slug", c.Name)
			continue
		}
		if prev, dup := seen[c.Slug]; dup {
			t.Errorf("slug %q shared by %q and %q after migration", c.Slug, prev, c.Name)
		}
		seen[c.Slug] = c.Name
	}
	// The oldest row keeps the plain slug; the colliding row is disambiguated.
	if got := seen["dup-a"]; got != "dup a" {
		t.Errorf("slug \"dup-a\" belongs to %q, want the oldest client \"dup a\"", got)
	}
	if got := seen["laptop-docs"]; got != "Laptop Docs" {
		t.Errorf("slug \"laptop-docs\" belongs to %q, want \"Laptop Docs\"", got)
	}
	if seen["client-3"] != ".." {
		t.Errorf("unsafe name \"..\" was not given a deterministic fallback slug: %v", seen)
	}

	// Exactly one collision ("dup a" vs "dup-a"), so exactly one warning — it
	// must name both clients, both slugs and the directory they still share.
	if len(warnings) != 1 {
		t.Fatalf("expected exactly one legacy-collision warning, got %d: %q", len(warnings), warnings)
	}
	msg := warnings[0]
	for _, want := range []string{
		"WARNING",
		`clients 1 ("dup a") and 2 ("dup-a")`,
		`both mapped to storage slug "dup-a"`,
		`Client 2 ("dup-a") was migrated to slug "dup-a-2"`,
		"<backup dir>/dup-a",
		"remote_missing",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("collision warning does not mention %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, `".."`) {
		t.Errorf("the slugless-fallback row was reported as a collision:\n%s", msg)
	}

	// The unique index must now be enforced by the database itself.
	if _, err := st.db.Exec(
		`INSERT INTO clients (name, slug, mode, created_at) VALUES ('intruder', 'dup-a', 'rsync', '2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("database accepted a duplicate slug: the unique index is missing")
	}

	// Re-opening a migrated database must be a no-op, not a second backfill or a
	// failing index creation — and must not repeat the warning (it is startup
	// noise after the operator has already been told once).
	_ = st.Close()
	warnings = nil
	again, err := Open(path)
	if err != nil {
		t.Fatalf("re-open migrated db: %v", err)
	}
	defer func() { _ = again.Close() }()
	if len(warnings) != 0 {
		t.Errorf("re-open warned about collisions again: %q", warnings)
	}
	clients2, err := again.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients after re-open: %v", err)
	}
	if len(clients2) != len(names) {
		t.Fatalf("re-open changed the row count: %d, want %d", len(clients2), len(names))
	}
	for i := range clients {
		if clients[i].Slug != clients2[i].Slug {
			t.Errorf("re-open changed %q's slug from %q to %q", clients[i].Name, clients[i].Slug, clients2[i].Slug)
		}
	}
}

// A legacy name longer than a slug may be must still migrate to a slug the
// authkeys allowlist accepts — including after collision disambiguation makes it
// longer — otherwise that client silently loses its SSH key at startup.
func TestBackfillKeepsLegacySlugsWithinAllowlist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	long := strings.Repeat("a", 80)
	// Both truncate to the same 63-character stem, so the second needs a suffix.
	writeLegacyDB(t, path, []string{long, long[:70] + "b"})

	prevWarnf := warnf
	var warnings int
	warnf = func(string, ...any) { warnings++ }
	t.Cleanup(func() { warnf = prevWarnf })

	st, err := Open(path)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	defer func() { _ = st.Close() }()

	clients, err := st.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("got %d clients, want 2", len(clients))
	}
	shape := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	for _, c := range clients {
		if !shape.MatchString(c.Slug) {
			t.Errorf("migrated slug %q (len %d) for %q would be refused by authkeys", c.Slug, len(c.Slug), c.Name)
		}
	}
	if warnings != 1 {
		t.Errorf("expected one collision warning for the truncation clash, got %d", warnings)
	}
}
