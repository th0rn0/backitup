package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/store"
)

// fakeRclone installs a stub rclone binary that exits 0 for all subcommands
// and returns empty JSON for "config dump". It prepends its temp dir to PATH
// for the duration of the test.
func fakeRclone(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in dump) echo '{}'; exit 0;; create|delete) exit 0;; esac; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// fakeRcloneWithDump is like fakeRclone but "config dump" outputs dumpJSON.
// The JSON is written to a file to avoid shell quoting issues.
func fakeRcloneWithDump(t *testing.T, dumpJSON string) {
	t.Helper()
	dir := t.TempDir()
	dumpFile := filepath.Join(dir, "dump.json")
	if err := os.WriteFile(dumpFile, []byte(dumpJSON), 0o644); err != nil {
		t.Fatalf("write dump.json: %v", err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in dump) cat " + dumpFile + "; exit 0;; create|delete) exit 0;; esac; done\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// rcloneStack builds a server+store with a rclone.conf path configured.
// Call fakeRclone before making HTTP requests that invoke rclone.
func rcloneStack(t *testing.T) (*store.Store, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	srv.ConfigureRclone(filepath.Join(t.TempDir(), "rclone.conf"))
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, ts
}

// readBody drains and closes a response body, returning the content as string.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// ── unit tests ────────────────────────────────────────────────────────────────

func TestValidRemoteName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"s3", true},
		{"gdrive", true},
		{"my-backup.1", true},
		{"My_Remote", true},
		{"", false},
		{"has space", false},
		{"has/slash", false},
		{"semi;colon", false},
		{"back`tick", false},
		{"dollar$", false},
		{"amp&", false},
		{"pipe|", false},
		{"gt>", false},
		{"lt<", false},
	}
	for _, c := range cases {
		if got := validRemoteName(c.name); got != c.want {
			t.Errorf("validRemoteName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConfigureRclone(t *testing.T) {
	s := New(nil, false)

	// Empty path leaves rcloneConfig unchanged (stays "").
	s.ConfigureRclone("")
	if s.rcloneConfig != "" {
		t.Fatalf("empty ConfigureRclone set rcloneConfig = %q, want empty", s.rcloneConfig)
	}

	// Non-empty path sets it.
	s.ConfigureRclone("/data/rclone.conf")
	if s.rcloneConfig != "/data/rclone.conf" {
		t.Fatalf("ConfigureRclone did not apply: %q", s.rcloneConfig)
	}

	// Second call with empty preserves existing value.
	s.ConfigureRclone("")
	if s.rcloneConfig != "/data/rclone.conf" {
		t.Fatalf("empty ConfigureRclone overwrote existing: %q", s.rcloneConfig)
	}
}

func TestListRcloneRemotesNotConfigured(t *testing.T) {
	s := New(nil, false)
	// rcloneConfig intentionally left empty → short-circuits without touching rclone.
	remotes, err := s.listRcloneRemotes(context.Background())
	if err != nil || remotes != nil {
		t.Fatalf("unconfigured = (%v, %v), want (nil, nil)", remotes, err)
	}
}

func TestListRcloneRemotesEmpty(t *testing.T) {
	fakeRclone(t)
	s := New(nil, false)
	s.ConfigureRclone(filepath.Join(t.TempDir(), "rclone.conf"))

	remotes, err := s.listRcloneRemotes(context.Background())
	if err != nil {
		t.Fatalf("listRcloneRemotes: %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("got %d remotes, want 0", len(remotes))
	}
}

func TestListRcloneRemotes(t *testing.T) {
	dumpJSON := `{"gdrive":{"type":"drive","scope":"drive"},"s3":{"type":"s3","provider":"AWS"}}`
	fakeRcloneWithDump(t, dumpJSON)
	s := New(nil, false)
	s.ConfigureRclone(filepath.Join(t.TempDir(), "rclone.conf"))

	remotes, err := s.listRcloneRemotes(context.Background())
	if err != nil {
		t.Fatalf("listRcloneRemotes: %v", err)
	}
	if len(remotes) != 2 {
		t.Fatalf("got %d remotes, want 2: %v", len(remotes), remotes)
	}
	// Sorted alphabetically: gdrive before s3.
	if remotes[0].Name != "gdrive" || remotes[0].Type != "drive" {
		t.Errorf("remotes[0] = %+v, want {gdrive drive}", remotes[0])
	}
	if remotes[1].Name != "s3" || remotes[1].Type != "s3" {
		t.Errorf("remotes[1] = %+v, want {s3 s3}", remotes[1])
	}
}

// ── GET /settings/remotes ─────────────────────────────────────────────────────

func TestRemotesRequiresLogin(t *testing.T) {
	_, ts := testStack(t)
	c := noRedirectClient()
	resp, err := c.Get(ts.URL + "/settings/remotes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated GET /settings/remotes = %d -> %q; want 303 -> /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRemotesPageNoRcloneConfigured(t *testing.T) {
	// rclone NOT configured → NoRclone banner should appear; forms should not.
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.Get(ts.URL + "/settings/remotes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "BACKITUP_RCLONE_CONFIG") {
		t.Error("expected NoRclone banner on page when rclone not configured")
	}
	if strings.Contains(body, "Add S3-compatible remote") {
		t.Error("S3 form shown when rclone not configured — should be hidden")
	}
}

func TestRemotesPageWithRclone(t *testing.T) {
	fakeRclone(t)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.Get(ts.URL + "/settings/remotes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if strings.Contains(body, "BACKITUP_RCLONE_CONFIG") {
		t.Error("NoRclone banner shown when rclone IS configured")
	}
	if !strings.Contains(body, "Add S3-compatible remote") {
		t.Error("S3 form missing when rclone is configured")
	}
	if !strings.Contains(body, "Add Google Drive remote") {
		t.Error("GDrive form missing when rclone is configured")
	}
}

func TestRemotesPageShowsConfiguredRemotes(t *testing.T) {
	fakeRcloneWithDump(t, `{"my-s3":{"type":"s3","provider":"AWS"}}`)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.Get(ts.URL + "/settings/remotes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "my-s3") {
		t.Error("configured remote name not shown in table")
	}
}

func TestRemotesPageFlashAndError(t *testing.T) {
	fakeRclone(t)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.Get(ts.URL + "/settings/remotes?msg=All+good&err=Something+bad")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "All good") {
		t.Error("flash message not rendered")
	}
	if !strings.Contains(body, "Something bad") {
		t.Error("error message not rendered")
	}
}

// ── POST /settings/remotes/s3 ─────────────────────────────────────────────────

func TestS3RemoteRequiresLogin(t *testing.T) {
	_, ts := testStack(t)
	c := noRedirectClient()
	resp, err := c.PostForm(ts.URL+"/settings/remotes/s3", url.Values{
		"name": {"s3"}, "access_key_id": {"k"}, "secret_access_key": {"s"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated POST s3 = %d -> %q; want 303 -> /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestS3RemoteMissingFields(t *testing.T) {
	// These hit validation before rclone is ever invoked, so no fakeRclone needed.
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cases := []struct {
		form    url.Values
		wantErr string
	}{
		{
			url.Values{"name": {""}, "access_key_id": {"key"}, "secret_access_key": {"sec"}},
			"required",
		},
		{
			url.Values{"name": {"s3"}, "access_key_id": {""}, "secret_access_key": {"sec"}},
			"required",
		},
		{
			url.Values{"name": {"s3"}, "access_key_id": {"key"}, "secret_access_key": {""}},
			"required",
		},
	}
	for _, tc := range cases {
		resp, err := c.PostForm(ts.URL+"/settings/remotes/s3", tc.form)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, tc.wantErr) {
			t.Errorf("form %v: body missing %q\n%s", tc.form, tc.wantErr, body[:min(len(body), 500)])
		}
	}
}

func TestS3RemoteInvalidName(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes/s3", url.Values{
		"name": {"bad name!"}, "access_key_id": {"k"}, "secret_access_key": {"s"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "letters") {
		t.Errorf("invalid-name error missing from body; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestS3RemoteCreated(t *testing.T) {
	fakeRclone(t)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes/s3", url.Values{
		"name":              {"my-s3"},
		"access_key_id":     {"AKIAIOSFODNN7EXAMPLE"},
		"secret_access_key": {"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"},
		"region":            {"us-east-1"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "my-s3") {
		t.Errorf("success flash missing remote name; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestS3RemoteCreatedWithEndpoint(t *testing.T) {
	fakeRclone(t)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes/s3", url.Values{
		"name":              {"r2"},
		"access_key_id":     {"key"},
		"secret_access_key": {"secret"},
		"endpoint":          {"https://account.r2.cloudflarestorage.com"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "r2") {
		t.Errorf("success flash missing remote name; got:\n%s", body[:min(len(body), 500)])
	}
}

// ── POST /settings/remotes/gdrive ─────────────────────────────────────────────

func TestGDriveRemoteRequiresLogin(t *testing.T) {
	_, ts := testStack(t)
	c := noRedirectClient()
	resp, err := c.PostForm(ts.URL+"/settings/remotes/gdrive", url.Values{
		"name": {"gdrive"}, "service_account_credentials": {`{"type":"service_account"}`},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated POST gdrive = %d -> %q; want 303 -> /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestGDriveRemoteMissingFields(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cases := []struct {
		form    url.Values
		wantErr string
	}{
		{
			url.Values{"name": {""}, "service_account_credentials": {`{"type":"service_account"}`}},
			"required",
		},
		{
			url.Values{"name": {"gdrive"}, "service_account_credentials": {""}},
			"required",
		},
	}
	for _, tc := range cases {
		resp, err := c.PostForm(ts.URL+"/settings/remotes/gdrive", tc.form)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, tc.wantErr) {
			t.Errorf("form %v: body missing %q\n%s", tc.form, tc.wantErr, body[:min(len(body), 500)])
		}
	}
}

func TestGDriveRemoteInvalidJSON(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes/gdrive", url.Values{
		"name":                        {"gdrive"},
		"service_account_credentials": {"not valid json {{{"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Invalid service account JSON") {
		t.Errorf("JSON error missing from body; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestGDriveRemoteInvalidName(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes/gdrive", url.Values{
		"name":                        {"bad name!"},
		"service_account_credentials": {`{"type":"service_account"}`},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "letters") {
		t.Errorf("invalid-name error missing from body; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestGDriveRemoteCreated(t *testing.T) {
	fakeRclone(t)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cred := `{"type":"service_account","project_id":"proj","private_key_id":"kid","private_key":"key","client_email":"svc@proj.iam.gserviceaccount.com","client_id":"1","auth_uri":"u","token_uri":"u"}`
	resp, err := c.PostForm(ts.URL+"/settings/remotes/gdrive", url.Values{
		"name":                        {"gdrive"},
		"service_account_credentials": {cred},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "gdrive") {
		t.Errorf("success flash missing remote name; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestGDriveRemoteCreatedWithTeamDrive(t *testing.T) {
	fakeRclone(t)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cred := `{"type":"service_account","project_id":"p","private_key_id":"k","private_key":"k","client_email":"s@p.iam.gserviceaccount.com","client_id":"1","auth_uri":"u","token_uri":"u"}`
	resp, err := c.PostForm(ts.URL+"/settings/remotes/gdrive", url.Values{
		"name":                        {"gdrive"},
		"service_account_credentials": {cred},
		"team_drive_id":               {"0ABCdef123"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "gdrive") {
		t.Errorf("success flash missing remote name; got:\n%s", body[:min(len(body), 500)])
	}
}

// ── POST /settings/remotes/{name}/delete ──────────────────────────────────────

func TestDeleteRemoteRequiresLogin(t *testing.T) {
	_, ts := testStack(t)
	c := noRedirectClient()
	resp, err := c.Post(ts.URL+"/settings/remotes/s3/delete", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated delete = %d -> %q; want 303 -> /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestDeleteRemoteInvalidName(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	// Space in path segment → decoded name = "bad name" → validRemoteName fails → 404.
	resp, err := c.Post(ts.URL+"/settings/remotes/bad%20name/delete", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("invalid name status = %d, want 404", resp.StatusCode)
	}
}

func TestDeleteRemoteSuccess(t *testing.T) {
	fakeRclone(t)
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.Post(ts.URL+"/settings/remotes/s3/delete", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "deleted") {
		t.Errorf("success flash missing 'deleted'; got:\n%s", body[:min(len(body), 500)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
