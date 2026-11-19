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
