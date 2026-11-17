package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deleting a client removes its row and its SSH access, not its data: the slug
// directory stays on disk, and because a new client's slug is derived from its
// name, creating the same name again lands in the same directory and inherits
// the old snapshots. The delete confirmation has to say that, and say it
// accurately — so this test checks both the rendered page and the behaviour it
// describes.
func TestDeleteClientKeepsDirectoryAndSaysSo(t *testing.T) {
	st, srv, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	resp, err := c.PostForm(ts.URL+"/clients", url.Values{"name": {"docs"}, "mode": {"rsync"}})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("create status = %d, want 200", resp.StatusCode)
	}

	// The confirmation has to warn about the kept directory before the user
	// ticks the box.
	page, err := c.Get(ts.URL + "/clients/docs")
	if err != nil {
		t.Fatalf("get client page: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK {
		t.Fatalf("client page status = %d, want 200", page.StatusCode)
	}
	html := string(body)
	for _, want := range []string{
		"kept on disk",
		"snapshots",
		"same name",
		"re-uses that directory",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("delete confirmation does not mention %q:\n%s", want, deleteBlock(html))
		}
	}

	// Give the client a snapshot, then delete it.
	snapshot := filepath.Join(srv.backupBaseDir, "docs", "snapshots", "2026-01-01T00-00-00", "payload.tar")
	if err := os.MkdirAll(filepath.Dir(snapshot), 0o700); err != nil {
		t.Fatalf("make snapshot dir: %v", err)
	}
	if err := os.WriteFile(snapshot, []byte("data"), 0o600); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	del, err := c.PostForm(ts.URL+"/clients/docs/delete", url.Values{"confirm": {"1"}})
	if err != nil {
		t.Fatalf("delete client: %v", err)
	}
	del.Body.Close()

	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("clients after delete = %d, want 0", len(clients))
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("snapshot was removed with the client: %v", err)
	}

	// Re-creating the name re-uses the directory — which is exactly what the
	// warning promises, and why it has to be there.
	resp, err = c.PostForm(ts.URL+"/clients", url.Values{"name": {"docs"}, "mode": {"rsync"}})
	if err != nil {
		t.Fatalf("re-create client: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-create status = %d, want 200", resp.StatusCode)
	}
	clients, err = st.ListClients(ctx)
	if err != nil || len(clients) != 1 {
		t.Fatalf("clients after re-create = %d, err %v", len(clients), err)
	}
	if got := clients[0].Slug; got != "docs" {
		t.Fatalf("re-created slug = %q, want docs", got)
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("re-created client did not inherit the directory: %v", err)
	}
}

// deleteBlock narrows a failure message to the danger zone, so a missing phrase
// is reported against the delete confirmation rather than the whole page.
func deleteBlock(html string) string {
	i := strings.Index(html, "Danger zone")
	if i < 0 {
		return html
	}
	block := html[i:]
	if len(block) > 1200 {
		block = block[:1200]
	}
	return block
}
