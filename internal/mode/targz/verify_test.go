package targz

// The lifecycle's integrity check for a tar.gz client is VerifySnapshot, and it
// is the one place a damaged archive has to surface before a restore does. A
// size comparison would pass here — Bytes came from the file's own size at List
// time — so these tests damage archives in the ways a bad disk or a truncated
// copy damages them, and require the check to notice.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/archiveutil"
	"github.com/th0rn0/backitup/internal/mode"
)

// archiveAt builds a real archive of a small tree and returns its path.
func archiveAt(t *testing.T, dir, name string) string {
	t.Helper()
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "note.txt"), []byte("a backup worth restoring\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	if _, _, err := archiveutil.TarGz(context.Background(), f, src, nil, false, nil); err != nil {
		t.Fatalf("tar.gz: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return p
}

func snapshotOf(t *testing.T, dir, name string) mode.Snapshot {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("stat %s: %v", name, err)
	}
	return mode.Snapshot{ID: name, Bytes: info.Size()}
}

// An intact archive passes.
func TestServerVerifySnapshotAcceptsAnIntactArchive(t *testing.T) {
	dir := t.TempDir()
	const name = "backup-20260101T000000Z-000.tar.gz"
	archiveAt(t, dir, name)

	if err := (Server{}).VerifySnapshot(context.Background(), dir, snapshotOf(t, dir, name)); err != nil {
		t.Fatalf("an intact archive failed verification: %v", err)
	}
}

// A truncated archive — the shape a killed upload or a short copy leaves — must
// fail. It still has a valid gzip header, so a header-only check would pass it.
func TestServerVerifySnapshotRefusesATruncatedArchive(t *testing.T) {
	dir := t.TempDir()
	const name = "backup-20260102T000000Z-000.tar.gz"
	p := archiveAt(t, dir, name)

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() < 64 {
		t.Fatalf("archive is only %d bytes; the truncation below would not be meaningful", info.Size())
	}
	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Keep the header, drop the tail: the gzip trailer (CRC + length) is gone.
	if err := os.WriteFile(p, body[:len(body)-8], 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	// Bytes in the listing still matches what is on disk, which is exactly why
	// a size test cannot be the check.
	snap := snapshotOf(t, dir, name)
	if err := (Server{}).VerifySnapshot(context.Background(), dir, snap); err == nil {
		t.Fatalf("a truncated archive (%d bytes) passed verification", snap.Bytes)
	}
}

// A flipped byte in the compressed body is a CRC failure, not a short read: the
// archive is the right length and still not the tree that went in.
func TestServerVerifySnapshotRefusesCorruptedBytes(t *testing.T) {
	dir := t.TempDir()
	const name = "backup-20260103T000000Z-000.tar.gz"
	p := archiveAt(t, dir, name)

	body, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	mid := len(body) / 2
	body[mid] ^= 0xff
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatalf("corrupt: %v", err)
	}

	if err := (Server{}).VerifySnapshot(context.Background(), dir, snapshotOf(t, dir, name)); err == nil {
		t.Fatal("an archive with a corrupted byte passed verification")
	}
}

// A row that names an archive the volume no longer holds must not be reported as
// intact — the snapshot is gone, and the error has to carry the path.
func TestServerVerifySnapshotReportsAMissingArchive(t *testing.T) {
	dir := t.TempDir()
	snap := mode.Snapshot{ID: "backup-20260104T000000Z-000.tar.gz", Bytes: 1234}

	err := (Server{}).VerifySnapshot(context.Background(), dir, snap)
	if err == nil {
		t.Fatal("a missing archive passed verification")
	}
	if !bytes.Contains([]byte(err.Error()), []byte(snap.ID)) {
		t.Errorf("error = %v, want it to name the archive", err)
	}
}

// tar.gz has no partial snapshot to reclaim: backitup-recv renames from .part
// only on success, so nothing this mode lists can be half-written. Leaning on an
// invented "incomplete" list here would let the lifecycle delete a real archive,
// so the answer is empty even when a .part file is sitting in the directory.
func TestServerListIncompleteIsAlwaysEmpty(t *testing.T) {
	dir := t.TempDir()
	// An interrupted upload: recv leaves the .part file, not a snapshot.
	if err := os.WriteFile(filepath.Join(dir, "backup-20260105T000000Z-000.tar.gz.part"), []byte("partial"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	archiveAt(t, dir, "backup-20260105T000000Z-001.tar.gz")

	var got []mode.Snapshot
	got, err := (Server{}).ListIncomplete(context.Background(), dir)
	if err != nil {
		t.Fatalf("ListIncomplete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListIncomplete returned %d snapshots; a complete archive on disk and a .part in flight are both 'not incomplete'", len(got))
	}
}

// An unreadable client directory is an error, not an empty listing: "no
// snapshots" and "the volume is not mounted" look identical to a pruner, and one
// of those should stop the pass.
func TestServerListFailsOnANonDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := (Server{}).List(context.Background(), p); err == nil {
		t.Fatal("List accepted a regular file as a client directory")
	}
}
