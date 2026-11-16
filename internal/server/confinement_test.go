package server

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// loginClient logs in as user/pass and returns a client that stops at the first
// response (so redirect assertions are visible) while keeping the session cookie.
func loginClient(t *testing.T, ts *httptest.Server, user, pass string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := c.PostForm(ts.URL+"/login", url.Values{"username": {user}, "password": {pass}})
	if err != nil {
		t.Fatalf("login %q: %v", user, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login %q status = %d, want 303 (%s)", user, resp.StatusCode, body)
	}
	return c
}

func mustGet(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

func mustPostForm(t *testing.T, c *http.Client, url string, form url.Values) (*http.Response, string) {
	t.Helper()
	resp, err := c.PostForm(url, form)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// assertLoginRedirect fails unless the response bounced the caller to /login.
func assertLoginRedirect(t *testing.T, what string, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("%s: status = %d, location = %q; want 303 -> /login",
			what, resp.StatusCode, resp.Header.Get("Location"))
	}
}

// S2/S3 via the HTTP surface: "dup a" and "dup-a" are one slug, so the second
// must be refused with a visible form error naming the conflict — not silently
// given a second key into the same read-write jail.
func TestAddClientRejectsSlugCollision(t *testing.T) {
	st, ts, authKeys := ingestStack(t)
	c := loggedInClient(t, st, ts)

	resp, _ := mustPostForm(t, c, ts.URL+"/clients", url.Values{"name": {"dup a"}, "mode": {"rsync"}})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first client status = %d, want 200", resp.StatusCode)
	}

	resp, body := mustPostForm(t, c, ts.URL+"/clients", url.Values{"name": {"dup-a"}, "mode": {"rsync"}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("colliding client status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(body, "dup-a") {
		t.Errorf("form error does not name the conflicting slug \"dup-a\": %s", body)
	}
	if !strings.Contains(body, "dup a") {
		t.Errorf("form error does not name the client already holding the slug: %s", body)
	}

	clients, err := st.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("slug collision created a second client: %+v", clients)
	}

	// One client, one forced command: no two keys can resolve to one jail.
	data, err := os.ReadFile(authKeys)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if n := strings.Count(string(data), `command="rrsync`); n != 1 {
		t.Fatalf("expected exactly 1 rrsync forced command, got %d:\n%s", n, data)
	}
}

// A name that slugs to "" would jail the client at the shared base directory,
// where it can read and write every other client's data.
func TestAddClientRejectsEmptySlug(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)

	for _, name := range []string{"..", ".", "/", "...."} {
		resp, body := mustPostForm(t, c, ts.URL+"/clients", url.Values{"name": {name}, "mode": {"targz"}})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("client named %q: status = %d, want 400", name, resp.StatusCode)
		}
		if !strings.Contains(body, "slug") {
			t.Errorf("client named %q: form error does not explain the slug requirement: %s", name, body)
		}
	}

	clients, err := st.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("unsafe names created clients: %+v", clients)
	}
}

// M1: deleting a user must end their admin access immediately, not in 12 hours.
func TestDeletedUserSessionRejected(t *testing.T) {
	st, ts, _ := ingestStack(t)
	setAdmin(t, st, "admin", "pw")
	// loginClient stops at the redirect, so each response's status is the
	// handler's own verdict rather than the followed-through page.
	admin := loginClient(t, ts, "admin", "pw")

	resp, _ := mustPostForm(t, admin, ts.URL+"/users",
		url.Values{"username": {"ghost"}, "password": {"ghostpass1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create user status = %d, want 303", resp.StatusCode)
	}
	ctx := context.Background()
	u, err := st.GetUserByUsername(ctx, "ghost")
	if err != nil || u == nil {
		t.Fatalf("load ghost user: %v (nil=%v)", err, u == nil)
	}

	ghost := loginClient(t, ts, "ghost", "ghostpass1")
	// Sanity: the fresh session works before the delete.
	if resp, _ := mustGet(t, ghost, ts.URL+"/users"); resp.StatusCode != http.StatusOK {
		t.Fatalf("ghost GET /users before delete = %d, want 200", resp.StatusCode)
	}
	if resp, _ := mustPostForm(t, ghost, ts.URL+"/clients",
		url.Values{"name": {"ghost-client"}, "mode": {"targz"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("ghost POST /clients before delete = %d, want 200", resp.StatusCode)
	}

	// Delete the account as the admin.
	resp, _ = mustPostForm(t, admin, ts.URL+"/users/"+strconv.FormatInt(u.ID, 10)+"/delete", url.Values{"confirm": {"1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete user status = %d, want 303", resp.StatusCode)
	}

	// The deleted user's cookie must no longer authenticate anything.
	resp, _ = mustGet(t, ghost, ts.URL+"/users")
	assertLoginRedirect(t, "deleted user GET /users", resp)
	resp, _ = mustPostForm(t, ghost, ts.URL+"/clients", url.Values{"name": {"ghost-client-2"}, "mode": {"targz"}})
	assertLoginRedirect(t, "deleted user POST /clients", resp)

	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	for _, cl := range clients {
		if cl.Name == "ghost-client-2" {
			t.Fatal("deleted user minted a new client keypair after deletion")
		}
	}
}

// Hardening: even if a session were left in the store (or a user row vanished
// out from under it), requireAdmin must re-check that the account still exists.
func TestSessionForVanishedUserRejected(t *testing.T) {
	st, ts, _ := ingestStack(t)
	setAdmin(t, st, "admin", "pw")
	// loginClient stops at the redirect, so each response's status is the
	// handler's own verdict rather than the followed-through page.
	admin := loginClient(t, ts, "admin", "pw")

	resp, _ := mustPostForm(t, admin, ts.URL+"/users",
		url.Values{"username": {"vanish"}, "password": {"vanishpass1"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create user status = %d, want 303", resp.StatusCode)
	}
	ctx := context.Background()
	u, err := st.GetUserByUsername(ctx, "vanish")
	if err != nil || u == nil {
		t.Fatalf("load vanish user: %v (nil=%v)", err, u == nil)
	}
	vanish := loginClient(t, ts, "vanish", "vanishpass1")
	if resp, _ := mustGet(t, vanish, ts.URL+"/users"); resp.StatusCode != http.StatusOK {
		t.Fatalf("vanish GET /users before delete = %d, want 200", resp.StatusCode)
	}

	// Remove the row behind the session store's back: only the existence check
	// in requireAdmin can catch this one.
	if err := st.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("delete user row: %v", err)
	}

	resp, _ = mustGet(t, vanish, ts.URL+"/users")
	assertLoginRedirect(t, "vanished user GET /users", resp)
	resp, _ = mustPostForm(t, vanish, ts.URL+"/clients", url.Values{"name": {"x"}, "mode": {"targz"}})
	assertLoginRedirect(t, "vanished user POST /clients", resp)
}

// Clients whose stored slug differs from what Slug(name) would produce must
// still be jailed to the stored value — the same one their snapshots use.
func TestClientDetailUsesStoredSlug(t *testing.T) {
	st, ts, _ := ingestStack(t)
	c := loggedInClient(t, st, ts)

	id, err := st.CreateClient(context.Background(), model.Client{
		Name: "Renamed Client", Mode: model.ModeRsync, SSHPubKey: "", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	cl, err := st.GetClient(context.Background(), id)
	if err != nil || cl == nil {
		t.Fatalf("load client: %v", err)
	}
	if cl.Slug != model.Slug(cl.Name) {
		t.Fatalf("test premise: persisted slug %q != Slug(name) %q", cl.Slug, model.Slug(cl.Name))
	}
	resp, _ := mustGet(t, c, ts.URL+"/clients/"+cl.Slug)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /clients/%s = %d, want 200", cl.Slug, resp.StatusCode)
	}
	// The old name-derived path must not resolve to this client any more than
	// an unrelated slug would.
	resp, _ = mustGet(t, c, ts.URL+"/clients/no-such-client")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /clients/no-such-client = %d, want 404", resp.StatusCode)
	}
}
