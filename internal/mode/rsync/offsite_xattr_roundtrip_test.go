package rsync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
	"golang.org/x/sys/unix"
)

// setXattr writes one extended attribute, skipping the test when the filesystem
// under t.TempDir cannot hold user attributes (a container on a volume that
// refuses them).
func setXattr(t *testing.T, path, name, value string) {
	t.Helper()
	if err := unix.Lsetxattr(path, name, []byte(value), 0); err != nil {
		if err == unix.ENOTSUP || err == unix.EOPNOTSUPP || err == unix.EPERM {
			t.Skipf("this filesystem cannot store user xattrs: %v", err)
		}
		t.Fatalf("lsetxattr %s on %s: %v", name, path, err)
	}
}

// getXattr reads an attribute back, without following a symlink.
func getXattr(t *testing.T, path, name string) (string, bool) {
	t.Helper()
	buf := make([]byte, 4096)
	n, err := unix.Lgetxattr(path, name, buf)
	if err == unix.ENODATA {
		return "", false
	}
	if err != nil {
		t.Fatalf("lgetxattr %s on %s: %v", name, path, err)
	}
	return string(buf[:n]), true
}

// gnuTarOrSkip requires GNU tar: the documented restore command relies on it
// spelling extended attributes as SCHILY.xattr PAX records.
func gnuTarOrSkip(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("tar is not installed")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil || !strings.Contains(string(out), "GNU tar") {
		t.Skipf("not GNU tar, so --xattrs means something else here: %q", string(out))
	}
	return path
}

// The tier boundary: a snapshot whose ownership lives in `user.rsync.%stat` on
// the hot store goes through the offsite archiver (Server.PrepareOffsite, the
// real code path a rsync client's snapshot takes on its way to B2) and comes
// back out with the attribute intact, using the command the README documents.
//
// Asserting on a PAX header alone would not prove this: the value has to survive
// `tar --xattrs`, which is the step that decides whether a restore from offsite
// still knows who owned the file.
func TestOffsiteArchiveCarriesFakeSuperOwnership(t *testing.T) {
	tarBin := gnuTarOrSkip(t)

	clientDir := t.TempDir()
	id := "20260101T000000Z"
	snapPath := filepath.Join(clientDir, "snapshots", id)
	if err := os.MkdirAll(filepath.Join(snapPath, "photos", "raw"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A uid/gid that is not this process's: 1000:1000 is the client's own
	// account, 10000:10000 is who the test (and, in the image, the app) runs as.
	// If the attribute were dropped or rewritten, the value below would differ.
	const pseudostat = "100640 0,0 1000:1000"
	for _, rel := range []string{"top.txt", "photos/raw/shot.cr2"} {
		p := filepath.Join(snapPath, rel)
		if err := os.WriteFile(p, []byte("payload"), 0o640); err != nil {
			t.Fatal(err)
		}
		setXattr(t, p, "user.rsync.%stat", pseudostat)
	}
	// The walk covers every entry, not just the ones the hot tier happens to
	// carry an attribute for: a directory attribute is as real as a file's.
	const dirStat = "40755 0,0 1000:1000"
	setXattr(t, filepath.Join(snapPath, "photos"), "user.rsync.%stat", dirStat)
	// And an entry with no attribute must not gain one.
	plain := filepath.Join(snapPath, "photos", "plain.txt")
	if err := os.WriteFile(plain, []byte("no owner of its own"), 0o644); err != nil {
		t.Fatal(err)
	}

	archive, err := Server{}.PrepareOffsite(context.Background(), clientDir, mode.Snapshot{ID: id})
	if err != nil {
		t.Fatalf("prepare offsite: %v", err)
	}
	defer os.Remove(archive)

	dst := t.TempDir()
	// This is the documented command: `tar --xattrs --xattrs-include='*' -xpf`,
	// with -z because backitup's offsite objects are gzipped.
	cmd := exec.Command(tarBin, "--xattrs", "--xattrs-include=*", "-xzpf", archive, "-C", dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("tar --xattrs --xattrs-include='*' -xzpf %s: %v\n%s", archive, err, out)
	}

	for _, rel := range []string{"top.txt", "photos/raw/shot.cr2"} {
		got, ok := getXattr(t, filepath.Join(dst, rel), "user.rsync.%stat")
		if !ok {
			t.Fatalf("%s came out of the archive without user.rsync.%%stat: ownership is gone at the B2 boundary", rel)
		}
		if got != pseudostat {
			t.Fatalf("%s: user.rsync.%%stat = %q, want %q", rel, got, pseudostat)
		}
		// Where the ownership lands: tar applies the archive header's owner, so
		// an unprivileged extraction yields the extracting user and the real
		// owner only comes back from a --fake-super read of this attribute (the
		// README says so, and this is the assertion that keeps that honest).
		info, err := os.Lstat(filepath.Join(dst, rel))
		if err != nil {
			t.Fatal(err)
		}
		if uid := info.Sys().(*syscall.Stat_t).Uid; int(uid) != os.Geteuid() {
			t.Fatalf("%s extracted as uid %d, want the extracting user's %d (the owner lives in the xattr, not the header)",
				rel, uid, os.Geteuid())
		}
	}

	if got, ok := getXattr(t, filepath.Join(dst, "photos"), "user.rsync.%stat"); !ok || got != dirStat {
		t.Fatalf("directory attribute = %q (present=%v), want %q", got, ok, dirStat)
	}
	if got, ok := getXattr(t, filepath.Join(dst, "photos", "plain.txt"), "user.rsync.%stat"); ok {
		t.Fatalf("an entry with no attribute in the source came out with user.rsync.%%stat = %q", got)
	}
}
