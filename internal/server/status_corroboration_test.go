package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/th0rn0/backitup/internal/mode/rsync" // registers the server-side mode, as cmd/server does
	_ "github.com/th0rn0/backitup/internal/mode/targz"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// statusStack builds a Server + store with one client and a temp backup dir so
// postStatus can be exercised through its real code path, without HTTP.
func statusStack(t *testing.T, name string, mode model.Mode) (*store.Store, *Server, model.Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	srv.backupBaseDir = filepath.Join(t.TempDir(), "backups")

	ctx := context.Background()
	if _, err := st.CreateClient(ctx, model.Client{
		Name: name, Mode: mode, RetentionDays: 14,
		ExpectedIntervalSecs: 3600, Enabled: true,
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	clients, err := st.ListClients(ctx)
	if err != nil || len(clients) != 1 {
		t.Fatalf("list clients: %v (%d clients)", err, len(clients))
	}
	return st, srv, clients[0]
}

// postStatusFor calls the handler directly. The bearer token already did its
// job upstream (it says WHICH client is talking, not that it backed anything
// up), so the handler is where the claim has to be corroborated.
func postStatusFor(t *testing.T, srv *Server, cl model.Client, body string) int64 {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/status", strings.NewReader(body))
	r = r.WithContext(context.WithValue(r.Context(), clientCtxKey, &cl))
	w := httptest.NewRecorder()
	srv.postStatus(w, r)
	if w.Code != 201 {
		t.Fatalf("postStatus = %d (%s), want 201", w.Code, w.Body.String())
	}
	var resp struct {
		RunID int64 `json:"run_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	if resp.RunID == 0 {
		t.Fatalf("no run_id in response %q", w.Body.String())
	}
	return resp.RunID
}

func storedRun(t *testing.T, st *store.Store, id int64) model.Run {
	t.Helper()
	run, err := st.GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("get run %d: %v", id, err)
	}
	if run == nil {
		t.Fatalf("run %d not stored", id)
	}
	return *run
}

func tarGzPath(t *testing.T, srv *Server, cl model.Client, name, content string) string {
	t.Helper()
	dir, err := srv.clientDir(cl)
	if err != nil {
		t.Fatalf("clientDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// M3: a valid client token used to be believed outright. The host is untrusted
// by design, so a claimed "ok" for a snapshot the server cannot find is recorded
// as a failure, not as health.
func TestPostStatusFailsUncorroboratedOK(t *testing.T) {
	st, srv, cl := statusStack(t, "laptop-documents", model.ModeTarGz)

	id := postStatusFor(t, srv, cl, `{"status":"ok","bytes":1000000000,"snapshot_id":"does-not-exist.tar.gz"}`)

	run := storedRun(t, st, id)
	if run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q for an unverifiable snapshot, want %q", run.Status, model.StatusFailed)
	}
	if !strings.Contains(run.LogTail, "unverified by server") {
		t.Fatalf("log_tail = %q, want it flagged %q", run.LogTail, "unverified by server")
	}
}

// Regression guard: a real, non-empty archive is still recorded as ok.
func TestPostStatusAcceptsCorroboratedOKTarGz(t *testing.T) {
	st, srv, cl := statusStack(t, "laptop-documents", model.ModeTarGz)
	tarGzPath(t, srv, cl, "backup-2026-01-01T00-00-00Z.tar.gz", "not really gzip, but non-empty")

	id := postStatusFor(t, srv, cl, `{"status":"ok","bytes":1234,"files":7,"snapshot_id":"backup-2026-01-01T00-00-00Z.tar.gz"}`)

	run := storedRun(t, st, id)
	if run.Status != model.StatusOK {
		t.Fatalf("stored status = %q for an existing archive, want %q (log_tail: %q)", run.Status, model.StatusOK, run.LogTail)
	}
	if run.Bytes != 1234 || run.Files != 7 || run.SnapshotID != "backup-2026-01-01T00-00-00Z.tar.gz" {
		t.Fatalf("corroborated run mangled: %+v", run)
	}
	if strings.Contains(run.LogTail, "unverified by server") {
		t.Fatalf("verified run flagged unverified: %q", run.LogTail)
	}
}

// An empty archive is not a backup: the file exists but holds nothing.
func TestPostStatusFailsEmptyArchive(t *testing.T) {
	st, srv, cl := statusStack(t, "empty-archive", model.ModeTarGz)
	tarGzPath(t, srv, cl, "zero.tar.gz", "")

	id := postStatusFor(t, srv, cl, `{"status":"ok","bytes":0,"snapshot_id":"zero.tar.gz"}`)

	if run := storedRun(t, st, id); run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q for a zero-byte archive, want %q", run.Status, model.StatusFailed)
	}
}

// A snapshot ID that is not a plain component (path traversal, absolute path)
// cannot be corroborated because the store never lists such an ID.
func TestPostStatusFailsSnapshotIDOutsideStore(t *testing.T) {
	st, srv, cl := statusStack(t, "traversal", model.ModeTarGz)
	tarGzPath(t, srv, cl, "real.tar.gz", "payload")

	for _, bad := range []string{"../real.tar.gz", "/etc/passwd", "sub/real.tar.gz"} {
		id := postStatusFor(t, srv, cl, `{"status":"ok","snapshot_id":"`+bad+`"}`)
		if run := storedRun(t, st, id); run.Status != model.StatusFailed {
			t.Fatalf("stored status = %q for snapshot_id %q, want %q", run.Status, bad, model.StatusFailed)
		}
	}
}

// rsync mode: the snapshot is a directory under snapshots/, not an archive.
func TestPostStatusCorroboratesRsyncSnapshots(t *testing.T) {
	st, srv, cl := statusStack(t, "rsync-client", model.ModeRsync)
	dir, err := srv.clientDir(cl)
	if err != nil {
		t.Fatalf("clientDir: %v", err)
	}
	real := filepath.Join(dir, "snapshots", "20260101T000000Z")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}

	good := postStatusFor(t, srv, cl, `{"status":"ok","bytes":2048,"snapshot_id":"20260101T000000Z"}`)
	if run := storedRun(t, st, good); run.Status != model.StatusOK {
		t.Fatalf("stored status = %q for an existing rsync snapshot, want %q", run.Status, model.StatusOK)
	}

	bad := postStatusFor(t, srv, cl, `{"status":"ok","bytes":2048,"snapshot_id":"20260102T000000Z"}`)
	if run := storedRun(t, st, bad); run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q for a missing rsync snapshot, want %q", run.Status, model.StatusFailed)
	}
}

// The finalising update path (run_id set) must be corroborated too: it is the
// path a real client uses to turn "running" into "ok".
func TestPostStatusCorroboratesOnUpdatePath(t *testing.T) {
	st, srv, cl := statusStack(t, "update-path", model.ModeTarGz)

	runID := postStatusFor(t, srv, cl, `{"status":"running"}`)
	// Reuse the running row: same request shape a client sends on completion.
	r := httptest.NewRequest("POST", "/api/v1/status", strings.NewReader(
		`{"status":"ok","run_id":`+itoa(runID)+`,"bytes":999999,"snapshot_id":"never-written.tar.gz"}`))
	r = r.WithContext(context.WithValue(r.Context(), clientCtxKey, &cl))
	w := httptest.NewRecorder()
	srv.postStatus(w, r)
	if w.Code != 201 {
		t.Fatalf("postStatus update = %d (%s), want 201", w.Code, w.Body.String())
	}

	run := storedRun(t, st, runID)
	if run.Status != model.StatusFailed {
		t.Fatalf("updated run status = %q for an unverifiable snapshot, want %q", run.Status, model.StatusFailed)
	}
	if !strings.Contains(run.LogTail, "unverified by server") {
		t.Fatalf("log_tail = %q, want it flagged %q", run.LogTail, "unverified by server")
	}
}

// Failure/overlap reports are untouched: only a claimed SUCCESS is checked.
func TestPostStatusLeavesFailuresAlone(t *testing.T) {
	st, srv, cl := statusStack(t, "failing-client", model.ModeRsync)

	id := postStatusFor(t, srv, cl, `{"status":"failed","log_tail":"rsync: connection closed"}`)

	run := storedRun(t, st, id)
	if run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q, want %q", run.Status, model.StatusFailed)
	}
	if run.LogTail != "rsync: connection closed" {
		t.Fatalf("log_tail = %q, want it untouched", run.LogTail)
	}
}

// The bundled tar.gz client cannot name its archive: it streams to
// backitup-recv, which picks the filename server-side, so mode.BackupResult
// carries no SnapshotID and the report has no snapshot_id. A real backup of that
// shape must still be recorded as ok — otherwise the fix would fail every real
// tar.gz client.
func TestPostStatusAcceptsTarGzOKWithoutSnapshotID(t *testing.T) {
	st, srv, cl := statusStack(t, "targz-no-id", model.ModeTarGz)
	tarGzPath(t, srv, cl, "backup-20260101T000000Z-000.tar.gz", "streamed payload")

	id := postStatusFor(t, srv, cl, `{"status":"ok","bytes":4096,"files":9,"started_at":"`+time.Now().UTC().Add(-2*time.Minute).Format(time.RFC3339)+`"}`)

	run := storedRun(t, st, id)
	if run.Status != model.StatusOK {
		t.Fatalf("stored status = %q for an upload that did land, want %q (log_tail: %q)", run.Status, model.StatusOK, run.LogTail)
	}
}

// ...but a claimed success with no snapshot_id and nothing on disk is not a
// backup, however clean the numbers look.
func TestPostStatusFailsTarGzOKWithoutSnapshotIDAndNoArchive(t *testing.T) {
	st, srv, cl := statusStack(t, "targz-nothing", model.ModeTarGz)

	id := postStatusFor(t, srv, cl, `{"status":"ok","bytes":1000000000,"files":1234}`)

	run := storedRun(t, st, id)
	if run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q with no archive on disk, want %q", run.Status, model.StatusFailed)
	}
	if !strings.Contains(run.LogTail, "unverified by server") {
		t.Fatalf("log_tail = %q, want it flagged %q", run.LogTail, "unverified by server")
	}
}

// A stale archive cannot stand in for this run's upload.
func TestPostStatusFailsTarGzOKWithOnlyAStaleArchive(t *testing.T) {
	st, srv, cl := statusStack(t, "targz-stale", model.ModeTarGz)
	p := tarGzPath(t, srv, cl, "backup-20250101T000000Z-000.tar.gz", "last week's backup")
	old := time.Now().UTC().Add(-72 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	id := postStatusFor(t, srv, cl, `{"status":"ok","bytes":4096,"started_at":"`+time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)+`"}`)

	if run := storedRun(t, st, id); run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q for a 72h-old archive, want %q", run.Status, model.StatusFailed)
	}
}

// rsync has no excuse for omitting the snapshot name: fail closed.
func TestPostStatusFailsRsyncOKWithoutSnapshotID(t *testing.T) {
	st, srv, cl := statusStack(t, "rsync-no-id", model.ModeRsync)
	dir, err := srv.clientDir(cl)
	if err != nil {
		t.Fatalf("clientDir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "snapshots", "20260101T000000Z"), 0o755); err != nil {
		t.Fatalf("mkdir snapshot: %v", err)
	}

	id := postStatusFor(t, srv, cl, `{"status":"ok","bytes":2048}`)

	if run := storedRun(t, st, id); run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q without a reported snapshot, want %q", run.Status, model.StatusFailed)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
