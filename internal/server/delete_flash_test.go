package server

// A refused or failed delete used to be invisible from the operator's chair: a
// bare 404 for an id the validator rejects, and a flat "delete failed" for a
// filesystem error that only reached the container log. These tests assert on
// the reason the operator reads on the page — not just on the redirect target —
// because the defect was that the page carried no reason at all.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// TestSnapshotDeleteFilesystemErrorIsLegible drives the single-hot-delete route
// against a directory the process cannot write, which is the operator's actual
// report ("I clicked delete and nothing happened"): the failure must come back
// as a flash carrying the underlying error, never as a bare redirect to
// "delete failed" with the mechanism stranded in the log.
func TestSnapshotDeleteFilesystemErrorIsLegible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so there is no EACCES to report")
	}
	const id = "backup-20260101T000000Z-000.tar.gz"

	srv, c, ts := deleteStack(t)
	_, clientDir := makeClient(t, srv, c, ts, "ro", model.ModeTarGz)
	archive := filepath.Join(clientDir, id)
	if err := os.WriteFile(archive, []byte("tarball"), 0o644); err != nil {
		t.Fatalf("plant archive: %v", err)
	}
	// Removing an entry needs write permission on its directory.
	if err := os.Chmod(clientDir, 0o555); err != nil {
		t.Fatalf("chmod client dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(clientDir, 0o755) })

	resp, err := c.PostForm(ts.URL+"/clients/ro/snapshots/"+id+"/delete", nil)
	if err != nil {
		t.Fatalf("post delete: %v", err)
	}
	loc, page := followFlash(t, c, ts, resp)

	// The flash must carry the mechanism, and the path in it is what identifies
	// the failing tier — these are admin-only pages.
	if !strings.Contains(loc, "delete failed") {
		t.Errorf("Location = %q, want a delete-failed flash", loc)
	}
	if !strings.Contains(loc, "permission denied") {
		t.Errorf("Location = %q, want the underlying error text", loc)
	}
	if !strings.Contains(loc, archive) {
		t.Errorf("Location = %q, want the failing path %q", loc, archive)
	}
	// ...and the operator must actually see it.
	if !strings.Contains(page, "alert-error") ||
		!strings.Contains(page, "delete failed") ||
		!strings.Contains(page, "permission denied") ||
		!strings.Contains(page, id) {
		t.Errorf("rendered page does not show the delete failure:\n%s", truncate([]byte(page), 3000))
	}
	// Legibility only: the refused delete left the archive exactly where it was.
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("archive gone after a failed delete: %v", err)
	}
}

// TestBulkDeletePartialFailureNamesIDsAndCount deletes a batch where exactly one
// member cannot be removed. The rest must still go (that is the existing
// behaviour) and the flash must report both the count that succeeded and the id
// that failed, with a cause — "some deletes failed" told the operator nothing.
func TestBulkDeletePartialFailureNamesIDsAndCount(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so there is no EACCES to report")
	}
	const (
		locked = "20260101T000000Z"
		plain  = "20260102T000000Z"
		latest = "20260103T000000Z"
	)

	srv, c, ts := deleteStack(t)
	_, clientDir := makeClient(t, srv, c, ts, "nas", model.ModeRsync)
	// newest is `latest`, which rsync mode refuses to delete — keep it out of
	// the batch so the only failure is the permission one under test.
	plantRsyncSnapshots(t, clientDir, locked, plain, latest)

	// Removing `locked` needs write permission inside it; making one subdir
	// read-only makes os.RemoveAll fail with EACCES and leaves the snapshot.
	pinned := filepath.Join(clientDir, "snapshots", locked, "pinned")
	if err := os.MkdirAll(pinned, 0o755); err != nil {
		t.Fatalf("plant pinned dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pinned, "payload.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("plant pinned payload: %v", err)
	}
	if err := os.Chmod(pinned, 0o555); err != nil {
		t.Fatalf("chmod pinned dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(pinned, 0o755) })

	resp, _ := mustPostForm(t, c, ts.URL+"/clients/nas/snapshots/bulk-delete",
		url.Values{"ids": {locked, plain}})
	loc, page := followFlash(t, c, ts, resp)

	for _, want := range []string{
		"deleted 1 of 2 snapshots",
		locked,
		"permission denied",
	} {
		if !strings.Contains(loc, want) {
			t.Errorf("flash %q does not contain %q", loc, want)
		}
		if !strings.Contains(page, want) {
			t.Errorf("rendered page does not contain %q:\n%s", want, truncate([]byte(page), 3000))
		}
	}
	if _, err := os.Stat(filepath.Join(clientDir, "snapshots", plain)); !os.IsNotExist(err) {
		t.Errorf("the deletable member of the batch was not deleted (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(clientDir, "snapshots", locked)); err != nil {
		t.Errorf("the snapshot whose delete failed is gone anyway: %v", err)
	}
	assertRsyncStoreIntact(t, clientDir, latest)
}

// installRcloneStub puts a stub `rclone` first on PATH that appends each
// invocation to a log and fails any call whose arguments mention failFor.
func installRcloneStub(t *testing.T, failFor string) (callsPath string) {
	t.Helper()
	dir := t.TempDir()
	callsPath = filepath.Join(dir, "rclone-calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + callsPath + "\n"
	if failFor != "" {
		script += "case \"$*\" in\n" +
			"  *" + failFor + "*)\n" +
			"    printf 'Failed to delete file: object not found\\n' >&2\n" +
			"    exit 1\n" +
			"    ;;\n" +
			"esac\n"
	}
	script += "exit 0\n"
	bin := filepath.Join(dir, "rclone")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("install rclone stub: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return callsPath
}

// TestBulkDeleteOffsiteObjectsEndToEnd is the path that was never exercised from
// the UI: selecting offsite objects and pressing Delete must reach rclone once
// per object, drop the tracking records, and land on the page with a visible
// success flash.
func TestBulkDeleteOffsiteObjectsEndToEnd(t *testing.T) {
	const (
		obj1 = "20260101T000000Z"
		obj2 = "20260102T000000Z"
	)
	calls := installRcloneStub(t, "")

	srv, c, ts := deleteStack(t)
	client, _ := makeClient(t, srv, c, ts, "nas", model.ModeRsync)
	ctx := context.Background()
	if err := srv.st.UpdateClientOffsite(ctx, client.ID, "gdrive", "", "", "all", false); err != nil {
		t.Fatalf("set offsite remote: %v", err)
	}
	for _, id := range []string{obj1, obj2} {
		if err := srv.st.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: client.ID, SnapshotID: id, Remote: "gdrive", Bytes: 10,
		}); err != nil {
			t.Fatalf("record offsite %s: %v", id, err)
		}
	}

	resp, _ := mustPostForm(t, c, ts.URL+"/clients/nas/offsite/bulk-delete",
		url.Values{"ids": {obj1, obj2}})
	loc, page := followFlash(t, c, ts, resp)

	if !strings.Contains(loc, "offsite objects deleted") {
		t.Errorf("Location = %q, want the success flash", loc)
	}
	if !strings.Contains(page, "alert-ok") || !strings.Contains(page, "offsite objects deleted") {
		t.Errorf("rendered page shows no success flash:\n%s", truncate([]byte(page), 3000))
	}
	callsBody, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read rclone calls: %v", err)
	}
	for _, id := range []string{obj1, obj2} {
		want := "deletefile gdrive:nas/" + id + ".tar.gz"
		if !strings.Contains(string(callsBody), want) {
			t.Errorf("rclone was not asked to %q; calls:\n%s", want, callsBody)
		}
	}
	objs, err := srv.st.ListOffsiteObjects(ctx, client.ID)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 0 {
		t.Errorf("offsite records left behind after a successful delete: %+v", objs)
	}
	if strings.Contains(page, obj1) || strings.Contains(page, obj2) {
		t.Errorf("deleted offsite objects are still listed on the page")
	}
}

// TestBulkDeleteOffsiteFailureNamesIDsAndCount covers the offsite leg's failure
// path: it had a flash, but a useless one. The flash must name the objects whose
// deletion failed, give the first cause, and say what did succeed — and name an
// id it refused to address rather than dropping it silently.
func TestBulkDeleteOffsiteFailureNamesIDsAndCount(t *testing.T) {
	const (
		ok   = "20260101T000000Z"
		bad  = "20260102T000000Z"
		junk = ".."
	)
	installRcloneStub(t, bad)

	srv, c, ts := deleteStack(t)
	client, _ := makeClient(t, srv, c, ts, "nas", model.ModeRsync)
	ctx := context.Background()
	if err := srv.st.UpdateClientOffsite(ctx, client.ID, "gdrive", "", "", "all", false); err != nil {
		t.Fatalf("set offsite remote: %v", err)
	}
	for _, id := range []string{ok, bad} {
		if err := srv.st.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: client.ID, SnapshotID: id, Remote: "gdrive", Bytes: 10,
		}); err != nil {
			t.Fatalf("record offsite %s: %v", id, err)
		}
	}

	resp, _ := mustPostForm(t, c, ts.URL+"/clients/nas/offsite/bulk-delete",
		url.Values{"ids": {ok, bad, junk}})
	loc, page := followFlash(t, c, ts, resp)

	for _, want := range []string{
		"deleted 1 of 3 selected offsite objects",
		bad,
		"object not found",
		"skipped invalid ids",
	} {
		if !strings.Contains(loc, want) {
			t.Errorf("flash %q does not contain %q", loc, want)
		}
		if !strings.Contains(page, want) {
			t.Errorf("rendered page does not contain %q:\n%s", want, truncate([]byte(page), 3000))
		}
	}
	if !strings.Contains(page, "alert-error") {
		t.Errorf("offsite failure rendered as a non-error flash")
	}
}
