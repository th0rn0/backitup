package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"

	// register the server-side modes, as cmd/server does
	_ "github.com/th0rn0/backitup/internal/mode/rsync"
	_ "github.com/th0rn0/backitup/internal/mode/targz"
)

// newDiskStack builds a Server over its own temp backup directory so a test can
// point backupBaseDir somewhere else afterwards and prove a page did not look.
func newDiskStack(t *testing.T) (*store.Store, *Server, *httptest.Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "disk.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	srv.dbTimeout = testDBTimeout
	base := filepath.Join(t.TempDir(), "backups")
	srv.backupBaseDir = base
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, srv, ts, base
}

// createClientViaForm creates a client the way an operator does, so the page
// under test is reached through a real slug and a real storage directory.
func createClientViaForm(t *testing.T, c *http.Client, ts *httptest.Server, st *store.Store, name, mode string) *model.Client {
	t.Helper()
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {name}, "mode": {mode}})
	cl, err := st.GetClientBySlug(t.Context(), model.Slug(name))
	if err != nil || cl == nil {
		t.Fatalf("load created client %q: %v (client=%v)", name, err, cl)
	}
	return cl
}

// writeSnapshot plants a snapshot directory with one file, as rsync would leave
// it (the page's snapshot list is real, only the size is stored).
func writeSnapshot(t *testing.T, base, slug, id, file string, size int) {
	t.Helper()
	dir := filepath.Join(base, slug, "snapshots", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(id, filepath.Join(base, slug, "snapshots", "latest")); err != nil && !os.IsExist(err) {
		t.Fatalf("link latest: %v", err)
	}
}

func statCard(t *testing.T, body, label string) string {
	t.Helper()
	needle := ">" + label
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("page has no %q stat label:\n%s", label, body)
	}
	card := body[i:]
	if end := strings.Index(card, "client-stat\">"); end > 0 {
		card = card[:end]
	}
	if len(card) > 800 {
		card = card[:800]
	}
	return card
}

// TestClientPageShowsTheMeasuredStoreNotASumOfSnapshotBytes is the defect: an
// rsync client with snapshots rendered "0 B" because the page summed
// Snapshot.Bytes, which is zero by design for that mode. The figure must come
// from the stored measurement — and must be the stored number even when the tree
// on disk would say something else, because a request must never walk it.
func TestClientPageShowsTheMeasuredStoreNotASumOfSnapshotBytes(t *testing.T) {
	st, _, ts, base := newDiskStack(t)
	c := loggedInClient(t, st, ts)
	cl := createClientViaForm(t, c, ts, st, "vm-host", "rsync")

	// A real, small tree: two snapshots, ~1 MiB together.
	writeSnapshot(t, base, cl.Slug, "20260617T120000Z", "payload.bin", 512*1024)
	writeSnapshot(t, base, cl.Slug, "20260618T120000Z", "payload.bin", 512*1024)

	measured := int64(800 * 1024 * 1024) // 800 MB: not what the tree holds
	if err := st.SetClientStorage(t.Context(), cl.ID, measured, false, time.Now().Add(-3*time.Minute)); err != nil {
		t.Fatalf("store measurement: %v", err)
	}

	body := getPage(t, c, ts.URL+"/clients/"+cl.Slug)
	card := statCard(t, body, "On disk (allocated)")
	if !strings.Contains(card, "800.0 MB") {
		t.Fatalf("the page did not render the stored measurement (%d bytes):\n%s", measured, card)
	}
	if strings.Contains(card, "0 B") {
		t.Fatalf("an rsync client with snapshots still renders 0 B:\n%s", card)
	}
	if strings.Contains(card, "1.0 MB") {
		t.Fatalf("the page recomputed the size from the tree instead of reading the measurement:\n%s", card)
	}
	if !strings.Contains(card, "as of 3m ago") {
		t.Fatalf("the age of the measurement is not visible:\n%s", card)
	}
	if !strings.Contains(card, "2 snapshots") {
		t.Fatalf("the snapshot count went missing:\n%s", card)
	}
}

// TestClientPageWithNoSnapshotsRendersADash: nothing stored, nothing measured.
// A dash is honest; a confident "0 B" for a client that has never backed up is
// the same class of lie in the other direction.
func TestClientPageWithNoSnapshotsRendersADash(t *testing.T) {
	st, _, ts, _ := newDiskStack(t)
	c := loggedInClient(t, st, ts)
	cl := createClientViaForm(t, c, ts, st, "brand-new", "targz")

	card := statCard(t, getPage(t, c, ts.URL+"/clients/"+cl.Slug), "On disk (allocated)")
	if !strings.Contains(card, "—") {
		t.Fatalf("a client with no snapshots and no measurement does not render a dash:\n%s", card)
	}
	if strings.Contains(card, "0 B") {
		t.Fatalf("a client with no snapshots renders a confident 0 B:\n%s", card)
	}
}

// TestClientPageWithSnapshotsButNoMeasurementSaysSo: snapshots exist but the
// lifecycle pass has not measured yet. Saying "not measured yet" is the only
// answer that is neither a zero nor an invented figure.
func TestClientPageWithSnapshotsButNoMeasurementSaysSo(t *testing.T) {
	st, _, ts, base := newDiskStack(t)
	c := loggedInClient(t, st, ts)
	cl := createClientViaForm(t, c, ts, st, "docs", "rsync")
	writeSnapshot(t, base, cl.Slug, "20260618T120000Z", "payload.bin", 4096)

	card := statCard(t, getPage(t, c, ts.URL+"/clients/"+cl.Slug), "On disk (allocated)")
	if !strings.Contains(card, "not measured yet") {
		t.Fatalf("a client whose store has never been measured does not say so:\n%s", card)
	}
	if strings.Contains(card, "0 B") {
		t.Fatalf("an unmeasured client renders 0 B:\n%s", card)
	}
}

// TestClientPageMarksALowerBound: a capped walk stores a lower bound, and the
// page must present it as one (≥) with the reason, never as a total.
func TestClientPageMarksALowerBound(t *testing.T) {
	st, _, ts, base := newDiskStack(t)
	c := loggedInClient(t, st, ts)
	cl := createClientViaForm(t, c, ts, st, "archive", "rsync")
	writeSnapshot(t, base, cl.Slug, "20260618T120000Z", "payload.bin", 4096)

	lowerBound := int64(12 * 1024 * 1024 * 1024 * 1024 / 10) // 1.2 TB
	if err := st.SetClientStorage(t.Context(), cl.ID, lowerBound, true, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("store measurement: %v", err)
	}

	card := statCard(t, getPage(t, c, ts.URL+"/clients/"+cl.Slug), "On disk (allocated)")
	if !strings.Contains(card, "≥ 1.2 TB") {
		t.Fatalf("a partial measurement is not rendered as a lower bound:\n%s", card)
	}
	if !strings.Contains(card, "lower bound") || !strings.Contains(card, "entry cap") {
		t.Fatalf("the page does not say why the figure is a lower bound:\n%s", card)
	}
	if !strings.Contains(card, "as of 1m ago") {
		t.Fatalf("the age of the lower bound is not visible:\n%s", card)
	}
}

// TestDashboardSumsStoredMeasurementsAndShowsTheirAge: the fleet figure is the
// sum of the same per-client measurements, aged at the OLDEST one included, and
// a client with no measurement makes it a lower bound rather than a zero.
//
// The backup directory is moved out from under the server before the request: a
// handler that measured the tree would report nothing (or fail), so the figure
// appearing at all proves it read the store.
func TestDashboardSumsStoredMeasurementsAndShowsTheirAge(t *testing.T) {
	st, srv, ts, base := newDiskStack(t)
	c := loggedInClient(t, st, ts)
	old := createClientViaForm(t, c, ts, st, "old-client", "rsync")
	fresh := createClientViaForm(t, c, ts, st, "fresh-client", "rsync")
	_ = createClientViaForm(t, c, ts, st, "unmeasured", "rsync") // enabled, no measurement yet

	now := time.Now()
	if err := st.SetClientStorage(t.Context(), old.ID, 100*1024*1024, false, now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("store old client: %v", err)
	}
	if err := st.SetClientStorage(t.Context(), fresh.ID, 200*1024*1024, false, now.Add(-2*time.Minute)); err != nil {
		t.Fatalf("store fresh client: %v", err)
	}
	srv.backupBaseDir = filepath.Join(t.TempDir(), "moved-away")

	body := getPage(t, c, ts.URL+"/")
	if !strings.Contains(body, "300.0 MB") {
		t.Fatalf("the dashboard did not sum the stored measurements (100 MB + 200 MB):\n%s", storageChips(body))
	}
	if !strings.Contains(body, "as of 10m ago") {
		t.Fatalf("the dashboard does not show the age of the oldest measurement:\n%s", storageChips(body))
	}
	if !strings.Contains(body, "1 client not measured yet") {
		t.Fatalf("the dashboard does not name the client it could not measure:\n%s", storageChips(body))
	}
	if !strings.Contains(storageChips(body), "≥") {
		t.Fatalf("a total missing one client is presented as exact:\n%s", storageChips(body))
	}
	_ = base
}

// TestDashboardSaysMeasuringUntilTheFirstPass: before any lifecycle pass has
// stored a figure, the row says so rather than rendering a confident 0 B.
func TestDashboardSaysMeasuringUntilTheFirstPass(t *testing.T) {
	st, _, ts, _ := newDiskStack(t)
	c := loggedInClient(t, st, ts)
	createClientViaForm(t, c, ts, st, "c1", "rsync")

	body := getPage(t, c, ts.URL+"/")
	if !strings.Contains(body, "measuring") {
		t.Fatalf("an unmeasured fleet does not say it is measuring:\n%s", storageChips(body))
	}
}

// storageChips returns just the dashboard's storage chips, so a "≥" elsewhere on
// the page cannot make an assertion pass.
func storageChips(body string) string {
	start := strings.Index(body, `class="storage-stats"`)
	if start < 0 {
		return "(no storage-stats block)"
	}
	chips := body[start:]
	if end := strings.Index(chips, "</div>\n      <table"); end > 0 {
		chips = chips[:end]
	}
	return chips
}
