//go:build linux

package rsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// The real-transfer proofs for the ownership setting. Two things have to be true
// and neither can be asserted from argv alone:
//
//   - the server-side forced command, with --fake-super injected, is accepted by
//     rrsync and makes the unprivileged receiver store the source's uid/gid in a
//     `user.rsync.%stat` xattr instead of chowning;
//   - a restore through the same command puts the real owner back when the
//     restoring end is plain and root — no --fake-super on the receiving side,
//     which would write a new pseudostat instead of chowning.
//
// Both need a real rsync AND a real rrsync (the Debian rsync package ships it),
// so they skip on a host without them, like the other client-path tests here. The
// ownership notes and the xattr-failure classifier are unit-tested separately in
// ownership_test.go, which needs neither binary.

// requireRrsync skips unless the restricted-rsync jail is installed: it is what
// authorized_keys actually forces, so a proof without it would be a proof of a
// different command.
func requireRrsync(t *testing.T) {
	t.Helper()
	requireRsync(t)
	if _, err := exec.LookPath("rrsync"); err != nil {
		t.Skip("rrsync is not on PATH (the Debian rsync package ships it): skipping the forced-command proof")
	}
	// Debian's rrsync is a script with RSYNC = '/usr/bin/rsync' baked in, so it
	// only works where rsync is installed system-wide. A host running a
	// hand-unpacked rsync has the binary on PATH and not there; saying which half
	// is missing is the difference between "skipped" and "broken".
	if _, err := os.Stat("/usr/bin/rsync"); err != nil {
		t.Skipf("rrsync execs /usr/bin/rsync, which this host does not have (%v): install the distro's rsync to run the forced-command proof", err)
	}
	if _, err := os.Stat(wrapperPath(t)); err != nil {
		t.Fatalf("the shipped wrapper is missing: %v", err)
	}
}

// fakeSSHForced stands in for ssh the way sshd does for a key with a forced
// command: it does not run the client's remote command as an executable, it hands
// its text to the wrapper as SSH_ORIGINAL_COMMAND, with the working directory at
// the jail root (rrsync's confinement). That is exactly why the client cannot ask
// for --fake-super itself — this is the seam it is injected at.
func fakeSSHForced(t *testing.T, jail string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is needed for the ssh stand-in")
	}
	if err := os.MkdirAll(filepath.Join(jail, "snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapper := wrapperPath(t)
	binDir := t.TempDir()
	script := `#!/bin/bash
# stand-in for sshd with a forced command:
JAIL='` + jail + `'
WRAPPER='` + wrapper + `'
while [ $# -gt 0 ]; do
	case "$1" in
		-i|-p|-o|-l) shift 2 ;;
		-*) shift ;;
		*) break ;;
	esac
done
[ $# -gt 0 ] || { echo "fake ssh: no host" >&2; exit 96; }
shift
[ $# -gt 0 ] || { echo "fake ssh: no remote command" >&2; exit 98; }
SSH_ORIGINAL_COMMAND="$*"
export SSH_ORIGINAL_COMMAND
cd "$JAIL" || exit 97
exec "$WRAPPER" "$JAIL"
`
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
}

// ownerOf returns a path's uid and gid.
func ownerOf(t *testing.T, path string) (int, int) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("this platform cannot report a file's owner")
	}
	return int(st.Uid), int(st.Gid)
}

// mustChownTo marks a file as belonging to somebody other than the receiver,
// and skips the test when the process cannot (an unprivileged test run cannot
// create the very condition under test).
func mustChownTo(t *testing.T, path string, uid, gid int) {
	t.Helper()
	if err := os.Chown(path, uid, gid); err != nil {
		if os.IsPermission(err) {
			t.Skipf("cannot chown to %d:%d without root (%v): the source cannot be made to differ from the receiver", uid, gid, err)
		}
		t.Fatalf("chown %s to %d:%d: %v", path, uid, gid, err)
	}
}

// userXattr reads one extended attribute, or "" when it is not there.
func userXattr(t *testing.T, path, name string) string {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := unix.Lgetxattr(path, name, buf)
	if err != nil {
		if err == unix.ENODATA || err == unix.ENOTSUP || err == unix.EOPNOTSUPP {
			return ""
		}
		t.Fatalf("lgetxattr %s on %s: %v", name, path, err)
	}
	return string(buf[:n])
}

// The core assertion of the card: a file owned by 1000:1000 arrives in the
// snapshot owned by the receiver, carrying its real owner in the xattr.
func TestOwnershipSettingRecordsTheSourceOwnerInTheSnapshot(t *testing.T) {
	requireRrsync(t)
	jail := t.TempDir()
	fakeSSHForced(t, jail)

	src := t.TempDir()
	file := filepath.Join(src, "owned.txt")
	if err := os.WriteFile(file, []byte("owned by a real user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustChownTo(t, file, 1000, 1000)

	opts, logBuf := backupOpts(t, src, false)
	opts.RecordOwnership = true
	res, err := Mode{}.Backup(context.Background(), opts)
	if err != nil {
		// A receiver-side rrsync that refuses --fake-super is the failure this
		// test exists to catch, so say what rsync said.
		t.Fatalf("backup with the ownership setting on: %v\nlog:\n%s", err, logBuf.String())
	}
	snapFile := filepath.Join(jail, "snapshots", res.SnapshotID, "owned.txt")
	if _, err := os.Stat(snapFile); err != nil {
		t.Fatalf("the file did not arrive: %v", err)
	}

	// 1. The hot tier stays owned by the receiver: that is what keeps the app's
	//    read and delete rights, and why the receiver is not root.
	uid, gid := ownerOf(t, snapFile)
	if uid != os.Geteuid() || gid != os.Getegid() {
		t.Fatalf("snapshot file is owned by %d:%d, want the receiver %d:%d", uid, gid, os.Geteuid(), os.Getegid())
	}
	// 2. The real owner is recorded, in the place rsync reads it back from.
	if got := userXattr(t, snapFile, "user.rsync.%stat"); !strings.Contains(got, "1000:1000") {
		t.Fatalf("user.rsync.%%stat = %q, want it to name 1000:1000", got)
	}
	// 3. The run says what it stored, so an operator reading the log is not
	//    surprised by xattrs in the tree.
	if !strings.Contains(logBuf.String(), "user.rsync.%stat") {
		t.Errorf("the run log does not mention what it recorded:\n%s", logBuf.String())
	}
}

// The restore half: a plain root receiver, no --fake-super on its own side, gets
// the original uid, gid and mode back. The flag comes from the server's command,
// which is the asymmetry that makes this work at all.
func TestOwnershipRestorePutsTheOriginalOwnerBack(t *testing.T) {
	requireRrsync(t)
	if os.Geteuid() != 0 {
		t.Skip("the restoring end must be root to chown: run this as root to prove the round trip")
	}
	jail := t.TempDir()
	fakeSSHForced(t, jail)

	src := t.TempDir()
	file := filepath.Join(src, "owned.txt")
	if err := os.WriteFile(file, []byte("round trip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustChownTo(t, file, 1000, 1000)

	opts, logBuf := backupOpts(t, src, false)
	opts.RecordOwnership = true
	res, err := Mode{}.Backup(context.Background(), opts)
	if err != nil {
		t.Fatalf("backup: %v\nlog:\n%s", err, logBuf.String())
	}

	// The restore is a plain rsync pull: the stand-in runs the server's forced
	// command as the sender, and the local receiver is whatever rsync defaults to
	// (root here) with no --fake-super of its own.
	restore := t.TempDir()
	target := "backitup@fake-ingest:snapshots/" + res.SnapshotID + "/"
	args := []string{"-a", "--numeric-ids", "-e", "ssh", target, restore + "/"}
	cmd := exec.Command("rsync", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restore %v: %v\n%s", args, err, out)
	}

	restored := filepath.Join(restore, "owned.txt")
	uid, gid := ownerOf(t, restored)
	if uid != 1000 || gid != 1000 {
		t.Fatalf("restored owner is %d:%d, want the source's 1000:1000", uid, gid)
	}
	if got := perm(t, restored); got != 0o600 {
		t.Fatalf("restored mode is %04o, want 0600", got)
	}
	// A real chown leaves no pseudostat behind: if one is still here, the
	// receiver was fake-super too and this is a copy of the snapshot, not a
	// restore.
	if got := userXattr(t, restored, "user.rsync.%stat"); got != "" {
		t.Fatalf("the restored file still carries a pseudostat %q: the receiving side must not run --fake-super", got)
	}
}

// With the setting off nothing changes: no xattr appears in the snapshot tree,
// and the run log says nothing about ownership.
func TestOwnershipSettingOffLeavesNoPseudostat(t *testing.T) {
	requireRrsync(t)
	jail := t.TempDir()
	fakeSSHForced(t, jail)

	src := t.TempDir()
	file := filepath.Join(src, "plain.txt")
	if err := os.WriteFile(file, []byte("today's shape\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts, logBuf := backupOpts(t, src, false)
	res, err := Mode{}.Backup(context.Background(), opts)
	if err != nil {
		t.Fatalf("backup: %v\nlog:\n%s", err, logBuf.String())
	}
	snapFile := filepath.Join(jail, "snapshots", res.SnapshotID, "plain.txt")
	if _, err := os.Stat(snapFile); err != nil {
		t.Fatalf("the file did not arrive: %v", err)
	}
	if got := userXattr(t, snapFile, "user.rsync.%stat"); got != "" {
		t.Fatalf("a client with the setting off stored a pseudostat %q: the snapshot shape must not change", got)
	}
	if strings.Contains(logBuf.String(), "user.rsync.%stat") {
		t.Errorf("a client with the setting off was told about ownership:\n%s", logBuf.String())
	}
}
