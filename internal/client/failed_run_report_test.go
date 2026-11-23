package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"

	// the run loop resolves the mode through the registry, which cmd/client
	// populates with blank imports; the test needs the same one.
	_ "github.com/th0rn0/backitup/internal/mode/rsync"
)

// A mode may hand back counts together with its error: rsync --stats reports the
// file list it *intended* to copy ("Number of files: 5" above four unreadable
// .qcow2 files). Those are not a backup size, and a failed run must not report
// them as one — a plausible "synced 5 files" on the dashboard is how four empty
// files pass as a backup.
func TestBuildStatusFailedReportsNoCounts(t *testing.T) {
	s := buildStatus(mode.BackupResult{Bytes: 4096, Files: 5, SnapshotID: "20260101T000000Z"}, errors.New("rsync: exit status 23"))
	if s.Status != string(model.StatusFailed) {
		t.Fatalf("status = %q, want failed", s.Status)
	}
	if s.Bytes != 0 || s.Files != 0 {
		t.Fatalf("a failed run must report no counts, got %d files / %d bytes", s.Files, s.Bytes)
	}
	// The id is left alone: a mode can fail after storing a usable snapshot (the
	// latest pointer did not move), and naming it is still true.
	if s.SnapshotID != "20260101T000000Z" {
		t.Fatalf("SnapshotID = %q, want it preserved", s.SnapshotID)
	}
}

// TestRunReportsTheFailureAndItsErrorText drives the real client run loop against
// a stand-in rsync that reproduces the reported failure (sender-side EACCES, exit
// 23, zero bytes copied) and a stand-in API server.
//
// What this pins is the reporting chain end to end: the run is reported `failed`,
// with the error text in its log tail (which is all the dashboard shows), with no
// counts, and the failed snapshot directory is marked incomplete. What a real
// rsync leaves behind is covered against real rsync in internal/mode/rsync.
func TestRunReportsTheFailureAndItsErrorText(t *testing.T) {
	root := t.TempDir() // stands in for the client's directory on the server
	binDir := t.TempDir()
	shim := `#!/bin/bash
# stand-in for rsync: the main sync fails the way a sender-side EACCES does,
# the marker write succeeds.
dest="${@: -1}"
src="${@: -2:1}"
case "$dest" in
	*:snapshots/*/) ;;
	*) exit 0 ;;
esac
dir="$FAKE_RSYNC_ROOT/${dest#*:}"
mkdir -p "$dir" || exit 23
if [ -d "$src" ]; then
	: > "$dir/drone-runner.qcow2"
	: > "$dir/gitlab-runner.qcow2"
	echo "Number of files: 3 (reg: 2, dir: 1)" >&2
	echo "Literal data: 0 bytes" >&2
	echo 'rsync: [sender] send_files failed to open "/source/drone-runner.qcow2": Permission denied (13)' >&2
	exit 23
fi
cp "$src" "$dir/" || exit 23
exit 0
`
	if err := os.WriteFile(filepath.Join(binDir, "rsync"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RSYNC_ROOT", root)
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	var (
		mu       sync.Mutex
		statuses []StatusReq
	)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/config":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"mode":"rsync","excludes":[],"skip_symlinks":false,"has_previous_snapshot":false}`))
		case "/api/v1/status":
			var req StatusReq
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode status: %v", err)
			}
			mu.Lock()
			statuses = append(statuses, req)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"run_id":7}`))
		default:
			t.Errorf("unexpected request to %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "readable.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		APIBase: ts.URL, Token: "t", SSHServer: "fake-ingest:2222", SSHKey: "/dev/null",
		Source: src, Mode: model.ModeRsync, InsecureTLS: true, InsecureSSH: true, Quiet: true,
	}
	if err := Run(context.Background(), cfg, filepath.Join(t.TempDir(), "run.lock")); err == nil {
		t.Fatal("Run must return the backup's error")
	} else {
		t.Logf("Run returned: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(statuses) != 2 {
		t.Fatalf("posted %d statuses, want a running one and a final one: %+v", len(statuses), statuses)
	}
	if statuses[0].Status != string(model.StatusRunning) {
		t.Fatalf("first status = %q, want running", statuses[0].Status)
	}
	final := statuses[1]
	if final.Status != string(model.StatusFailed) {
		t.Fatalf("final status = %q, want failed", final.Status)
	}
	if final.Bytes != 0 || final.Files != 0 {
		t.Fatalf("a failed run must report no counts: %d files / %d bytes", final.Files, final.Bytes)
	}
	for _, want := range []string{"exit status 23", "Permission denied (13)", "NOT a backup", "marked incomplete"} {
		if !strings.Contains(final.LogTail, want) {
			t.Errorf("the reported log tail must contain %q, got:\n%s", want, final.LogTail)
		}
	}

	// And the snapshot the failed run left is marked, so nothing treats it as a
	// backup while the operator fixes the source.
	snapsDir := filepath.Join(root, "snapshots")
	entries, err := os.ReadDir(snapsDir)
	if err != nil {
		t.Fatalf("read %s: %v", snapsDir, err)
	}
	marked := 0
	for _, e := range entries {
		if _, statErr := os.Stat(filepath.Join(snapsDir, e.Name(), ".backitup-partial")); statErr == nil {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("want exactly one marked snapshot directory under %s, found %d (%v)", snapsDir, marked, entries)
	}
}
