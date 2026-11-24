package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// The operator's view of the hot store. A run that was killed before it could
// report leaves a directory under snapshots/<ts>/ with no marker on it, so the
// mode lists it; the run row it opened and never finished is what says it is not
// a backup. The client page must not offer it, and it must not be downloadable —
// a download link is how a half-copied tree leaves the building as "the backup".
func TestClientPageHidesSnapshotFromKilledRun(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	srv.dbTimeout = testDBTimeout
	base := t.TempDir()
	srv.backupBaseDir = base
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	ctx := context.Background()

	clientID, err := st.CreateClient(ctx, model.Client{
		Name: "vmhost", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	slug := model.Slug("vmhost")
	snapDir := filepath.Join(base, slug, "snapshots")

	now := time.Now().UTC().Truncate(time.Second)
	goodStart := now.Add(-2 * time.Hour)
	goodID := goodStart.Format("20060102T150405Z")
	badStart := now.Add(-90 * time.Minute)
	badID := badStart.Format("20060102T150405Z")

	for _, id := range []string{goodID, badID} {
		dir := filepath.Join(snapDir, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "payload.bin"), []byte("data-"+id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(goodID, filepath.Join(snapDir, "latest")); err != nil {
		t.Fatal(err)
	}
	// The good run reported ok and named its snapshot; the killed one never
	// reported at all, and is what the server recorded when it gave up on it.
	if _, err := st.RecordRun(ctx, model.Run{
		ClientID: clientID, StartedAt: goodStart, FinishedAt: goodStart.Add(20 * time.Minute),
		Status: model.StatusOK, SnapshotID: goodID,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordRun(ctx, model.Run{
		ClientID: clientID, StartedAt: badStart, FinishedAt: badStart.Add(30 * time.Minute),
		Status: model.StatusFailed, LogTail: "interrupted: run exceeded maximum allowed duration",
	}); err != nil {
		t.Fatal(err)
	}

	c := loggedInClient(t, st, ts)
	page := getBody(t, c, ts.URL+"/clients/"+slug)
	if !strings.Contains(page, goodID) {
		t.Fatalf("the real snapshot must be listed:\n%s", page)
	}
	if strings.Contains(page, badID) {
		t.Fatalf("the killed run's directory must not be listed on the client page:\n%s", page)
	}

	// And it is not downloadable by id either.
	for id, want := range map[string]int{goodID: http.StatusOK, badID: http.StatusNotFound} {
		resp, err := c.Get(ts.URL + "/clients/" + slug + "/snapshots/" + id)
		if err != nil {
			t.Fatalf("GET snapshot %s: %v", id, err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET snapshot %s = %d, want %d", id, resp.StatusCode, want)
		}
	}
}
