package archiveutil

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
)

func TestTarGz(t *testing.T) {
	src := t.TempDir()
	write(t, filepath.Join(src, "keep.txt"), "hello")
	write(t, filepath.Join(src, "skip.tmp"), "junk")
	mkdir(t, filepath.Join(src, "sub"))
	write(t, filepath.Join(src, "sub", "nested.txt"), "world")
	mkdir(t, filepath.Join(src, "node_modules", "deep"))
	write(t, filepath.Join(src, "node_modules", "deep", "x.txt"), "excluded")

	var buf bytes.Buffer
	files, written, err := TarGz(context.Background(), &buf, src, []string{"*.tmp", "node_modules"})
	if err != nil {
		t.Fatalf("TarGz: %v", err)
	}
	if files != 2 {
		t.Fatalf("files = %d, want 2", files)
	}
	if written != int64(len("hello")+len("world")) {
		t.Fatalf("written = %d, want %d", written, len("hello")+len("world"))
	}

	got := readArchive(t, &buf)
	if got["sub/nested.txt"] != "world" || got["keep.txt"] != "hello" {
		t.Fatalf("archive contents wrong: %v", got)
	}
	if _, ok := got["skip.tmp"]; ok {
		t.Error("skip.tmp should be excluded")
	}
	for name := range got {
		if strings.HasPrefix(name, "node_modules") {
			t.Errorf("node_modules leaked: %s", name)
		}
	}
}

func TestVerifyGzip(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.tar.gz")
	src := t.TempDir()
	write(t, filepath.Join(src, "f.txt"), "data")
	f, _ := os.Create(good)
	if _, _, err := TarGz(context.Background(), f, src, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := VerifyGzip(good); err != nil {
		t.Fatalf("valid archive failed verify: %v", err)
	}

	// Corrupt the gzip → verify must fail.
	bad := filepath.Join(dir, "bad.tar.gz")
	write(t, bad, "this is not gzip at all")
	if err := VerifyGzip(bad); err == nil {
		t.Fatal("corrupt archive passed verify")
	}
	// Truncated gzip (valid header, broken stream) → fail.
	raw, _ := os.ReadFile(good)
	trunc := filepath.Join(dir, "trunc.tar.gz")
	os.WriteFile(trunc, raw[:len(raw)-10], 0o644)
	if err := VerifyGzip(trunc); err == nil {
		t.Fatal("truncated archive passed verify")
	}
}

func TestExcluded(t *testing.T) {
	pats := []string{"*.tmp", "node_modules", "cache/*"}
	cases := map[string]bool{
		"keep.txt": false, "scratch.tmp": true, "node_modules": true,
		"sub/node_modules": true, "cache/x": true, "src/main.go": false, "a/b/d.tmp": true,
	}
	for rel, want := range cases {
		if got := Excluded(rel, pats); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

func TestTrimSlash(t *testing.T) {
	for in, want := range map[string]string{"a/": "a", "b//": "b", "c": "c", "/": "/"} {
		if got := trimSlash(in); got != want {
			t.Errorf("trimSlash(%q)=%q want %q", in, got, want)
		}
	}
}

func write(t *testing.T, p, c string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
		t.Fatal(err)
	}
}
func mkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}
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
			t.Fatalf("tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			out[hdr.Name] = string(b)
		}
	}
	return out
}
