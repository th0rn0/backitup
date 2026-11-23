package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// installFakeRclone puts a stub `rclone` at the front of PATH that records its
// argv, one invocation per line, and does nothing else successfully.
func installFakeRclone(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "rclone-calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + calls + "\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("install fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// TestOffsiteUploadRunsThroughCryptRemote is the proof that offsite bytes are
// addressed to the encrypted remote. The client's destination is a crypt remote
// named "vault" that wraps a plaintext remote named "provider"; rclone must be
// asked to copy to `vault:<path>` — that is what makes it encrypt on the way
// out — and never to the plaintext remote, which would put the backup on the
// provider as-is.
func TestOffsiteUploadRunsThroughCryptRemote(t *testing.T) {
	calls := installFakeRclone(t)
	st := openStore(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "vault",
		RetentionDays: 14, Enabled: true,
	})
	archive := filepath.Join(dir, "snap.tar.gz")
	mkArchive(t, archive, now.Add(-time.Hour))

	confPath := filepath.Join(t.TempDir(), "rclone.conf")
	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(confPath),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read rclone call log: %v", err)
	}
	log := string(b)
	if !strings.Contains(log, "copyto "+archive+" vault:") {
		t.Fatalf("offsite upload did not go through the encrypted remote; rclone was called with:\n%s", log)
	}
	if strings.Contains(log, "provider:") {
		t.Fatalf("offsite upload addressed the plaintext remote the crypt section wraps:\n%s", log)
	}
	if !strings.Contains(log, "--config "+confPath) {
		t.Fatalf("offsite upload did not use backitup's rclone config:\n%s", log)
	}
}
