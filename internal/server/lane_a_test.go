package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/keys"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// ingestStack builds a server with temp ingest paths so the add-client flow can
// write a real authorized_keys file.
func ingestStack(t *testing.T) (*store.Store, *httptest.Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	srv.dbTimeout = testDBTimeout // assert behaviour, not the machine's speed
	tmp := t.TempDir()
	authKeys := filepath.Join(tmp, "authkeys", "authorized_keys")
	srv.authKeysPath = authKeys
	srv.backupBaseDir = filepath.Join(tmp, "backups")
	srv.publicHost = "backup.test:2222"
	srv.clientImage = "th0rn0/backitup-client:test"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, ts, authKeys
}

func TestAddClientFlow(t *testing.T) {
	st, ts, authKeys := ingestStack(t)
	c := loggedInClient(t, st, ts)

	resp, err := c.PostForm(ts.URL+"/clients", url.Values{
		"name": {"docs"}, "mode": {"rsync"}, "retention_days": {"7"},
		"offsite_remote": {"s3"}, "expected_interval_secs": {"3600"},
	})
	if err != nil {
		t.Fatalf("post clients: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	for _, want := range []string{
		"created", "OPENSSH PRIVATE KEY", "BACKITUP_TOKEN=",
		"BACKITUP_SERVER=backup.test:2222", "BACKITUP_API=",
		"BACKITUP_INSECURE=1", "cannot be retrieved later",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("created page missing %q", want)
		}
	}
	// This server has no BACKITUP_SSH_HOST_KEY, so it has no known_hosts file to
	// hand over: a command mounting one would be a dead end (see
	// client_created_page_test.go), so the page must not render it at all.
	if strings.Contains(html, "BACKITUP_KNOWN_HOSTS") {
		t.Errorf("created page references BACKITUP_KNOWN_HOSTS without a host key on the server:\n%s", html)
	}

	// Client persisted with a public key + token hash.
	clients, err := st.ListClients(context.Background())
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients = %d, err %v", len(clients), err)
	}
	cl := clients[0]
	if cl.Name != "docs" || cl.SSHPubKey == "" || cl.TokenHash == "" || cl.RetentionDays != 7 {
		t.Fatalf("stored client wrong: %+v", cl)
	}

	// authorized_keys was regenerated atomically with this client's forced command.
	data, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if !strings.Contains(string(data), `command="rrsync `) || !strings.Contains(string(data), cl.SSHPubKey) {
		t.Fatalf("authorized_keys missing forced command or key:\n%s", data)
	}
}

func TestAddClientDuplicateName(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)
	form := url.Values{"name": {"dup"}, "mode": {"targz"}}
	r1, _ := c.PostForm(ts.URL+"/clients", form)
	r1.Body.Close()
	r2, err := c.PostForm(ts.URL+"/clients", form)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate name status = %d, want 400", r2.StatusCode)
	}
	body, _ := io.ReadAll(r2.Body)
	if !strings.Contains(string(body), "already taken") {
		t.Fatal("expected duplicate-name error message")
	}
}

func TestAddClientInvalidMode(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)
	r, err := c.PostForm(ts.URL+"/clients", url.Values{"name": {"x"}, "mode": {"zip"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want 400", r.StatusCode)
	}
}

func TestAddClientRequiresAuth(t *testing.T) {
	_, ts, _ := ingestStack(t)
	c := noRedirectClient()
	r, err := c.PostForm(ts.URL+"/clients", url.Values{"name": {"x"}, "mode": {"targz"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Fatalf("unauth create = %d -> %q; want 303 -> /login", r.StatusCode, r.Header.Get("Location"))
	}
}

func TestGetNewClientForm(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)
	resp, err := c.Get(ts.URL + "/clients/new")
	if err != nil {
		t.Fatalf("get form: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Create client") {
		t.Fatalf("add-client form not rendered (status %d)", resp.StatusCode)
	}
}

func TestClientDetail(t *testing.T) {
	st, ts, _ := ingestStack(t)
	_, err := st.CreateClient(context.Background(), model.Client{
		Name: "detail-me", Mode: model.ModeTarGz, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := loggedInClient(t, st, ts)

	resp, err := c.Get(ts.URL + "/clients/detail-me")
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "detail-me") {
		t.Fatalf("detail page wrong (status %d)", resp.StatusCode)
	}

	// Unknown name -> 404.
	resp2, err := c.Get(ts.URL + "/clients/nonexistent")
	if err != nil {
		t.Fatalf("get unknown: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown client = %d, want 404", resp2.StatusCode)
	}
}

func TestRotateClientFlow(t *testing.T) {
	st, ts, authKeys := ingestStack(t)
	ctx := context.Background()

	// Create a client to rotate.
	id, err := st.CreateClient(ctx, model.Client{
		Name: "rotateme", Mode: model.ModeRsync, RetentionDays: 14,
		SSHPubKey: "ssh-ed25519 AAAA original", TokenHash: "originalhash", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	c := loggedInClient(t, st, ts)

	// Happy path: rotate with confirmation.
	resp, err := c.PostForm(ts.URL+"/clients/rotateme/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	for _, want := range []string{
		"Credentials rotated", "OPENSSH PRIVATE KEY", "BACKITUP_TOKEN=",
		"stopped working immediately", "BACKITUP_SERVER=",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rotated page missing %q", want)
		}
	}

	// Credentials in DB must have changed.
	got, err := st.GetClient(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get client: %v", err)
	}
	if got.SSHPubKey == "ssh-ed25519 AAAA original" || got.TokenHash == "originalhash" {
		t.Fatal("credentials were not rotated in the DB")
	}
	if got.RetentionDays != 14 {
		t.Fatalf("unrelated field clobbered after rotate: %+v", got)
	}

	// authorized_keys must reflect the new public key.
	data, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if !strings.Contains(string(data), got.SSHPubKey) {
		t.Fatal("authorized_keys does not contain the new public key after rotate")
	}
}

func TestRotateClientMissingConfirm(t *testing.T) {
	st, ts, _ := ingestStack(t)
	ctx := context.Background()
	_, err := st.CreateClient(ctx, model.Client{Name: "x", Mode: model.ModeTarGz, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := loggedInClient(t, st, ts)
	resp, err := c.PostForm(ts.URL+"/clients/x/rotate", url.Values{})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing confirm = %d, want 400", resp.StatusCode)
	}
}

func TestRotateClientUnknown(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)
	resp, err := c.PostForm(ts.URL+"/clients/nonexistent/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown client rotate = %d, want 404", resp.StatusCode)
	}
}

func TestRotateClientRequiresAuth(t *testing.T) {
	_, ts, _ := ingestStack(t)
	c := noRedirectClient()
	resp, err := c.PostForm(ts.URL+"/clients/nonexistent/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth rotate = %d -> %q; want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestRotateClientUnknownSlug verifies that a non-existent slug returns 404.
func TestRotateClientUnknownSlug(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)
	resp, err := c.PostForm(ts.URL+"/clients/nonexistent-slug/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown slug rotate = %d, want 404", resp.StatusCode)
	}
}

// TestRotateClientStoreError verifies that with an unusable database the request
// is refused at the door rather than executed against a half-live server.
// requireAdmin now re-checks the session's account on every request, so a dead
// DB fails closed: the caller is bounced to /login instead of reaching the
// handler (previously this reached the rotate path and returned 500).
func TestRotateClientStoreError(t *testing.T) {
	st, ts, _ := ingestStack(t)
	ctx := context.Background()
	_, err := st.CreateClient(ctx, model.Client{
		Name: "storedown", Mode: model.ModeTarGz, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA original", TokenHash: "originalhash",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := noRedirectClientWithSession(t, st, ts)

	// Close the underlying store so the session cannot be re-validated.
	_ = st.Close()

	resp, err := c.PostForm(ts.URL+"/clients/storedown/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("store-down rotate = %d -> %q, want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// noRedirectClientWithSession logs in as the test admin without following the
// post-login redirect, so handler statuses stay observable after the store dies.
func noRedirectClientWithSession(t *testing.T, st *store.Store, ts *httptest.Server) *http.Client {
	t.Helper()
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
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", resp.StatusCode)
	}
	return c
}

// TestRotateClientInvalidatesOldToken verifies that a bearer token is rejected
// immediately after the client's credentials are rotated.
func TestRotateClientInvalidatesOldToken(t *testing.T) {
	st, ts, _ := ingestStack(t)
	ctx := context.Background()

	oldToken, err := keys.GenerateToken()
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	hash, err := auth.HashPassword(oldToken)
	if err != nil {
		t.Fatalf("hash token: %v", err)
	}
	_, err = st.CreateClient(ctx, model.Client{
		Name: "tok-test", Mode: model.ModeRsync, RetentionDays: 7,
		SSHPubKey: "ssh-ed25519 AAAA tok-test", TokenHash: hash, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	// Old token must be accepted before rotation.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/config", nil)
	req.Header.Set("Authorization", "Bearer "+oldToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("pre-rotate api call: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("old token pre-rotate = %d, want 200", resp.StatusCode)
	}

	// Rotate via the admin UI.
	c := loggedInClient(t, st, ts)
	resp2, err := c.PostForm(ts.URL+"/clients/tok-test/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d, want 200", resp2.StatusCode)
	}

	// Old token must be rejected after rotation.
	req2, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/config", nil)
	req2.Header.Set("Authorization", "Bearer "+oldToken)
	resp3, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("post-rotate api call: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token post-rotate = %d, want 401", resp3.StatusCode)
	}
}

func TestDeleteClientFlow(t *testing.T) {
	st, ts, authKeys := ingestStack(t)
	ctx := context.Background()

	id, err := st.CreateClient(ctx, model.Client{
		Name: "bye", Mode: model.ModeTarGz, RetentionDays: 7,
		SSHPubKey: "ssh-ed25519 AAAA bye", TokenHash: "hash", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	c := loggedInClient(t, st, ts)

	// Confirm required.
	r, err := c.PostForm(ts.URL+"/clients/bye/delete", url.Values{})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing confirm = %d, want 400", r.StatusCode)
	}

	// Happy path: delete with confirmation → redirect to dashboard.
	r2, err := c.PostForm(ts.URL+"/clients/bye/delete", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("delete status = %d, want 200 (after redirect)", r2.StatusCode)
	}

	// Client must be gone from the store.
	got, err := st.GetClient(ctx, id)
	if err != nil || got != nil {
		t.Fatalf("client still exists after delete: %+v, err %v", got, err)
	}

	// authorized_keys must have been regenerated (empty now).
	data, _ := os.ReadFile(authKeys)
	if strings.Contains(string(data), "ssh-ed25519 AAAA bye") {
		t.Fatal("deleted client's key still in authorized_keys")
	}
}

func TestDeleteClientUnknown(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)
	r, err := c.PostForm(ts.URL+"/clients/nonexistent/delete", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown delete = %d, want 404", r.StatusCode)
	}
}

func TestDeleteClientRequiresAuth(t *testing.T) {
	_, ts, _ := ingestStack(t)
	c := noRedirectClient()
	r, err := c.PostForm(ts.URL+"/clients/nonexistent/delete", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Fatalf("unauth delete = %d -> %q; want 303 -> /login", r.StatusCode, r.Header.Get("Location"))
	}
}
