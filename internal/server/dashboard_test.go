package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// loggedInClient logs in as admin and returns a cookie-jar client.
func loggedInClient(t *testing.T, st *store.Store, ts *httptest.Server) *http.Client {
	t.Helper()
	setAdmin(t, st, "admin", "pw")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	return c
}

func TestLogout(t *testing.T) {
	st, ts := testStack(t)
	setAdmin(t, st, "admin", "pw")
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()

	// Logout, then the dashboard must redirect to /login again.
	resp, err = c.Post(ts.URL+"/logout", "", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", resp.StatusCode)
	}
	resp, err = c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get after logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Location") != "/login" {
		t.Fatalf("after logout / -> %q, want /login", resp.Header.Get("Location"))
	}
}

func TestGetLoginPage(t *testing.T) {
	_, ts := testStack(t)
	resp, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatalf("get login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login page status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Sign in") {
		t.Fatal("login page missing sign-in form")
	}
}

// TestDashboardWithClients exercises every health/format branch: ok (sizes,
// relative times), failed, never, and stale, plus the failed-first sort.
func TestDashboardWithClients(t *testing.T) {
	st, ts := testStack(t)
	ctx := context.Background()
	now := time.Now().UTC()

	mk := func(name string, mode model.Mode, interval int) int64 {
		id, err := st.CreateClient(ctx, model.Client{
			Name: name, Mode: mode, RetentionDays: 14, OffsiteRemote: "s3",
			ExpectedIntervalSecs: interval, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return id
	}
	run := func(id int64, status model.RunStatus, ago time.Duration, bytes int64) {
		fin := now.Add(-ago)
		if _, err := st.RecordRun(ctx, model.Run{
			ClientID: id, StartedAt: fin.Add(-time.Minute), FinishedAt: fin,
			Status: status, Bytes: bytes,
		}); err != nil {
			t.Fatalf("record run: %v", err)
		}
	}

	okRecent := mk("a-ok-recent", model.ModeTarGz, 0)
	run(okRecent, model.StatusOK, 30*time.Second, 500) // "just now", B

	okHours := mk("b-ok-hours", model.ModeRsync, 0)
	run(okHours, model.StatusOK, 3*time.Hour, 5*1024*1024) // "3h ago", MB

	okDays := mk("c-ok-days", model.ModeTarGz, 0)
	run(okDays, model.StatusOK, 50*time.Hour, 2048) // "2d ago", KB

	failed := mk("d-failed", model.ModeRsync, 0)
	run(failed, model.StatusFailed, 1*time.Hour, 0)

	stale := mk("e-stale", model.ModeTarGz, 3600) // 1h cadence
	run(stale, model.StatusOK, 5*time.Hour, 1024) // ok but >2x interval -> stale

	_ = mk("f-never", model.ModeTarGz, 0) // no runs -> "never"

	c := loggedInClient(t, st, ts)
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	for _, want := range []string{
		"a-ok-recent", "f-never", // names render
		"just now", "3h ago", "2d ago", // relTime branches
		"5.0 MB", "2.0 KB", "500 B", // humanBytes branches
		"Failed", "Stale", "Never", "OK", // health labels (DD2)
		"✓ s3", // offsite label
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}

	// Failed row must appear before any OK row (DD1: problems sort to top).
	if iFail, iOK := strings.Index(html, "d-failed"), strings.Index(html, "a-ok-recent"); iFail > iOK {
		t.Errorf("failed client should sort above ok client (failed@%d, ok@%d)", iFail, iOK)
	}

	// Summary counts: 3 ok, 1 stale, 1 failed, 1 never.
	for _, want := range []string{"3 OK", "1 stale", "1 failed", "1 never"} {
		if !strings.Contains(html, want) {
			t.Errorf("summary band missing %q", want)
		}
	}
}

func TestConfigEmptyExcludes(t *testing.T) {
	st, ts := testStack(t)
	token := "tok"
	hash, _ := auth.HashPassword(token)
	st.CreateClient(context.Background(), model.Client{
		Name: "c", Mode: model.ModeTarGz, RetentionDays: 7, TokenHash: hash, Enabled: true,
	}) // Excludes nil
	resp := doAuthed(t, ts, "GET", "/api/v1/config", token, "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"excludes":[]`) {
		t.Fatalf("nil excludes should serialise to [], got %s", body)
	}
}
