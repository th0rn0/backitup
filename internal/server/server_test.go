package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(New(st, false).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthzOK(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok\n" {
		t.Fatalf("body = %q, want %q", body, "ok\n")
	}
}

func TestHealthzAfterStoreClosed(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ts := httptest.NewServer(New(st, false).Handler())
	defer ts.Close()
	st.Close() // simulate a dead DB; healthz must NOT report a false green

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("get healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 when DB is down", resp.StatusCode)
	}
}

func TestUnknownRoute404(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHealthzRejectsPost(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Post(ts.URL+"/healthz", "text/plain", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	// Method-scoped route ("GET /healthz") should not serve POST.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("POST /healthz returned 200, want a non-200 (method not allowed/404)")
	}
}
