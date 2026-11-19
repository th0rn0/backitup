package server

// Bulk delete takes its ids from a form field, so unlike the single-snapshot
// route no router path-cleaning applies to them. `ids=.` used to be passed
// straight to rsync's DeleteSnapshot, where filepath.Join(snapshotsDir, ".") ==
// snapshotsDir, and os.RemoveAll took the whole hot store (every snapshot AND the
// latest symlink) in one 200-answering request. These tests drive that HTTP path
// with the real handler, the real modes and real on-disk state.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// deleteStack is restoreStack plus an admin session that stops at the first
// response, so a 303 redirect is visible as a 303.
func deleteStack(t *testing.T) (*Server, *http.Client, *httptest.Server) {
	t.Helper()
	st, ts, srv := restoreStack(t)
	setAdmin(t, st, "admin", "pw")
	return srv, loginClient(t, ts, "admin", "pw"), ts
}

// makeClient creates a client through the real handler and returns its slug and
// storage directory.
func makeClient(t *testing.T, srv *Server, c *http.Client, ts *httptest.Server, name string, m model.Mode) (*model.Client, string) {
	t.Helper()
	resp, body := mustPostForm(t, c, ts.URL+"/clients", url.Values{
		"name": {name}, "mode": {string(m)}, "retention_days": {"7"},
	})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create client %q = %d: %s", name, resp.StatusCode, truncate([]byte(body), 300))
	}
	st := srv.st
	client, err := st.GetClientBySlug(context.Background(), name)
	if err != nil || client == nil {
		t.Fatalf("load client %q: %v (client=%v)", name, err, client)
	}
	dir, err := srv.clientDir(*client)
	if err != nil {
		t.Fatalf("clientDir: %v", err)
	}
	return client, dir
}

// plantRsyncSnapshots writes <clientDir>/snapshots/<id>/payload.txt for each id
// and points `latest` at the last one — the layout the rsync mode produces.
func plantRsyncSnapshots(t *testing.T, clientDir string, ids ...string) {
	t.Helper()
	snaps := filepath.Join(clientDir, "snapshots")
	for _, id := range ids {
		d := filepath.Join(snaps, id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("plant %s: %v", id, err)
		}
		if err := os.WriteFile(filepath.Join(d, "payload.txt"), []byte(id), 0o644); err != nil {
			t.Fatalf("plant %s: %v", id, err)
		}
	}
	if err := os.Symlink(ids[len(ids)-1], filepath.Join(snaps, "latest")); err != nil {
		t.Fatalf("plant latest: %v", err)
	}
}

// assertRsyncStoreIntact checks that every planted snapshot and the latest
// symlink are still there.
func assertRsyncStoreIntact(t *testing.T, clientDir string, ids ...string) {
	t.Helper()
	snaps := filepath.Join(clientDir, "snapshots")
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(snaps, id, "payload.txt")); err != nil {
			t.Errorf("snapshot %q was removed by a refused request: %v", id, err)
		}
	}
	latest, err := os.Readlink(filepath.Join(snaps, "latest"))
	if err != nil {
		t.Errorf("latest symlink gone after a refused request: %v", err)
		return
	}
	if latest != ids[len(ids)-1] {
		t.Errorf("latest -> %q, want %q", latest, ids[len(ids)-1])
	}
}

// followFlash closes a redirect response and returns the flash it carries plus
// the page a browser would land on, so a test can assert on what the operator
// actually reads — the whole defect was that the operator read nothing.
func followFlash(t *testing.T, c *http.Client, ts *httptest.Server, resp *http.Response) (loc, page string) {
	t.Helper()
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (Location %q)", resp.StatusCode, resp.Header.Get("Location"))
	}
	raw := resp.Header.Get("Location")
	loc, err := url.QueryUnescape(raw)
	if err != nil {
		t.Fatalf("unescape Location %q: %v", raw, err)
	}
	pageResp, page := mustGet(t, c, ts.URL+raw)
	if pageResp.StatusCode != http.StatusOK {
		t.Fatalf("flash page %q = %d, want 200 (so the reason is on screen)", raw, pageResp.StatusCode)
	}
	return loc, page
}

// TestBulkDeleteRefusesNonNameIDs is the regression test for the defect: a form
// carrying ".", "..", a traversal, or empty must delete nothing at all — the
// batch is validated before it runs, so a bad id cannot half-execute — and the
// refusal must be legible: a flash naming the offending id and saying nothing
// was deleted, on a page the operator actually sees.
func TestBulkDeleteRefusesNonNameIDs(t *testing.T) {
	const (
		snap1 = "20260101T000000Z"
		snap2 = "20260102T000000Z"
	)
	cases := []struct {
		name string
		ids  []string
		want string // substring of the flash, checked both in the Location and on the page
	}{
		{"dot alone", []string{"."}, `invalid snapshot id: "."`},
		{"dot mixed with a real id", []string{".", snap1}, `invalid snapshot id: "."`},
		{"real id mixed with dot", []string{snap1, "."}, `invalid snapshot id: "."`},
		{"dot dot", []string{".."}, `invalid snapshot id: ".."`},
		{"traversal", []string{"../../etc"}, `invalid snapshot id: "../../etc"`},
		{"empty", []string{""}, `invalid snapshot id: ""`},
		{"path fragment", []string{snap1 + "/payload.txt"}, `is not a name`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, c, ts := deleteStack(t)
			_, clientDir := makeClient(t, srv, c, ts, "nas", model.ModeRsync)
			plantRsyncSnapshots(t, clientDir, snap1, snap2)

			resp, _ := mustPostForm(t, c, ts.URL+"/clients/nas/snapshots/bulk-delete", url.Values{"ids": tc.ids})
			loc, page := followFlash(t, c, ts, resp)
			// Assert the data loss first: a redirect with the store intact is
			// the contract; this is the ordering that makes a pre-fix run show
			// the wipe itself.
			assertRsyncStoreIntact(t, clientDir, snap1, snap2)
			if _, err := os.Stat(clientDir); err != nil {
				t.Errorf("client dir itself was removed: %v", err)
			}
			if !strings.Contains(loc, "?err=") {
				t.Fatalf("refusal Location = %q, want a ?err= flash", loc)
			}
			if !strings.Contains(loc, "nothing was deleted") {
				t.Errorf("flash does not say nothing was deleted: %q", loc)
			}
			if !strings.Contains(loc, tc.want) {
				t.Errorf("flash does not name the offending id (%q): %q", tc.want, loc)
			}
			if !strings.Contains(page, "alert-error") || !strings.Contains(page, "nothing was deleted") {
				t.Errorf("rendered page shows no refusal reason:\n%s", truncate([]byte(page), 2000))
			}
		})
	}
}

// TestBulkDeleteValidIDStillDeletes is the other half: the validation must not
// over-reject. A real snapshot id still deletes exactly that snapshot, and the
// latest symlink is untouched.
func TestBulkDeleteValidIDStillDeletes(t *testing.T) {
	const (
		snap1 = "20260101T000000Z"
		snap2 = "20260102T000000Z"
	)
	srv, c, ts := deleteStack(t)
	_, clientDir := makeClient(t, srv, c, ts, "nas", model.ModeRsync)
	plantRsyncSnapshots(t, clientDir, snap1, snap2)

	resp, body := mustPostForm(t, c, ts.URL+"/clients/nas/snapshots/bulk-delete", url.Values{"ids": {snap1}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("bulk-delete a real id = %d, want 303 (body: %s)", resp.StatusCode, truncate([]byte(body), 300))
	}
	if _, err := os.Stat(filepath.Join(clientDir, "snapshots", snap1)); !os.IsNotExist(err) {
		t.Fatalf("snapshot %q still present after a valid bulk delete (err=%v)", snap1, err)
	}
	assertRsyncStoreIntact(t, clientDir, snap2)
}

// TestBulkDeleteRefusesNonNameIDsTarGz covers the same form path for the other
// mode: there DeleteSnapshot is os.Remove(dir/id), so "." is os.Remove(the client
// dir) — harmless while the dir is non-empty, but the same missing validation.
// The refusal is a legible flash, not a bare 400.
func TestBulkDeleteRefusesNonNameIDsTarGz(t *testing.T) {
	const (
		snap1 = "backup-20260101T000000Z-000.tar.gz"
		snap2 = "backup-20260102T000000Z-000.tar.gz"
	)
	for _, ids := range [][]string{{"."}, {".", snap1}, {".."}, {"../../etc"}} {
		srv, c, ts := deleteStack(t)
		_, clientDir := makeClient(t, srv, c, ts, "laptop", model.ModeTarGz)
		for _, id := range []string{snap1, snap2} {
			if err := os.WriteFile(filepath.Join(clientDir, id), []byte(id), 0o644); err != nil {
				t.Fatalf("plant %s: %v", id, err)
			}
		}

		resp, _ := mustPostForm(t, c, ts.URL+"/clients/laptop/snapshots/bulk-delete", url.Values{"ids": ids})
		loc, page := followFlash(t, c, ts, resp)
		if !strings.Contains(loc, "nothing was deleted") || !strings.Contains(loc, "invalid snapshot id") {
			t.Fatalf("ids=%q: flash = %q, want a refusal naming the bad id", ids, loc)
		}
		if !strings.Contains(page, "alert-error") || !strings.Contains(page, "invalid snapshot id") {
			t.Errorf("ids=%q: rendered page shows no refusal reason:\n%s", ids, truncate([]byte(page), 2000))
		}
		for _, id := range []string{snap1, snap2} {
			if data, err := os.ReadFile(filepath.Join(clientDir, id)); err != nil || string(data) != id {
				t.Errorf("ids=%q: archive %q gone or changed (err=%v)", ids, id, err)
			}
		}
		if _, err := os.Stat(clientDir); err != nil {
			t.Fatalf("ids=%q: client dir itself was removed: %v", ids, err)
		}
	}
}

// TestSingleSnapshotDeleteRefusesNonNameIDs keeps the URL route and the form
// route on the same rule. The router will not hand "." or ".." to the handler,
// but a mode-shaped id that is not a name (or the wrong mode's shape) must still
// be refused rather than attempted — and the refusal must name the id on the
// page instead of answering a bare 404.
func TestSingleSnapshotDeleteRefusesNonNameIDs(t *testing.T) {
	srv, c, ts := deleteStack(t)
	_, clientDir := makeClient(t, srv, c, ts, "nas", model.ModeRsync)
	plantRsyncSnapshots(t, clientDir, "20260101T000000Z", "20260102T000000Z")

	for _, id := range []string{"not-a-snapshot", "backup-20260101T000000Z-000.tar.gz"} {
		resp, err := c.PostForm(ts.URL+"/clients/nas/snapshots/"+id+"/delete", nil)
		if err != nil {
			t.Fatalf("post %q: %v", id, err)
		}
		loc, page := followFlash(t, c, ts, resp)
		if !strings.Contains(loc, "?err=") || !strings.Contains(loc, id) {
			t.Errorf("delete %q: Location = %q, want a ?err= flash naming the id", id, loc)
		}
		if !strings.Contains(page, "alert-error") || !strings.Contains(page, id) {
			t.Errorf("delete %q: rendered page does not name the id:\n%s", id, truncate([]byte(page), 2000))
		}
	}
	assertRsyncStoreIntact(t, clientDir, "20260101T000000Z", "20260102T000000Z")
}
