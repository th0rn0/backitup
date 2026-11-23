package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
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
	st, ts, _ := rcloneStackWithConf(t)
	return st, ts
}

// rcloneStackWithConf also hands back the rclone.conf path, so a test can read
// what the app actually wrote there.
func rcloneStackWithConf(t *testing.T) (*store.Store, *httptest.Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	confPath := filepath.Join(t.TempDir(), "rclone.conf")
	srv.ConfigureRclone(confPath)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, ts, confPath
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
	// The create form asks for what B2 needs and offers no provider choice:
	// offsite storage is Backblaze B2 only (Thorno, 2026-11-19).
	if !strings.Contains(body, "Backblaze B2 only") {
		t.Error("the page does not say B2 is the only supported destination")
	}
	for _, field := range []string{`name="account"`, `name="key"`, `name="bucket"`, `name="path"`} {
		if !strings.Contains(body, field) {
			t.Errorf("B2 form field %s missing from the page", field)
		}
	}
	if strings.Contains(body, `id="add-remote-backend"`) || strings.Contains(body, `name="backend" value="b2"`) {
		t.Error("the page still offers a provider type selector")
	}
	for _, gone := range []string{"Amazon S3", "Google Drive", "SFTP", "WebDAV", "Azure Blob", "FTP"} {
		if strings.Contains(body, gone) {
			t.Errorf("the page still offers the dropped backend %q", gone)
		}
	}
}

func TestRemotesPageShowsConfiguredRemotes(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	// Insert a remote directly into the store.
	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "my-b2", Backend: model.BackendB2,
		Config:    map[string]string{"account": "key", "key": "sec", "bucket": "my-backups"},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}

	resp, err := c.Get(ts.URL + "/settings/remotes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "my-b2") {
		t.Error("configured remote name not shown in table")
	}
	if !strings.Contains(body, "Backblaze B2") {
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
		"name": {"b2"}, "account": {"k"}, "key": {"s"}, "bucket": {"my-bucket"},
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
		"name": {""}, "account": {"k"}, "key": {"s"}, "bucket": {"my-bucket"},
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
		"name": {"bad name!"}, "account": {"k"}, "key": {"s"}, "bucket": {"my-bucket"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "letters") {
		t.Errorf("invalid-name error not in body; got:\n%s", body[:min(len(body), 500)])
	}
}

// The type is not a choice any more. A request that names one this build does
// not support is refused at the boundary with 400 and the offending value in
// the message — not a silent fallback to b2, and not a 500.
func TestCreateRemoteRefusesUnsupportedType(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cases := []struct{ field, value string }{
		{"backend", "s3"},
		{"backend", "s3-compat"},
		{"backend", "drive"},
		{"backend", "sftp"},
		{"backend", "webdav"},
		{"backend", "azureblob"},
		{"backend", "ftp"},
		{"backend", "local"}, // the end-to-end test's backend: never configurable
		{"backend", "badbackend"},
		// `type` is rclone's own spelling for the same knob; it must not be a
		// way round the check.
		{"type", "s3"},
	}
	for _, tc := range cases {
		form := url.Values{
			"name": {"r"}, "account": {"k"}, "key": {"s"}, "bucket": {"my-bucket"},
			tc.field: {tc.value},
		}
		resp, err := c.PostForm(ts.URL+"/settings/remotes", form)
		if err != nil {
			t.Fatalf("post %s=%s: %v", tc.field, tc.value, err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s=%s: status = %d, want 400 (body: %s)", tc.field, tc.value, resp.StatusCode, body[:min(len(body), 300)])
		}
		if !strings.Contains(body, tc.value) {
			t.Errorf("%s=%s: refusal does not name the offending value; got: %s", tc.field, tc.value, body)
		}
		if !strings.Contains(body, "b2") {
			t.Errorf("%s=%s: refusal does not say what is supported; got: %s", tc.field, tc.value, body)
		}
	}
	// Nothing was stored by any of the refusals.
	remotes, err := st.ListRemotes(t.Context())
	if err != nil {
		t.Fatalf("list remotes: %v", err)
	}
	if len(remotes) != 0 {
		t.Fatalf("a refused create stored %d remote(s): %+v", len(remotes), remotes)
	}
}

func TestCreateB2RemoteMissingFields(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cases := []url.Values{
		{"name": {"b2"}, "account": {""}, "key": {"s"}, "bucket": {"my-bucket"}},
		{"name": {"b2"}, "account": {"k"}, "key": {""}, "bucket": {"my-bucket"}},
		{"name": {"b2"}, "account": {"k"}, "key": {"s"}, "bucket": {""}},
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

// A B2 remote is created from what B2 needs — name, keyID, applicationKey,
// bucket — and the managed block in rclone.conf carries type = b2 plus those
// values. The form sends no backend at all now, so this is also the check that
// an absent type means b2.
func TestCreateB2RemoteSuccess(t *testing.T) {
	st, ts, confPath := rcloneStackWithConf(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
		"name":    {"b2-offsite"},
		"account": {"004appkeyid"},
		"key":     {"K004applicationkey"},
		"bucket":  {"my-backups"},
		"path":    {"clients"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "b2-offsite") {
		t.Errorf("success flash missing remote name; got:\n%s", body[:min(len(body), 500)])
	}

	remote, err := st.GetRemoteByName(t.Context(), "b2-offsite")
	if err != nil || remote == nil {
		t.Fatalf("remote not stored: %v (remote=%v)", err, remote)
	}
	if remote.Backend != model.BackendB2 {
		t.Fatalf("stored backend = %q, want %q", remote.Backend, model.BackendB2)
	}
	if remote.Unsupported {
		t.Fatal("a remote created by this build is marked unsupported")
	}

	confBytes, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read rclone.conf: %v", err)
	}
	conf := string(confBytes)
	wantIn := func(want ...string) {
		t.Helper()
		for _, w := range want {
			if !strings.Contains(conf, w) {
				t.Fatalf("rclone.conf does not contain %q:\n%s", w, conf)
			}
		}
	}
	wantIn("[b2-offsite]", "type = b2", "account = 004appkeyid", "bucket = my-backups", "path = clients")
	if !strings.Contains(conf, rcloneManagedBegin) {
		t.Fatalf("the remote was not written inside the managed block:\n%s", conf)
	}
}

// A bucket that is not a bucket name is refused with the value named: it
// becomes part of every rclone path, so a slash or ".." in it would address a
// path the operator did not choose.
func TestCreateB2RemoteRefusesBadBucketAndPrefix(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	cases := []struct{ bucket, prefix, want string }{
		{"my/bucket", "", "my/bucket"},
		{"../escape", "", "../escape"},
		{"My-Bucket", "", "My-Bucket"},
		{"my-backups", "/abs", "Path prefix"},
		{"my-backups", "a/../b", "Path prefix"},
	}
	for _, tc := range cases {
		resp, err := c.PostForm(ts.URL+"/settings/remotes", url.Values{
			"name": {"b2"}, "account": {"k"}, "key": {"s"},
			"bucket": {tc.bucket}, "path": {tc.prefix},
		})
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, tc.want) {
			t.Errorf("bucket=%q prefix=%q: refusal does not mention %q; got:\n%s", tc.bucket, tc.prefix, tc.want, body[:min(len(body), 400)])
		}
	}
	if remotes, err := st.ListRemotes(t.Context()); err != nil || len(remotes) != 0 {
		t.Fatalf("a refused create stored remotes: %+v (err=%v)", remotes, err)
	}
}

func TestCreateRemoteDuplicateName(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	form := url.Values{
		"name":    {"dup"},
		"account": {"appkeyid"},
		"key":     {"appkey"},
		"bucket":  {"my-backups"},
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

// ── rclone.conf durability ────────────────────────────────────────────────────

// TestRcloneConfigWriteFailureKeepsOldFile: rclone.conf is read by every offsite
// upload, prune and remote test, so a regeneration that fails (full disk,
// read-only data volume, crash) must leave the previous file intact instead of
// truncating it. Before the atomic writer this was an os.WriteFile truncate-then-
// write, and a mid-write failure left an empty or partial config behind.
func TestRcloneConfigWriteFailureKeepsOldFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not block writes")
	}
	dir := t.TempDir()
	confPath := filepath.Join(dir, "rclone.conf")

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	srv.ConfigureRclone(confPath)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "old-b2", Backend: model.BackendB2,
		Config:    map[string]string{"account": "acct", "key": "k", "bucket": "my-backups"},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	if err := srv.RegenerateRcloneConfig(t.Context()); err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	before, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	if !strings.Contains(string(before), "[old-b2]") {
		t.Fatalf("seeded config missing old-b2 section: %q", before)
	}

	// A second remote makes the regenerated content different from the file on
	// disk, so a truncating write would be visible.
	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "new-b2", Backend: model.BackendB2,
		Config:    map[string]string{"account": "acct2", "key": "k2", "bucket": "my-backups"},
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("create second remote: %v", err)
	}

	// Make the write fail the way a full or read-only data volume does.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if err := srv.RegenerateRcloneConfig(t.Context()); err == nil {
		t.Fatal("regenerate into a read-only directory succeeded; expected an error")
	}

	after, err := os.ReadFile(confPath)
	if err != nil {
		t.Fatalf("old rclone.conf unreadable after the failed write: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed write changed rclone.conf:\n got: %q\nwant: %q", after, before)
	}
	if !strings.Contains(string(after), "[old-b2]") || !strings.Contains(string(after), "type = b2") {
		t.Fatalf("rclone.conf is not the complete old config: %q", after)
	}
	if strings.Contains(string(after), "new-b2") {
		t.Fatalf("failed write leaked new content into rclone.conf: %q", after)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only rclone.conf (temp file leaked)", names)
	}
}
