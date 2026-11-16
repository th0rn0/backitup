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

	"github.com/th0rn0/backitup/internal/keys"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// hardeningStack is ingestStack, plus the *Server so a test can break the ingest
// paths after clients exist.
func hardeningStack(t *testing.T) (*store.Store, *Server, *httptest.Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	tmp := t.TempDir()
	srv.authKeysPath = filepath.Join(tmp, "authkeys", "authorized_keys")
	srv.backupBaseDir = filepath.Join(tmp, "backups")
	srv.publicHost = "backup.test:2222"
	srv.clientImage = "th0rn0/backitup-client:test"
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, srv, ts
}

// A client name becomes a directory component (its slug) and part of the sshd
// forced command. One name too long for the filesystem used to make every
// MkdirAll fail, which froze the whole authorized_keys rewrite. It is refused at
// the edge now, and the boundary is exact: MaxSlugLen is accepted, +1 is not.
func TestCreateClientRejectsOverlongName(t *testing.T) {
	st, ts, authKeys := ingestStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	tooLong := strings.Repeat("a", model.MaxSlugLen+1)
	resp, err := c.PostForm(ts.URL+"/clients", url.Values{"name": {tooLong}, "mode": {"rsync"}})
	if err != nil {
		t.Fatalf("post clients: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("overlong name = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(body), "at most 63 characters") {
		t.Errorf("form error does not explain the length limit:\n%s", body)
	}
	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("overlong name created a client: %+v", clients)
	}

	// Exactly at the limit: accepted, and its directory is actually created.
	atLimit := strings.Repeat("b", model.MaxSlugLen)
	resp2, err := c.PostForm(ts.URL+"/clients", url.Values{"name": {atLimit}, "mode": {"rsync"}})
	if err != nil {
		t.Fatalf("post clients: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("%d-character name = %d, want 200", model.MaxSlugLen, resp2.StatusCode)
	}
	data, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if !strings.Contains(string(data), `/backups/`+atLimit+`"`) {
		t.Errorf("authorized_keys missing the %d-character client's jail:\n%s", model.MaxSlugLen, data)
	}
}

// Deleting a client must revoke its SSH access. When the authorized_keys rewrite
// cannot complete, the operator has to see it: before this, the failure was only
// logged and the redirect said success, so the deleted client's key stayed live
// with nobody the wiser.
func TestDeleteClientSurfacesAuthKeysFailure(t *testing.T) {
	st, srv, ts := hardeningStack(t)
	ctx := context.Background()

	// Two clients with real keys, so the file has something to revoke and a
	// remaining client whose directory setup can be made to fail.
	_, byeKey, err := keys.GenerateKeypair("backitup:bye")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	_, keeperKey, err := keys.GenerateKeypair("backitup:keeper")
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	for _, c := range []model.Client{
		{Name: "bye", Mode: model.ModeTarGz, SSHPubKey: byeKey, TokenHash: "h", Enabled: true},
		{Name: "keeper", Mode: model.ModeTarGz, SSHPubKey: keeperKey, TokenHash: "h", Enabled: true},
	} {
		if _, err := st.CreateClient(ctx, c); err != nil {
			t.Fatalf("create %q: %v", c.Name, err)
		}
	}
	if err := srv.regenAuthorizedKeys(ctx); err != nil {
		t.Fatalf("initial regen: %v", err)
	}
	data, err := os.ReadFile(srv.authKeysPath)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if !strings.Contains(string(data), byeKey) {
		t.Fatalf("test premise broken: the deleted client's key is not in the file yet:\n%s", data)
	}

	// Break directory setup for the client that stays: the base dir becomes a
	// regular file, so MkdirAll fails for every client under it.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	srv.backupBaseDir = blocked

	c := loggedInClient(t, st, ts)
	resp, err := c.PostForm(ts.URL+"/clients/bye/delete", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("delete with a failing authorized_keys rewrite = %d, want 500", resp.StatusCode)
	}
	for _, want := range []string{"deleted", "authorized_keys", "not-a-directory"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("error page does not mention %q:\n%s", want, body)
		}
	}

	// The row is gone …
	if got, err := st.GetClient(ctx, 1); err != nil || got != nil {
		t.Fatalf("deleted client still in the store: %+v (err %v)", got, err)
	}
	// … and the key was still revoked, because the broken directory no longer
	// aborts the rewrite.
	data, err = os.ReadFile(srv.authKeysPath)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if strings.Contains(string(data), byeKey) {
		t.Fatalf("deleted client's key is still in authorized_keys:\n%s", data)
	}
	if !strings.Contains(string(data), keeperKey) {
		t.Errorf("the remaining client lost its key:\n%s", data)
	}
}
