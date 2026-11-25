package rsync

import (
	"bytes"
	"context"
	"encoding/binary"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

// The fidelity settings are what "all files" means beyond `-a`. These tests
// exercise the flags in two layers: the argv the client builds (no rsync
// needed, so CI always runs it) and a real transfer through the same ssh
// stand-in the rest of the package uses (so the claim about a hole staying a
// hole is measured on a snapshot, not asserted about a flag).

func TestFidelityArgs(t *testing.T) {
	cases := []struct {
		name string
		opts mode.BackupOpts
		want string
	}{
		// The regression guard for the whole card: a client with every new
		// setting off must send exactly the invocation 86de926 sent.
		{"every setting off", mode.BackupOpts{}, ""},
		{"sparse", mode.BackupOpts{PreserveSparse: true}, "-S"},
		{"hardlinks", mode.BackupOpts{PreserveHardlinks: true}, "-H"},
		{"acls", mode.BackupOpts{PreserveACLs: true}, "-A"},
		{"xattrs", mode.BackupOpts{PreserveXattrs: true}, "-X"},
		{"all four", mode.BackupOpts{PreserveSparse: true, PreserveHardlinks: true, PreserveACLs: true, PreserveXattrs: true}, "-S -H -A -X"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(fidelityArgs(tc.opts), " "); got != tc.want {
				t.Errorf("fidelityArgs = %q, want %q", got, tc.want)
			}
			if got := fidelityFlags(tc.opts); got != tc.want {
				t.Errorf("fidelityFlags = %q, want %q", got, tc.want)
			}
		})
	}
}

// rrsync's refusal, verbatim from rrsync 3.5.0 in the shipped recv image
// (docker/sshd forced command). The client sees this on stderr followed by
// rsync's own "connection unexpectedly closed"/"protocol data stream (code 12)"
// and nothing else — the reported defect is that the operator gets no flag.
func TestRefusedFidelityOptionNamesTheSetting(t *testing.T) {
	const refused = "/usr/local/bin/rrsync error: option -A has been disabled on this server.\n" +
		"rsync: connection unexpectedly closed (0 bytes received so far) [sender]\n" +
		"rsync error: error in rsync protocol data stream (code 12) at io.c(232) [sender=3.4.3]"
	got := refusedFidelityOption(refused)
	for _, want := range []string{"-A", "refused", "preserve_acls", "has been disabled on this server"} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal hint %q does not mention %q", got, want)
		}
	}

	if got := refusedFidelityOption("rsync: [sender] write error: Broken pipe (32)"); got != "" {
		t.Errorf("an unrelated failure produced a refusal hint: %q", got)
	}
	if got := refusedFidelityOption(""); got != "" {
		t.Errorf("empty output produced a refusal hint: %q", got)
	}
}

// A tree of 1.9 MB on disk with a hole in it: without -S the hole is written out
// as zeros, and the snapshot costs its apparent size. With -S it costs about
// what the source costs. Both numbers are measured, not assumed.
func TestSnapshotDiskUsageFollowsTheSparseSetting(t *testing.T) {
	const hole = 64 << 20 // 64 MiB: big enough to be unambiguous, small enough for CI

	t.Run("setting off writes the hole out", func(t *testing.T) {
		snap := runOneBackup(t, mode.BackupOpts{}, hole)
		apparent, onDisk := treeSizes(t, snap)
		t.Logf("du -sb %d bytes, du -s -B1 %d bytes (hole %d bytes)", apparent, onDisk, hole)
		if onDisk < apparent*9/10 {
			t.Fatalf("with every fidelity setting off the snapshot uses %d bytes on disk for %d bytes of data — expected the hole to be materialised",
				onDisk, apparent)
		}
	})

	t.Run("sparse setting keeps the hole", func(t *testing.T) {
		snap := runOneBackup(t, mode.BackupOpts{PreserveSparse: true}, hole)
		apparent, onDisk := treeSizes(t, snap)
		t.Logf("du -sb %d bytes, du -s -B1 %d bytes (hole %d bytes)", apparent, onDisk, hole)
		if onDisk >= apparent/2 {
			t.Fatalf("with -S the snapshot uses %d bytes on disk for %d bytes of data — the hole was materialised", onDisk, apparent)
		}
		if onDisk > 8<<20 {
			t.Fatalf("with -S the snapshot still costs %d bytes for a %d-byte hole plus 1 MiB of data", onDisk, hole)
		}
	})
}

// Two names for one file are one file. Off, rsync stores the data twice.
func TestHardlinkSettingSharesOneInode(t *testing.T) {
	t.Run("setting on", func(t *testing.T) {
		snap := runOneBackup(t, mode.BackupOpts{PreserveHardlinks: true}, 8<<20)
		a, b := mustStat(t, filepath.Join(snap, "data.bin")), mustStat(t, filepath.Join(snap, "data-hardlink.bin"))
		if !os.SameFile(a, b) {
			t.Fatalf("the hardlink pair landed as two inodes (%v and %v): -H did not reach rsync", a.Sys(), b.Sys())
		}
	})
	t.Run("setting off", func(t *testing.T) {
		snap := runOneBackup(t, mode.BackupOpts{}, 8<<20)
		a, b := mustStat(t, filepath.Join(snap, "data.bin")), mustStat(t, filepath.Join(snap, "data-hardlink.bin"))
		if os.SameFile(a, b) {
			t.Fatal("the hardlink pair shares an inode with the setting off, which is not what 86de926 did")
		}
	})
}

// ACLs and xattrs are metadata the receiver applies as the file's owner, which
// it is. With the settings on the snapshot carries them; with them off the run
// still succeeds and carries neither (the old behaviour, and never a failure).
func TestACLAndXattrSettings(t *testing.T) {
	requireRsync(t)
	src := sparseSource(t, 8<<20)
	acl := posixACLBlob()
	aclPath := filepath.Join(src, "acl.txt")
	if err := os.WriteFile(aclPath, []byte("bearer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(aclPath, "system.posix_acl_access", acl, 0); err != nil {
		t.Skipf("this filesystem does not accept a POSIX ACL (%v): the ACL half cannot be measured here", err)
	}
	if err := syscall.Setxattr(aclPath, "user.backitup", []byte("hello"), 0); err != nil {
		t.Skipf("this filesystem does not accept a user xattr (%v)", err)
	}

	t.Run("settings on", func(t *testing.T) {
		server := t.TempDir()
		fakeSSH(t, server)
		opts, _ := backupOpts(t, src, false)
		opts.PreserveACLs = true
		opts.PreserveXattrs = true
		snap := runBackup(t, server, opts)

		gotACL, err := getxattr(filepath.Join(snap, "acl.txt"), "system.posix_acl_access")
		if err != nil || len(gotACL) == 0 {
			t.Fatalf("the snapshot lost the ACL: %v", err)
		}
		if string(gotACL) != string(acl) {
			t.Errorf("the snapshot's ACL is not the source's: got % x want % x", gotACL, acl)
		}
		gotX, err := getxattr(filepath.Join(snap, "acl.txt"), "user.backitup")
		if err != nil || string(gotX) != "hello" {
			t.Fatalf("the snapshot lost the xattr: %q, %v", gotX, err)
		}
	})

	t.Run("settings off", func(t *testing.T) {
		server := t.TempDir()
		fakeSSH(t, server)
		opts, _ := backupOpts(t, src, false) // every fidelity setting off
		snap := runBackup(t, server, opts)

		if got, err := getxattr(filepath.Join(snap, "acl.txt"), "system.posix_acl_access"); err == nil {
			t.Errorf("the snapshot carries an ACL although -A was not set: % x", got)
		}
		if got, err := getxattr(filepath.Join(snap, "acl.txt"), "user.backitup"); err == nil {
			t.Errorf("the snapshot carries an xattr although -X was not set: %q", got)
		}
	})
}

// A receiver that refuses one of the flags must produce an error naming it, not
// rsync's "protocol data stream" shrug. The stand-in ssh prints rrsync's own
// refusal text and exits, exactly as the real receiver did when measured
// against rrsync 3.5.0.
func TestRefusedFidelityFlagFailsLegibly(t *testing.T) {
	requireRsync(t)
	server := t.TempDir()
	fakeSSHRefusing(t, server, "-A")
	src := sparseSource(t, 1<<20)

	opts, buf := backupOpts(t, src, false)
	opts.PreserveACLs = true
	_, err := Mode{}.Backup(context.Background(), opts)
	if err == nil {
		t.Fatal("a run whose receiver refused -A reported success")
	}
	for _, want := range []string{"-A", "preserve_acls", "refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the run's error does not name %q: %v", want, err)
		}
		if !strings.Contains(buf.String(), want) {
			t.Errorf("the run's log does not name %q:\n%s", want, buf.String())
		}
	}
}

// ---- helpers -------------------------------------------------------------

// runOneBackup builds a one-directory source with a sparse file and a hardlink
// pair, runs a backup against its own jail, and returns the snapshot directory.
func runOneBackup(t *testing.T, fidelity mode.BackupOpts, holeBytes int64) string {
	t.Helper()
	requireRsync(t)
	server := t.TempDir()
	fakeSSH(t, server)
	src := sparseSource(t, holeBytes)
	opts, _ := backupOpts(t, src, false)
	opts.PreserveSparse = fidelity.PreserveSparse
	opts.PreserveHardlinks = fidelity.PreserveHardlinks
	opts.PreserveACLs = fidelity.PreserveACLs
	opts.PreserveXattrs = fidelity.PreserveXattrs
	return runBackup(t, server, opts)
}

// runBackup runs Mode.Backup with opts and returns the one snapshot it wrote.
func runBackup(t *testing.T, server string, opts mode.BackupOpts) string {
	t.Helper()
	buf := &bytes.Buffer{}
	opts.Logger = log.New(buf, "", 0)
	res, err := Mode{}.Backup(context.Background(), opts)
	if err != nil {
		t.Fatalf("backup failed: %v\nlog:\n%s", err, buf.String())
	}
	if res.SnapshotID == "" {
		t.Fatal("the run reported no snapshot id")
	}
	return filepath.Join(server, "snapshots", res.SnapshotID)
}

// sparseSource writes data.bin (holeBytes long with a little real data at the
// front), a hardlink to it, and one small file.
func sparseSource(t *testing.T, holeBytes int64) string {
	t.Helper()
	src := t.TempDir()
	path := filepath.Join(src, "data.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("head"), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(holeBytes); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(src, "data-hardlink.bin")); err != nil {
		t.Fatalf("create the hardlink pair: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("a small ordinary file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return src
}

// treeSizes returns the apparent size of a tree and the bytes it really costs,
// which is the pair the report is about (du -sb / du -s -B1).
func treeSizes(t *testing.T, root string) (apparent, onDisk int64) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		apparent += info.Size()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			onDisk += int64(st.Blocks) * 512
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return apparent, onDisk
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info
}

func getxattr(path, name string) ([]byte, error) {
	buf := make([]byte, 4096)
	n, err := syscall.Getxattr(path, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// posixACLBlob is a valid system.posix_acl_access value: rw- for the owner, rw-
// for uid 1234, r-- for the group with a rw- mask, r-- for other. Setting it
// needs no setfacl binary, so the test measures the transfer rather than the
// availability of acl tooling.
func posixACLBlob() []byte {
	entries := []struct {
		tag, perm uint16
		id        uint32
	}{
		{0x01, 6, 0xFFFFFFFF}, // ACL_USER_OBJ
		{0x02, 6, 1234},       // ACL_USER
		{0x04, 4, 0xFFFFFFFF}, // ACL_GROUP_OBJ
		{0x10, 6, 0xFFFFFFFF}, // ACL_MASK
		{0x20, 4, 0xFFFFFFFF}, // ACL_OTHER
	}
	buf := make([]byte, 4, 4+len(entries)*8)
	binary.LittleEndian.PutUint32(buf, 2) // ACL version 2
	for _, e := range entries {
		var raw [8]byte
		binary.LittleEndian.PutUint16(raw[0:2], e.tag)
		binary.LittleEndian.PutUint16(raw[2:4], e.perm)
		binary.LittleEndian.PutUint32(raw[4:8], e.id)
		buf = append(buf, raw[:]...)
	}
	return buf
}

// fakeSSHRefusing installs the same ssh stand-in as fakeSSH, except that a
// remote command carrying flag is refused the way rrsync refuses it: the
// script's message on stderr and a non-zero exit, before any transfer.
func fakeSSHRefusing(t *testing.T, jail, flag string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is needed for the ssh stand-in")
	}
	if err := os.MkdirAll(filepath.Join(jail, "snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	script := `#!/bin/bash
JAIL='` + jail + `'
FLAG='` + flag + `'
while [ $# -gt 0 ]; do
	case "$1" in
		-i|-p|-o|-l) shift 2 ;;
		-*) shift ;;
		*) break ;;
	esac
done
[ $# -gt 0 ] || { echo "fake ssh: no host" >&2; exit 96; }
shift
for a in "$@"; do
	case "$a" in
		-*)
			case "$a" in
				*"${FLAG#-}"*)
					echo "$0 error: option $FLAG has been disabled on this server." >&2
					exit 1
					;;
			esac
			;;
	esac
done
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
