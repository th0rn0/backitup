package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
		"BACKITUP_INSECURE=1", "BACKITUP_KNOWN_HOSTS=",
		"cannot be retrieved later",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("created page missing %q", want)
		}
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
	id, err := st.CreateClient(context.Background(), model.Client{
		Name: "detail-me", Mode: model.ModeTarGz, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := loggedInClient(t, st, ts)

	resp, err := c.Get(ts.URL + "/clients/" + itoa(id))
	if err != nil {
		t.Fatalf("get detail: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "detail-me") {
		t.Fatalf("detail page wrong (status %d)", resp.StatusCode)
	}

	// Unknown id -> 404.
	resp2, err := c.Get(ts.URL + "/clients/99999")
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
	resp, err := c.PostForm(ts.URL+"/clients/"+itoa(id)+"/rotate", url.Values{"confirm": {"1"}})
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
		"credentials rotated", "OPENSSH PRIVATE KEY", "BACKITUP_TOKEN=",
		"old credentials are now invalid", "BACKITUP_SERVER=",
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
	id, err := st.CreateClient(ctx, model.Client{Name: "x", Mode: model.ModeTarGz, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := loggedInClient(t, st, ts)
	resp, err := c.PostForm(ts.URL+"/clients/"+itoa(id)+"/rotate", url.Values{})
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
	resp, err := c.PostForm(ts.URL+"/clients/99999/rotate", url.Values{"confirm": {"1"}})
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
	resp, err := c.PostForm(ts.URL+"/clients/1/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauth rotate = %d -> %q; want 303 -> /login", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// TestRotateClientBadID verifies that a non-numeric path segment returns 404.
func TestRotateClientBadID(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)
	resp, err := c.PostForm(ts.URL+"/clients/notanumber/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad id rotate = %d, want 404", resp.StatusCode)
	}
}

// TestRotateClientStoreError verifies that a DB failure after the client lookup
// returns 500 rather than panicking.
func TestRotateClientStoreError(t *testing.T) {
	st, ts, _ := ingestStack(t)
	ctx := context.Background()
	id, err := st.CreateClient(ctx, model.Client{
		Name: "storedown", Mode: model.ModeTarGz, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA original", TokenHash: "originalhash",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	c := loggedInClient(t, st, ts)

	// Close the underlying store so the RotateClientCreds call fails.
	_ = st.Close()

	resp, err := c.PostForm(ts.URL+"/clients/"+itoa(id)+"/rotate", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("store-down rotate = %d, want 500", resp.StatusCode)
	}
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
	id, err := st.CreateClient(ctx, model.Client{
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
	resp2, err := c.PostForm(ts.URL+"/clients/"+itoa(id)+"/rotate", url.Values{"confirm": {"1"}})
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
	r, err := c.PostForm(ts.URL+"/clients/"+itoa(id)+"/delete", url.Values{})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing confirm = %d, want 400", r.StatusCode)
	}

	// Happy path: delete with confirmation → redirect to dashboard.
	r2, err := c.PostForm(ts.URL+"/clients/"+itoa(id)+"/delete", url.Values{"confirm": {"1"}})
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
	r, err := c.PostForm(ts.URL+"/clients/99999/delete", url.Values{"confirm": {"1"}})
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
	r, err := c.PostForm(ts.URL+"/clients/1/delete", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	r.Body.Close()
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Fatalf("unauth delete = %d -> %q; want 303 -> /login", r.StatusCode, r.Header.Get("Location"))
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
