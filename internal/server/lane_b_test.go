package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// testStack builds a store + httptest server and returns both for setup.
func testStack(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ts := httptest.NewServer(New(st, false).Handler())
	t.Cleanup(ts.Close)
	return st, ts
}

func setAdmin(t *testing.T, st *store.Store, user, pass string) {
	t.Helper()
	hash, err := auth.HashPassword(pass)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if err := st.UpsertUser(context.Background(), user, hash); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
}

// noRedirectClient stops at the first response so we can assert on redirects.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func TestDashboardRequiresLogin(t *testing.T) {
	_, ts := testStack(t)
	c := noRedirectClient()
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated / = %d -> %q; want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestLoginWrongPassword(t *testing.T) {
	st, ts := testStack(t)
	setAdmin(t, st, "admin", "s3cret")
	c := noRedirectClient()
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"nope"}})
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Invalid username or password") {
		t.Fatal("expected generic invalid-credentials message")
	}
}

func TestLoginThenDashboard(t *testing.T) {
	st, ts := testStack(t)
	setAdmin(t, st, "admin", "s3cret")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := c.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"s3cret"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("good login status = %d, want 303", resp.StatusCode)
	}

	// With the session cookie, the dashboard renders (empty-state for zero clients).
	resp, err = c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "No clients yet") {
		t.Fatal("expected first-run empty state on the dashboard")
	}
}

func TestStaticCSSServed(t *testing.T) {
	_, ts := testStack(t)
	resp, err := http.Get(ts.URL + "/static/app.css")
	if err != nil {
		t.Fatalf("get css: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("css status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "--accent") {
		t.Fatal("css body missing expected token")
	}
}

func TestClientAPIConfigAndStatus(t *testing.T) {
	st, ts := testStack(t)
	token := "tok-laptop-docs-123"
	hash, _ := auth.HashPassword(token)
	id, err := st.CreateClient(context.Background(), model.Client{
		Name: "laptop-docs", Mode: model.ModeRsync, Excludes: []string{"*.tmp"},
		RetentionDays: 14, OffsiteRemote: "s3", TokenHash: hash, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	// GET /api/v1/config with the bearer token.
	cfg := doAuthed(t, ts, "GET", "/api/v1/config", token, "")
	if cfg.StatusCode != http.StatusOK {
		t.Fatalf("config status = %d, want 200", cfg.StatusCode)
	}
	var got map[string]any
	_ = json.NewDecoder(cfg.Body).Decode(&got)
	cfg.Body.Close()
	if got["mode"] != "rsync" || got["retention_days"].(float64) != 14 {
		t.Fatalf("config mismatch: %v", got)
	}

	// POST /api/v1/status records a run.
	statusBody := `{"status":"ok","bytes":4200,"files":12,"snapshot_id":"snap-1"}`
	sresp := doAuthed(t, ts, "POST", "/api/v1/status", token, statusBody)
	if sresp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(sresp.Body)
		t.Fatalf("status post = %d (%s), want 201", sresp.StatusCode, b)
	}
	sresp.Body.Close()

	// The run is now visible to the store.
	latest, err := st.LatestRun(context.Background(), id)
	if err != nil || latest == nil || latest.Bytes != 4200 {
		t.Fatalf("recorded run = %+v, err=%v", latest, err)
	}
}

func TestClientAPIRejectsBadToken(t *testing.T) {
	st, ts := testStack(t)
	hash, _ := auth.HashPassword("real-token")
	_, _ = st.CreateClient(context.Background(), model.Client{Name: "c", Mode: model.ModeTarGz, TokenHash: hash, Enabled: true})

	// No token.
	if r := doAuthed(t, ts, "GET", "/api/v1/config", "", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", r.StatusCode)
	}
	// Wrong token.
	if r := doAuthed(t, ts, "GET", "/api/v1/config", "wrong-token", ""); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong-token status = %d, want 401", r.StatusCode)
	}
}

func TestPostStatusInvalidStatus(t *testing.T) {
	st, ts := testStack(t)
	token := "tok"
	hash, _ := auth.HashPassword(token)
	_, _ = st.CreateClient(context.Background(), model.Client{Name: "c", Mode: model.ModeTarGz, TokenHash: hash, Enabled: true})
	r := doAuthed(t, ts, "POST", "/api/v1/status", token, `{"status":"bogus"}`)
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, want 400", r.StatusCode)
	}
	r.Body.Close()
}

func doAuthed(t *testing.T, ts *httptest.Server, method, path, token, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, ts.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, path, err)
	}
	return resp
}
