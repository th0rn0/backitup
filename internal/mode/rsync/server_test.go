package rsync

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

// makeSnap creates clientDir/snapshots/<id>/file.txt.
func makeSnap(t *testing.T, clientDir, id, content string) {
	t.Helper()
	d := filepath.Join(clientDir, "snapshots", id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "file.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestServerListSkipsLatest(t *testing.T) {
	dir := t.TempDir()
	makeSnap(t, dir, "T1", "one")
	makeSnap(t, dir, "T2", "twotwo")
	// latest symlink must be excluded from List.
	_ = os.Symlink("T2", filepath.Join(dir, "snapshots", "latest"))

	snaps, err := Server{}.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("got %d snapshots, want 2 (latest excluded): %+v", len(snaps), snaps)
	}
	for _, s := range snaps {
		if s.ID == "latest" {
			t.Fatal("latest symlink leaked into List")
		}
		if s.Bytes == 0 {
			t.Errorf("snapshot %s has zero bytes", s.ID)
		}
	}
}

func TestServerPrepareOffsiteTarsSnapshot(t *testing.T) {
	dir := t.TempDir()
	makeSnap(t, dir, "T1", "payload-data")
	snaps, _ := Server{}.List(context.Background(), dir)

	tarPath, err := Server{}.PrepareOffsite(context.Background(), dir, snaps[0])
	if err != nil {
		t.Fatalf("prepare offsite: %v", err)
	}
	defer os.Remove(tarPath)

	// It's a real tar.gz containing the snapshot's file.
	found := map[string]string{}
	f, _ := os.Open(tarPath)
	defer f.Close()
	gr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar: %v", err)
		}
		if h.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			found[h.Name] = string(b)
		}
	}
	if found["file.txt"] != "payload-data" {
		t.Fatalf("offsite archive missing snapshot content: %v", found)
	}
}

func TestServerDeleteRefusesLatest(t *testing.T) {
	dir := t.TempDir()
	makeSnap(t, dir, "T1", "x")
	makeSnap(t, dir, "T2", "y")
	_ = os.Symlink("T2", filepath.Join(dir, "snapshots", "latest"))

	// Deleting the latest target must be refused (protects the --link-dest base).
	if err := (Server{}).DeleteSnapshot(context.Background(), dir, "T2"); err == nil {
		t.Fatal("expected refusal deleting the current latest")
	}
	// A non-latest snapshot deletes fine.
	if err := (Server{}).DeleteSnapshot(context.Background(), dir, "T1"); err != nil {
		t.Fatalf("delete T1: %v", err)
	}
	snaps, _ := Server{}.List(context.Background(), dir)
	if len(snaps) != 1 || snaps[0].ID != "T2" {
		t.Fatalf("after delete: %+v", snaps)
	}
}

var _ mode.ServerMode = Server{}
