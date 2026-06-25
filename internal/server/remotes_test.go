package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// rcloneStack builds a server+store with a rclone config path configured.
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

// readBody drains and closes a response body.
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
		t.Error("expected NoRclone banner when rclone not configured")
	}
}

func TestRemotesPageWithRclone(t *testing.T) {
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
	if !strings.Contains(body, "Add remote") {
		t.Error("add-remote section missing when rclone is configured")
	}
	// Backend options should be present in the dropdown.
	if !strings.Contains(body, "Amazon S3") {
		t.Error("Amazon S3 backend option missing")
	}
	if !strings.Contains(body, "Google Drive") {
		t.Error("Google Drive backend option missing")
	}
}

func TestRemotesPageShowsConfiguredRemotes(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	// Insert a remote directly into the store.
	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "my-s3", Backend: model.BackendS3,
		Config:    map[string]string{"access_key_id": "key", "secret_access_key": "sec", "region": "us-east-1"},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	resp, err := c.Get(ts.URL + "/settings/remotes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "my-s3") {
		t.Error("configured remote name not shown in table")
	}
	if !strings.Contains(body, "Amazon S3") {
		t.Error("backend label not shown in table")
	}
}

func TestRemotesPageFlashAndError(t *testing.T) {
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

// ── POST /settings/remotes ────────────────────────────────────────────────────

func TestCreateRemoteRequiresLogin(t *testing.T) {
	_, ts := testStack(t)
	c := noRedirectClient()
	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name": {"s3"}, "backend": {"s3"}, "access_key_id": {"k"}, "secret_access_key": {"s"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated POST /settings/remotes = %d -> %q; want 303 -> /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestCreateRemoteMissingName(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name": {""}, "backend": {"s3"}, "access_key_id": {"k"}, "secret_access_key": {"s"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "required") {
		t.Errorf("missing-name error not in body; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestCreateRemoteInvalidName(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name": {"bad name!"}, "backend": {"s3"}, "access_key_id": {"k"}, "secret_access_key": {"s"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "letters") {
		t.Errorf("invalid-name error not in body; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestCreateRemoteUnknownBackend(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name": {"r"}, "backend": {"badbackend"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Unknown") {
		t.Errorf("unknown-backend error not in body; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestCreateS3RemoteMissingFields(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cases := []url.Values{
		{"name": {"s3"}, "backend": {"s3"}, "access_key_id": {""}, "secret_access_key": {"s"}},
		{"name": {"s3"}, "backend": {"s3"}, "access_key_id": {"k"}, "secret_access_key": {""}},
	}
	for _, form := range cases {
		resp, err := c.PostForm(ts.URL+"/settings/remotes", form)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, "required") {
			t.Errorf("form %v: missing-required error not in body:\n%s", form, body[:min(len(body), 500)])
		}
	}
}

func TestCreateS3RemoteSuccess(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name":              {"my-s3"},
		"backend":           {"s3"},
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

func TestCreateS3CompatRemoteSuccess(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name":              {"r2"},
		"backend":           {"s3-compat"},
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

func TestCreateGDriveRemoteMissingCreds(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name": {"gdrive"}, "backend": {"drive"}, "service_account_credentials": {""},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "required") {
		t.Errorf("missing-creds error not in body; got:\n%s", body[:min(len(body), 500)])
	}
}

func TestCreateGDriveRemoteSuccess(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cred := `{"type":"service_account","project_id":"proj","private_key_id":"kid","private_key":"key","client_email":"svc@proj.iam.gserviceaccount.com","client_id":"1","auth_uri":"u","token_uri":"u"}`
	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name":                        {"gdrive"},
		"backend":                     {"drive"},
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

func TestCreateRemoteDuplicateName(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	form := url.Values{
		"name":    {"dup"},
		"backend": {"b2"},
		"account": {"appkeyid"},
		"key":     {"appkey"},
	}
	// First POST creates the remote.
	if _, err := c.PostForm(ts.URL+"/settings/remotes", form); err != nil {
		t.Fatalf("first post: %v", err)
	}
	// Second POST should be rejected; use no-redirect client to inspect the redirect.
	// Log in a fresh no-redirect client.
	setAdmin(t, st, "admin", "pw")
	nc2 := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	jar, _ := cookiejar.New(nil)
	nc2.Jar = jar
	if _, err := nc2.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"pw"}}); err != nil {
		t.Fatalf("login: %v", err)
	}
	resp, err := nc2.PostForm(ts.URL+"/settings/remotes", form)
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "already+exists") && !strings.Contains(loc, "already%20exists") {
		t.Errorf("duplicate redirect location = %q; want 'already exists' in query", loc)
	}
	// The DB must still have exactly one remote.
	remotes, err := st.ListRemotes(t.Context())
	if err != nil {
		t.Fatalf("list remotes: %v", err)
	}
	if len(remotes) != 1 {
		t.Errorf("got %d remotes after duplicate insert, want 1", len(remotes))
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
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	// Create the remote first.
	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "to-delete", Backend: model.BackendB2,
		Config: map[string]string{"account": "a", "key": "k"}, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	resp, err := c.Post(ts.URL+"/settings/remotes/to-delete/delete", "", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "deleted") {
		t.Errorf("success flash missing 'deleted'; got:\n%s", body[:min(len(body), 500)])
	}
}
