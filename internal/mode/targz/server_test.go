package targz

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
)

func TestServerListAndDelete(t *testing.T) {
	dir := t.TempDir()
	// Two archives + noise that must be ignored. Names are the shape
	// docker/sshd/backitup-recv actually writes.
	writeFile(t, filepath.Join(dir, "backup-20260101T000000Z-000.tar.gz"), "a")
	writeFile(t, filepath.Join(dir, "backup-20260102T000000Z-000.tar.gz"), "bb")
	writeFile(t, filepath.Join(dir, "notes.txt"), "ignore")
	_ = os.MkdirAll(filepath.Join(dir, "subdir"), 0o755)

	var sm mode.ServerMode = Server{}
	snaps, err := sm.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2 (only .tar.gz): %+v", len(snaps), snaps)
	}

	// PrepareOffsite returns the archive itself.
	p, err := sm.PrepareOffsite(context.Background(), dir, snaps[0])
	if err != nil || p != filepath.Join(dir, snaps[0].ID) {
		t.Fatalf("prepare offsite = %q, %v", p, err)
	}

	// DeleteSnapshot removes it.
	if err := sm.DeleteSnapshot(context.Background(), dir, snaps[0].ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	snaps, _ = sm.List(context.Background(), dir)
	if len(snaps) != 1 {
		t.Fatalf("after delete got %d, want 1", len(snaps))
	}
}

func TestServerListMissingDir(t *testing.T) {
	snaps, err := Server{}.List(context.Background(), filepath.Join(t.TempDir(), "nope"))
	if err != nil || snaps != nil {
		t.Fatalf("missing dir should be (nil,nil), got %v %v", snaps, err)
	}
}

func TestServerModeName(t *testing.T) {
	if (Server{}).Mode() != "targz" {
		t.Fatalf("server mode = %q", (Server{}).Mode())
	}
}

// The same non-name ids the HTTP form can carry must be refused here too:
// os.Remove(filepath.Join(clientDir, ".")) is os.Remove(clientDir), and an id is
// a name, not a path fragment.
func TestServerDeleteRefusesNonNameIDs(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../..", "../../etc", "sub/dir", `sub\dir`, "backup-20260101T000000Z-000.tar.gz/.."} {
		dir := t.TempDir()
		keep := filepath.Join(dir, "backup-20260101T000000Z-000.tar.gz")
		writeFile(t, keep, "irreplaceable")

		if err := (Server{}).DeleteSnapshot(context.Background(), dir, id); err == nil {
			t.Errorf("DeleteSnapshot(%q) = nil, want refusal", id)
		}
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("id %q: the archive is gone after a refused delete: %v", id, err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("id %q: the client dir itself was removed: %v", id, err)
		}
	}
}

func writeFile(t *testing.T, p, c string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
	// stable mtime so ordering is deterministic
	_ = os.Chtimes(p, time.Now(), time.Now())
}

// List answers "which snapshot is newest" from the timestamp in each archive's
// own name, and hands them back newest first. Both halves matter downstream: the
// hot pruner protects snaps[0] from deletion, and os.ReadDir returns entries in
// filename order, which for snapshot ids is OLDEST first.
//
// mtimes here deliberately contradict the names in both directions — the oldest
// archive was written last, the newest six days ago — because that is exactly
// what a cp, a volume copy, a restore or an extraction does to a client
// directory. An mtime is not an age.
func TestServerListIsNewestFirstAndAgesByID(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	writeAt(t, filepath.Join(dir, "backup-20260612T000000Z-000.tar.gz"), now.Add(-1*time.Minute))
	writeAt(t, filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz"), now.Add(-6*24*time.Hour))
	// Not the shape backitup-recv writes: no timestamp in the name, so mtime is
	// the only age available and is used.
	writeAt(t, filepath.Join(dir, "legacy-archive.tar.gz"), now.Add(-3*24*time.Hour))

	snaps, err := Server{}.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []string{
		"backup-20260618T000000Z-000.tar.gz", // 18 June, from its name
		"legacy-archive.tar.gz",              // 15 June, from its mtime (fallback)
		"backup-20260612T000000Z-000.tar.gz", // 12 June, from its name
	}
	if len(snaps) != len(want) {
		t.Fatalf("got %d snapshots, want %d: %+v", len(snaps), len(want), snaps)
	}
	for i, id := range want {
		if snaps[i].ID != id {
			t.Fatalf("position %d = %q, want %q (full listing: %+v)", i, snaps[i].ID, id, snaps)
		}
	}
	if got, want := snaps[0].CreatedAt, time.Date(2026, 6, 18, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("newest CreatedAt = %s, want the timestamp in its name (%s), not its mtime", got, want)
	}
	if snaps[0].Bytes == 0 {
		t.Fatal("Bytes must come from the file size: it is what the client page shows")
	}
}

// writeAt writes a file and stamps it with a chosen mtime.
func writeAt(t *testing.T, p string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(p, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}
