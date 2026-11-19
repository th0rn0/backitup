package server

// Restore is the one claim a backup product makes that nothing in this repo
// proved: that a snapshot can be turned back into the files that went in. These
// tests take the real producer (archiveutil.TarGz — what the targz client streams
// over SSH), hand the bytes to the real download handler, and put them back on
// disk the way an operator's `tar -xzf` would.
//
// They deliberately do not test the handler in isolation: a handler that answers
// 200 with an empty body would pass an assertion on status alone.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/archiveutil"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// restoreStack is ingestStack plus the *Server, so a test can compare the
// directory the handler reads with the one the test wrote into. Rebuilding the
// wiring in a second server would give the test a different backup dir and a
// passing test that proves nothing.
func restoreStack(t *testing.T) (*store.Store, *httptest.Server, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(st, false)
	srv.dbTimeout = testDBTimeout
	tmp := t.TempDir()
	srv.authKeysPath = filepath.Join(tmp, "authkeys", "authorized_keys")
	srv.backupBaseDir = filepath.Join(tmp, "backups")
	srv.publicHost = "backup.test:2222"
	srv.clientImage = "th0rn0/backitup-client:test"

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, ts, srv
}

// fileState is what gets compared: content, mode, and (for symlinks) the target.
type fileState struct {
	sha256 string
	mode   os.FileMode
	link   string
	mtime  time.Time
}

// writeSourceTree creates the awkward things a real home directory contains:
// a nested path, an executable bit, a symlink, an empty directory, and a name
// with a space in it.
func writeSourceTree(t *testing.T, root string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "docs", "nested"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "empty"), 0o755))
	must(os.WriteFile(filepath.Join(root, "notes.txt"), []byte("the quick brown fox\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "docs", "nested", "with space.txt"), []byte("spaces are legal\n"), 0o644))
	must(os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\necho restored\n"), 0o755))
	must(os.Symlink("notes.txt", filepath.Join(root, "notes-link")))
}

func snapshotTree(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			out[rel] = fileState{mode: info.Mode(), link: target}
		case d.IsDir():
			out[rel] = fileState{mode: info.Mode().Perm() | os.ModeDir}
		default:
			sum, err := sha256File(p)
			if err != nil {
				return err
			}
			out[rel] = fileState{sha256: sum, mode: info.Mode().Perm(), mtime: info.ModTime()}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// diffTrees reports differences in both directions, so a failure names what was
// lost instead of only saying the maps differ.
func diffTrees(want, got map[string]fileState) []string {
	var diffs []string
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	sort.Strings(all)
	for _, k := range all {
		w, inWant := want[k]
		g, inGot := got[k]
		switch {
		case !inGot:
			diffs = append(diffs, "missing after restore: "+k)
		case !inWant:
			diffs = append(diffs, "appeared after restore: "+k)
		case w.sha256 != g.sha256:
			diffs = append(diffs, fmt.Sprintf("content differs: %s (want sha %s, got %s)", k, short(w.sha256), short(g.sha256)))
		case w.mode != g.mode:
			diffs = append(diffs, fmt.Sprintf("mode: %s want %v got %v", k, w.mode, g.mode))
		case w.link != g.link:
			diffs = append(diffs, fmt.Sprintf("symlink target: %s want %q got %q", k, w.link, g.link))
		case !w.mtime.IsZero() && absDuration(w.mtime.Sub(g.mtime)) > 2*time.Second:
			diffs = append(diffs, fmt.Sprintf("mtime: %s want %s got %s", k, w.mtime.Format(time.RFC3339), g.mtime.Format(time.RFC3339)))
		}
	}
	return diffs
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// untarGz extracts an archive to dst and returns the top-level directory it
// created: the client archives `basename(sourceDir)/...`, so a restore lands in
// a directory rather than spilling into the caller's cwd.
func untarGz(t *testing.T, r io.Reader, dst string) string {
	t.Helper()
	gz, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	top := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar header: %v", err)
		}
		clean := filepath.Clean(hdr.Name)
		if strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			t.Fatalf("archive escapes the extraction root: %q", hdr.Name)
		}
		if top == "" {
			top = strings.SplitN(clean, string(filepath.Separator), 2)[0]
		}
		target := filepath.Join(dst, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				t.Fatalf("mkdir %s: %v", target, err)
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", filepath.Dir(target), err)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				t.Fatalf("symlink %s: %v", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", filepath.Dir(target), err)
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode))
			if err != nil {
				t.Fatalf("create %s: %v", target, err)
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				t.Fatalf("write %s: %v", target, err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close %s: %v", target, err)
			}
			if err := os.Chmod(target, os.FileMode(hdr.Mode)); err != nil {
				t.Fatalf("chmod %s: %v", target, err)
			}
		}
	}
	if top == "" {
		t.Fatal("archive was empty: nothing to restore")
	}
	return top
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// fetchSnapshot performs an authenticated GET against the download handler and
// asserts the headers an operator's shell depends on.
func fetchSnapshot(t *testing.T, c *http.Client, url string) []byte {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (%s)", url, resp.StatusCode, truncate(body, 200))
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/gzip" {
		t.Errorf("Content-Type = %q, want application/gzip — curl -O would misname the file", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", cd)
	}
	if len(body) == 0 {
		t.Fatalf("GET %s returned an empty body", url)
	}
	return body
}

// TestRestoreTarGzSnapshot: tar.gz client → archiveutil.TarGz → download handler
// → extract → identical tree.
func TestRestoreTarGzSnapshot(t *testing.T) {
	st, ts, srv := restoreStack(t)
	c := loggedInClient(t, st, ts)

	resp, body := mustPostForm(t, c, ts.URL+"/clients", map[string][]string{
		"name": {"laptop-docs"}, "mode": {"targz"}, "retention_days": {"7"},
	})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create client = %d: %s", resp.StatusCode, truncate([]byte(body), 300))
	}

	client, err := st.GetClientBySlug(context.Background(), "laptop-docs")
	if err != nil || client == nil {
		t.Fatalf("load client: %v (client=%v)", err, client)
	}
	if client.Mode != model.ModeTarGz {
		t.Fatalf("client mode = %q, want targz", client.Mode)
	}

	clientDir, err := srv.clientDir(*client)
	if err != nil {
		t.Fatalf("clientDir: %v", err)
	}
	if err := os.MkdirAll(clientDir, 0o755); err != nil {
		t.Fatal(err)
	}

	source := filepath.Join(t.TempDir(), "documents")
	writeSourceTree(t, source)

	// The real producer, not a hand-rolled tar.
	var buf bytes.Buffer
	if _, _, err := archiveutil.TarGz(context.Background(), &buf, source, nil, false, nil); err != nil {
		t.Fatalf("produce archive: %v", err)
	}
	snapID := "backup-20260101T000000Z-000.tar.gz"
	if err := os.WriteFile(filepath.Join(clientDir, snapID), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	got := fetchSnapshot(t, c, ts.URL+"/clients/laptop-docs/snapshots/"+snapID)

	// archiveutil.TarGz stores paths relative to the source root (hdr.Name = rel),
	// so a targz restore lands directly in the directory you extract into — this is
	// the behaviour the README's restore instructions have to describe.
	dst := t.TempDir()
	top := untarGz(t, bytes.NewReader(got), dst)
	if top != "docs" {
		t.Fatalf("expected the archive to start with the source's first entry, got %q", top)
	}

	if diffs := diffTrees(snapshotTree(t, source), snapshotTree(t, dst)); len(diffs) > 0 {
		t.Fatalf("restored tree differs from the source (%d differences):\n%s",
			len(diffs), strings.Join(diffs, "\n"))
	}
}

// TestRestoreRsyncSnapshot: the rsync path keeps no archive — the handler builds
// a tar.gz on the fly from the snapshot directory, so nothing else in the suite
// exercises that code at all.
func TestRestoreRsyncSnapshot(t *testing.T) {
	st, ts, srv := restoreStack(t)
	c := loggedInClient(t, st, ts)

	resp, body := mustPostForm(t, c, ts.URL+"/clients", map[string][]string{
		"name": {"nas-photos"}, "mode": {"rsync"}, "retention_days": {"7"},
	})
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create client = %d: %s", resp.StatusCode, truncate([]byte(body), 300))
	}

	client, err := st.GetClientBySlug(context.Background(), "nas-photos")
	if err != nil || client == nil {
		t.Fatalf("load client: %v", err)
	}
	if client.Mode != model.ModeRsync {
		t.Fatalf("client mode = %q, want rsync", client.Mode)
	}

	clientDir, err := srv.clientDir(*client)
	if err != nil {
		t.Fatalf("clientDir: %v", err)
	}
	snapID := "20260101T000000Z"
	snapDir := filepath.Join(clientDir, "snapshots", snapID)
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSourceTree(t, snapDir)
	// rsync snapshots are a real directory plus a `latest` symlink; List skips it.
	if err := os.Symlink(snapID, filepath.Join(clientDir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}

	got := fetchSnapshot(t, c, ts.URL+"/clients/nas-photos/snapshots/"+snapID)

	dst := t.TempDir()
	restored := filepath.Join(dst, untarGz(t, bytes.NewReader(got), dst))

	if diffs := diffTrees(snapshotTree(t, snapDir), snapshotTree(t, restored)); len(diffs) > 0 {
		t.Fatalf("rsync snapshot did not round-trip (%d differences):\n%s",
			len(diffs), strings.Join(diffs, "\n"))
	}
}

// TestRestoreRefusesUnknownAndTraversingIDs: a backup tool that will hand out
// ../../etc/passwd on request is worse than one that cannot restore.
func TestRestoreRefusesUnknownAndTraversingIDs(t *testing.T) {
	st, ts, _ := restoreStack(t)
	c := loggedInClient(t, st, ts)

	if resp, body := mustPostForm(t, c, ts.URL+"/clients", map[string][]string{
		"name": {"laptop-docs"}, "mode": {"targz"}, "retention_days": {"7"},
	}); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create client = %d: %s", resp.StatusCode, truncate([]byte(body), 200))
	}

	// Plain unknown ids and percent-encoded traversal must both 404: the handler
	// rejects any id containing "..". (A literal "../" is path-cleaned by net/http
	// before the handler runs, so it is covered by the disclosure check below
	// rather than by a status assertion.)
	for _, id := range []string{
		"does-not-exist.tar.gz",
		"%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"..%2f..%2fetc%2fpasswd",
	} {
		resp, err := c.Get(ts.URL + "/clients/laptop-docs/snapshots/" + id)
		if err != nil {
			t.Fatalf("GET %q: %v", id, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("snapshot id %q: status = %d, want 404 (%s)", id, resp.StatusCode, truncate(body, 120))
		}
		if bytes.Contains(body, []byte("root:")) {
			t.Errorf("snapshot id %q returned /etc/passwd", id)
		}
	}

	// Whatever the mux does with a literal "..", no response may contain the file.
	resp, err := c.Get(ts.URL + "/clients/laptop-docs/snapshots/../../../../etc/passwd")
	if err != nil {
		t.Fatalf("literal traversal: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if bytes.Contains(body, []byte("root:")) {
		t.Fatalf("literal traversal returned /etc/passwd (status %d)", resp.StatusCode)
	}
}
