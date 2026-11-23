package rsync

import (
	"bytes"
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

// These tests run the CLIENT path — Mode.Backup — against a real rsync, with a
// stand-in `ssh` that executes the "remote" command locally inside a directory
// standing in for the client's directory on the server. That is the only way to
// exercise what a failed transfer actually leaves behind: the shape comes from
// rsync, not from the test.
//
// They skip when rsync is not installed, like the offsite end-to-end test does
// for rclone; the deterministic coverage of the same behaviour is in
// incomplete_snapshot_test.go, which needs nothing external.

func requireRsync(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not on PATH: install it to run the client-path proof of a failed run")
	}
}

// fakeSSH replaces the `ssh` binary on PATH with a stand-in that runs the remote
// command locally inside jail, so `host:snapshots/<ts>/` lands in
// jail/snapshots/<ts>/ exactly as rrsync would put it.
//
// The two rewrites are what make it behave like the real thing: the command runs
// with its working directory at the jail root (rrsync's path jail), and the
// absolute --link-dest anchor rsync is given (/snapshots/latest) is re-anchored
// the same way.
func fakeSSH(t *testing.T, jail string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is needed for the ssh stand-in")
	}
	// The server pre-creates <clientDir>/snapshots when a client is saved
	// (server.setupClientDirs: "rsync won't create the missing snapshots/
	// parent"), so the stand-in does too.
	if err := os.MkdirAll(filepath.Join(jail, "snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/bash
# stand-in for ssh: skip the transport options, drop the host, then run the
# "remote" rsync command locally inside the jail.
JAIL='` + jail + `'
while [ $# -gt 0 ]; do
	case "$1" in
		-i|-p|-o|-l) shift 2 ;;
		-*) shift ;;
		*) break ;;
	esac
done
[ $# -gt 0 ] || { echo "fake ssh: no host" >&2; exit 96; }
shift
args=()
for a in "$@"; do
	args+=("${a//\/snapshots/$JAIL/snapshots}")
done
cd "$JAIL" || exit 97
exec "${args[@]}"
`
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

func backupOpts(t *testing.T, src string, hasPrevious bool) (mode.BackupOpts, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return mode.BackupOpts{
		SourceDir:           src,
		HasPreviousSnapshot: hasPrevious,
		Logger:              log.New(buf, "", 0),
		SSHServer:           "fake-ingest:2222",
		SSHUser:             "backitup",
		SSHKey:              "/dev/null",
		InsecureSSH:         true, // the transport is the stand-in above
	}, buf
}

// The acceptance case: a run whose source holds a file rsync cannot read fails,
// and what it leaves behind is not a snapshot to anybody.
//
// The reported shape is reproduced for real — a file rsync's sender cannot open
// (permission denied), which is exit 23, "Literal data: 0 bytes" — and every
// consumer is then checked: List (what the offsite worker tars), VerifySnapshot
// (the shape test), and `latest` (which must still point at the previous good
// snapshot, because the run returns before flipLatest).
func TestBackupFailureLeavesNothingListable(t *testing.T) {
	requireRsync(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0000 file is still readable, so the sender-side EACCES cannot be reproduced")
	}
	server := t.TempDir() // the client's directory on the server (rrsync's jail)
	good := "20260101T000000Z"
	makeSnap(t, server, good, "the previous good backup")
	if err := os.Symlink(good, filepath.Join(server, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	fakeSSH(t, server)

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "readable.txt"), []byte("fine"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(src, "drone-runner.qcow2")
	if err := os.WriteFile(locked, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}

	opts, logBuf := backupOpts(t, src, true)
	res, err := (Mode{}).Backup(context.Background(), opts)
	if err == nil {
		t.Fatal("a source with an unreadable file must fail the run")
	}
	if !strings.Contains(err.Error(), "exit status 23") && !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("the reported error should carry rsync's own failure: %v", err)
	}

	// The run reports no counts: rsync's "Number of files: 5" is the file list it
	// intended to copy, not a backup size.
	if res.Files != 0 || res.Bytes != 0 || res.SnapshotID != "" {
		t.Fatalf("a failed run must report nothing: %+v", res)
	}

	// 1. Nothing new is listable — the offsite worker tars what List returns.
	snaps, lErr := Server{}.List(context.Background(), server)
	if lErr != nil {
		t.Fatalf("list: %v", lErr)
	}
	if len(snaps) != 1 || snaps[0].ID != good {
		t.Fatalf("List = %+v, want only the previous good snapshot %s", snaps, good)
	}

	// 2. `latest` did not move: the previous good snapshot is still protected.
	target, rErr := os.Readlink(filepath.Join(server, "snapshots", "latest"))
	if rErr != nil {
		t.Fatalf("readlink latest: %v", rErr)
	}
	if target != good {
		t.Fatalf("latest -> %q, want the previous good snapshot %q", target, good)
	}

	// 3. The previous good snapshot is untouched and still verifies.
	if vErr := (Server{}).VerifySnapshot(context.Background(), server, mode.Snapshot{ID: good}); vErr != nil {
		t.Fatalf("the previous good snapshot must still verify after a failed run: %v", vErr)
	}

	// 4. What rsync did leave is marked, so the lifecycle can reclaim it and the
	//    failure is legible from the filesystem alone.
	incomplete, iErr := Server{}.ListIncomplete(context.Background(), server)
	if iErr != nil {
		t.Fatalf("list incomplete: %v", iErr)
	}
	if len(incomplete) != 1 {
		t.Fatalf("ListIncomplete = %+v, want the one directory the failed run left", incomplete)
	}
	if incomplete[0].ID == good || incomplete[0].ID == "latest" {
		t.Fatalf("ListIncomplete returned %q, which is not a failed run's directory", incomplete[0].ID)
	}
	if vErr := (Server{}).VerifySnapshot(context.Background(), server, incomplete[0]); vErr == nil ||
		!strings.Contains(vErr.Error(), "incomplete") {
		t.Fatalf("the failed run's directory must not verify, got: %v", vErr)
	}

	// 5. The log says what happened and does not read like a synced snapshot.
	logText := logBuf.String()
	if !strings.Contains(logText, "NOT a backup") {
		t.Errorf("the run log must say the attempt is not a backup:\n%s", logText)
	}
	if strings.Contains(logText, "synced ") {
		t.Errorf("a failed run must not log a synced line:\n%s", logText)
	}
	if !strings.Contains(logText, "marked incomplete") {
		t.Errorf("the run log must record that the directory was marked:\n%s", logText)
	}
}

// The negative case, so the fix cannot become "reject empty sources": a source
// directory that really is empty backs up normally — rsync exits 0, `latest`
// flips, the snapshot is listed, and nothing marks it incomplete.
func TestBackupSucceedsOnEmptySource(t *testing.T) {
	requireRsync(t)
	server := t.TempDir()
	fakeSSH(t, server)
	src := t.TempDir() // empty on purpose

	opts, logBuf := backupOpts(t, src, false)
	res, err := (Mode{}).Backup(context.Background(), opts)
	if err != nil {
		t.Fatalf("an empty source directory is a valid backup, got: %v\nlog:\n%s", err, logBuf.String())
	}
	if res.SnapshotID == "" {
		t.Fatal("a successful run must report its snapshot id")
	}

	snaps, lErr := Server{}.List(context.Background(), server)
	if lErr != nil {
		t.Fatalf("list: %v", lErr)
	}
	if len(snaps) != 1 || snaps[0].ID != res.SnapshotID {
		t.Fatalf("List = %+v, want the empty source's snapshot %s", snaps, res.SnapshotID)
	}
	incomplete, iErr := Server{}.ListIncomplete(context.Background(), server)
	if iErr != nil || len(incomplete) != 0 {
		t.Fatalf("an empty source must not be marked incomplete: %+v, %v", incomplete, iErr)
	}
	target, rErr := os.Readlink(filepath.Join(server, "snapshots", "latest"))
	if rErr != nil || target != res.SnapshotID {
		t.Fatalf("latest -> %q (%v), want %q", target, rErr, res.SnapshotID)
	}
}

// A second, successful run must be unaffected by an earlier failure: the retry
// uses a new timestamp directory, hardlinks against `latest` (still the previous
// good snapshot), and lists normally.
func TestBackupAfterAFailedRunSucceeds(t *testing.T) {
	requireRsync(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0000 file is still readable")
	}
	server := t.TempDir()
	good := "20260101T000000Z"
	makeSnap(t, server, good, "the previous good backup")
	if err := os.Symlink(good, filepath.Join(server, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	fakeSSH(t, server)

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "data.txt"), []byte("real content"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(bad, "locked.qcow2")
	if err := os.WriteFile(locked, nil, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	opts, _ := backupOpts(t, bad, true)
	if _, err := (Mode{}).Backup(context.Background(), opts); err == nil {
		t.Fatal("expected the first run to fail")
	}

	// The operator fixes permissions on the source and re-runs.
	if err := os.Chmod(locked, 0o644); err != nil {
		t.Fatal(err)
	}
	opts, logBuf := backupOpts(t, bad, true)
	res, err := (Mode{}).Backup(context.Background(), opts)
	if err != nil {
		t.Fatalf("the retry must succeed: %v\nlog:\n%s", err, logBuf.String())
	}
	snaps, lErr := Server{}.List(context.Background(), server)
	if lErr != nil {
		t.Fatalf("list: %v", lErr)
	}
	if len(snaps) != 2 {
		t.Fatalf("List = %+v, want the previous good snapshot plus the retry", snaps)
	}
	if target, _ := os.Readlink(filepath.Join(server, "snapshots", "latest")); target != res.SnapshotID {
		t.Fatalf("latest -> %q, want the retry %q", target, res.SnapshotID)
	}
	if _, err := os.Stat(filepath.Join(server, "snapshots", res.SnapshotID, "data.txt")); err != nil {
		t.Fatalf("the retry's snapshot should hold the source: %v", err)
	}
}
