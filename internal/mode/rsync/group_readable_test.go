package rsync

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
)

// These are the real-transfer proofs for the per-client group_readable switch:
// the source is the shape the card names (a 0700 directory holding a 0600 file),
// the snapshot is inspected on disk, and the second run is a genuine incremental
// against `latest`. They skip when rsync is absent, exactly like the rest of the
// client-path tests in this package; the argv regression test is in
// group_readable_args_test.go, which needs nothing external.

// privateSource makes the tree whose modes would otherwise stop the operator: a
// 0700 directory with a 0600 file inside it. t.TempDir() is already 0700, which
// is the point.
func privateSource(t *testing.T) (src, secret string) {
	t.Helper()
	src = t.TempDir()
	if err := os.Chmod(src, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(src, "private")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	secret = filepath.Join(inner, "secret.txt")
	if err := os.WriteFile(secret, []byte("the operator must be able to read this without root\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return src, secret
}

func perm(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// With the setting off, nothing changes: the snapshot carries the source's modes,
// so a 0700 source produces a 0700 snapshot directory — today's behaviour, and
// the regression guard for every client that does not turn this on.
func TestGroupReadableOffMirrorsTheSourceModes(t *testing.T) {
	requireRsync(t)
	jail := t.TempDir()
	fakeSSH(t, jail)
	src, secret := privateSource(t)

	opts, _ := backupOpts(t, src, false)
	res, err := Mode{}.Backup(context.Background(), opts)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	snap := filepath.Join(jail, "snapshots", res.SnapshotID)
	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		{snap, 0o700},
		{filepath.Join(snap, "private"), 0o700},
		{filepath.Join(snap, "private", "secret.txt"), 0o600},
	} {
		if got := perm(t, tc.path); got != tc.want {
			t.Errorf("%s has mode %04o, want the source's %04o", tc.path, got, tc.want)
		}
	}
	// The source itself was only ever read.
	if got := perm(t, secret); got != 0o600 {
		t.Errorf("the source file's mode changed to %04o; the run must never write to the source", got)
	}
}

// The argv guard: the flag appears only when the setting is on, and a client with
// every setting off sends exactly the invocation 86de926 sent. No rsync needed, so
// CI runs this even where the client-path tests skip.
func TestGroupReadableArgv(t *testing.T) {
	base := mode.BackupOpts{SourceDir: "/src", SSHServer: "h:22", SSHUser: "backitup", SSHKey: "/k"}

	off := snapshotArgs(base, "ssh -i /k", "backitup@h:snapshots/ts/")
	for _, a := range off {
		if a == "--chmod=Dg+rx,Fg+r" {
			t.Fatalf("a client with group_readable off asked for --chmod: %v", off)
		}
	}

	on := base
	on.GroupReadable = true
	args := snapshotArgs(on, "ssh -i /k", "backitup@h:snapshots/ts/")
	found := false
	for _, a := range args {
		if a == "--chmod=Dg+rx,Fg+r" {
			found = true
		}
	}
	if !found {
		t.Fatalf("group_readable on did not add --chmod=Dg+rx,Fg+r: %v", args)
	}
	// It rides with the snapshot transfer, not with the `latest` flip, and it does
	// not change what else is asked for.
	if !contains(args, "-a") || !contains(args, "--numeric-ids") {
		t.Errorf("the base invocation changed: %v", args)
	}
}

// nextSecond waits for the clock to tick: rsync names the snapshot directory from
// the clock it reads at the top of the run, so two runs inside the same second
// collide on the same name and the "second run" would write over the first.
func nextSecond() {
	next := time.Now().Truncate(time.Second).Add(time.Second + 20*time.Millisecond)
	time.Sleep(time.Until(next))
}

// With it on, the same source produces directories the group can traverse and
// files the group can read — and a second, unchanged run hardlinks the file
// rather than re-copying it, with the widened mode still on the shared inode.
func TestGroupReadableWidensModesAndStillHardlinks(t *testing.T) {
	requireRsync(t)
	jail := t.TempDir()
	fakeSSH(t, jail)
	src, _ := privateSource(t)

	first, _ := backupOpts(t, src, false)
	first.GroupReadable = true
	res1, err := Mode{}.Backup(context.Background(), first)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	snap1 := filepath.Join(jail, "snapshots", res1.SnapshotID)
	for _, tc := range []struct {
		path string
		want os.FileMode
	}{
		// Dg+rx on a 0700 directory, Fg+r on a 0600 file: owner bits untouched,
		// group gains exactly what the operator needs.
		{snap1, 0o750},
		{filepath.Join(snap1, "private"), 0o750},
		{filepath.Join(snap1, "private", "secret.txt"), 0o640},
	} {
		if got := perm(t, tc.path); got != tc.want {
			t.Errorf("%s has mode %04o, want %04o", tc.path, got, tc.want)
		}
	}

	// Second run, nothing changed in the source: `latest` now points at snap1, so
	// rsync hardlinks each unchanged file instead of copying it.
	nextSecond()
	second, _ := backupOpts(t, src, true)
	second.GroupReadable = true
	res2, err := Mode{}.Backup(context.Background(), second)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if res2.SnapshotID == res1.SnapshotID {
		t.Fatalf("both runs landed in the same snapshot directory %q", res1.SnapshotID)
	}
	f1, err := os.Stat(filepath.Join(snap1, "private", "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	f2, err := os.Stat(filepath.Join(jail, "snapshots", res2.SnapshotID, "private", "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(f1, f2) {
		t.Error("the second run re-copied the unchanged file instead of hardlinking it (inode differs)")
	}
	if got := f2.Mode().Perm(); got != 0o640 {
		t.Errorf("the hardlinked file has mode %04o, want the widened 0640 (the mode lives on the inode)", got)
	}
	// Turning the setting on after the fact changes earlier snapshots that share
	// the file, because there is only one inode. That is stated on the client
	// page; here it is the measured consequence.
	if got := f1.Mode().Perm(); got != 0o640 {
		t.Errorf("the earlier snapshot's file has mode %04o, want 0640", got)
	}
}

// Offsite reads the snapshot as the server's own uid and ships one archive, so
// widening group bits must not change the archive's shape — and an unchanged tree
// must still deduplicate, i.e. produce the same checksum as the snapshot it was
// hardlinked from.
func TestGroupReadableSnapshotArchivesTheSameAsAnUnchangedTree(t *testing.T) {
	requireRsync(t)
	jail := t.TempDir()
	fakeSSH(t, jail)
	src, _ := privateSource(t)

	first, _ := backupOpts(t, src, false)
	first.GroupReadable = true
	res1, err := Mode{}.Backup(context.Background(), first)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	nextSecond()
	second, _ := backupOpts(t, src, true)
	second.GroupReadable = true
	res2, err := Mode{}.Backup(context.Background(), second)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}

	srv := Server{}
	arch1, err := srv.PrepareOffsite(context.Background(), jail, mode.Snapshot{ID: res1.SnapshotID})
	if err != nil {
		t.Fatalf("archive the first snapshot: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(arch1) })
	arch2, err := srv.PrepareOffsite(context.Background(), jail, mode.Snapshot{ID: res2.SnapshotID})
	if err != nil {
		t.Fatalf("archive the second snapshot: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(arch2) })

	// Same shape: same entries, same modes inside the archive.
	e1, m1 := tarShape(t, arch1)
	e2, m2 := tarShape(t, arch2)
	if len(e1) != len(e2) {
		t.Fatalf("the two archives hold %d and %d entries", len(e1), len(e2))
	}
	for i := range e1 {
		if e1[i] != e2[i] {
			t.Fatalf("archive entries differ: %q vs %q", e1[i], e2[i])
		}
		if m1[i] != m2[i] {
			t.Fatalf("archive mode of %q differs: %04o vs %04o", e1[i], m1[i], m2[i])
		}
	}
	if !contains(e1, "private/secret.txt") {
		t.Fatalf("the archive does not hold the file that was backed up: %v", e1)
	}

	// Deduplication is a checksum of the archive: an unchanged tree must still
	// produce one identical archive, group-readable or not.
	sum1, err := fileSHA256(arch1)
	if err != nil {
		t.Fatal(err)
	}
	sum2, err := fileSHA256(arch2)
	if err != nil {
		t.Fatal(err)
	}
	if sum1 != sum2 {
		t.Errorf("an unchanged tree produced two different archive checksums after enabling group_readable:\n%s\n%s", sum1, sum2)
	}
}

// tarShape returns the archive's entry names and permission bits, in archive
// order.
func tarShape(t *testing.T, path string) ([]string, []os.FileMode) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	tr := tar.NewReader(zr)
	var names []string
	var modes []os.FileMode
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read tar entry: %v", err)
		}
		names = append(names, hdr.Name)
		modes = append(modes, os.FileMode(hdr.Mode).Perm())
	}
	return names, modes
}

func fileSHA256(path string) (string, error) {
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

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
