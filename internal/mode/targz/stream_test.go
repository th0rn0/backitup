package targz

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

// streamArchive writes to any io.Writer, so we can verify the tar/gzip/walk
// logic (the bulk of Backup) without an SSH server.
func TestStreamArchive(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "keep.txt"), "hello")
	mustWrite(t, filepath.Join(src, "skip.tmp"), "junk")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "sub", "nested.txt"), "world")
	if err := os.MkdirAll(filepath.Join(src, "node_modules", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "node_modules", "deep", "x.txt"), "excluded")

	var buf bytes.Buffer
	files, written, err := streamArchive(context.Background(), &buf, mode.BackupOpts{
		SourceDir: src,
		Excludes:  []string{"*.tmp", "node_modules"},
	})
	if err != nil {
		t.Fatalf("streamArchive: %v", err)
	}
	if files != 2 { // keep.txt + sub/nested.txt
		t.Fatalf("files = %d, want 2", files)
	}
	if written != int64(len("hello")+len("world")) {
		t.Fatalf("written = %d, want %d", written, len("hello")+len("world"))
	}

	got := readArchive(t, &buf)
	if _, ok := got["keep.txt"]; !ok {
		t.Error("keep.txt missing from archive")
	}
	if got["sub/nested.txt"] != "world" {
		t.Errorf("sub/nested.txt = %q", got["sub/nested.txt"])
	}
	if _, ok := got["skip.tmp"]; ok {
		t.Error("skip.tmp should be excluded")
	}
	for name := range got {
		if strings.HasPrefix(name, "node_modules") {
			t.Errorf("node_modules content leaked: %s", name)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readArchive returns regular-file name -> content from a tar.gz stream.
func readArchive(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	gr, err := gzip.NewReader(r)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	tr := tar.NewReader(gr)
	out := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			out[hdr.Name] = string(b)
		}
	}
	return out
}
