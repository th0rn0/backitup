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
	t.Cleanup(func() { st.Close() })
	srv := New(st, false)
	tmp := t.TempDir()
	authKeys := filepath.Join(tmp, "authkeys", "authorized_keys")
	srv.authKeysPath = authKeys
	srv.backupBaseDir = filepath.Join(tmp, "backups")
	srv.publicHost = "backup.test:2222"
	srv.clientImage = "ghcr.io/th0rn0/backitup-client:test"
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
		"created", "OPENSSH PRIVATE KEY", "Bearer token",
		"BACKITUP_SERVER=backup.test:2222", "shown", // the "shown once" warning
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

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
