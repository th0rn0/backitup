package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteCreatesAndReplaces: the file is created with the requested mode,
// replaced on the next write, and no temp files are left behind.
func TestWriteCreatesAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rclone.conf")

	before, err := statOrNil(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}
	if before != nil {
		t.Fatalf("file already exists before the test")
	}

	if err := Write(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after first write: %v", err)
	}
	if got := first.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}

	if err := Write(path, []byte("second\n"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second\n" {
		t.Fatalf("content = %q, want %q", got, "second\n")
	}

	// The replacement must be a rename, not an in-place truncate+write, which is
	// what makes concurrent readers and post-crash restarts safe.
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second write: %v", err)
	}
	if os.SameFile(first, second) {
		t.Error("file was modified in place; want a temp file renamed over it")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "rclone.conf" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only rclone.conf (temp file leaked)", names)
	}
}

const oldConfig = "# generated\n[s3]\ntype = s3\nprovider = AWS\n"

// TestWriteMidWriteFaultKeepsOldFile injects a failure after the new bytes are
// written to the temp file but before the rename — the point where a crash or
// ENOSPC hits — and requires the previous file to still be there, complete.
func TestWriteMidWriteFaultKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rclone.conf")
	if err := Write(path, []byte(oldConfig), 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	hookAfterWrite = func() error { return errors.New("simulated ENOSPC mid-write") }
	t.Cleanup(func() { hookAfterWrite = nil })

	bigNew := strings.Repeat("[new]\ntype = s3\n", 1<<16)
	err := Write(path, []byte(bigNew), 0o600)
	if err == nil {
		t.Fatal("Write succeeded despite the injected fault")
	}
	if !strings.Contains(err.Error(), "simulated ENOSPC") {
		t.Fatalf("error = %v, want it to carry the injected fault", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("old file unreadable after failed write: %v", err)
	}
	if string(got) != oldConfig {
		t.Fatalf("old file was modified by the failed write: got %d bytes %q, want %d bytes of the old config",
			len(got), firstLine(string(got)), len(oldConfig))
	}
	if !strings.Contains(string(got), "[s3]") || !strings.Contains(string(got), "type = s3") {
		t.Fatalf("old file is not a complete rclone config: %q", got)
	}
	assertOnlyFile(t, dir, "rclone.conf")
}

// TestWriteToReadOnlyDirKeepsOldFile simulates the other mid-write failure: the
// destination directory will not accept new files (full/read-only filesystem).
func TestWriteToReadOnlyDirKeepsOldFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not block writes; the injected-fault test covers this path")
	}
	dir := filepath.Join(t.TempDir(), "conf")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(dir, "rclone.conf")
	if err := Write(path, []byte(oldConfig), 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) }) // let TempDir cleanup succeed

	if err := Write(path, []byte("[new]\ntype = s3\n"), 0o600); err == nil {
		t.Fatal("Write into a read-only directory succeeded")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("old file unreadable after failed write: %v", err)
	}
	if string(got) != oldConfig {
		t.Fatalf("old file was modified by the failed write: %q", got)
	}
	assertOnlyFile(t, dir, "rclone.conf")
}

func statOrNil(path string) (os.FileInfo, error) {
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	return fi, err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func assertOnlyFile(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != want {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only %s (temp file leaked)", names, want)
	}
}
