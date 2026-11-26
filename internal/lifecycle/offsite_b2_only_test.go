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

// installRecordingRclone puts a stub `rclone` at the front of PATH that records
// its argv (one invocation per line) and succeeds, so a test can assert on what
// the lifecycle asked rclone to do — and, just as importantly, that it asked
// nothing at all.
func installRecordingRclone(t *testing.T) string {
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

func rcloneCalls(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read rclone call log: %v", err)
	}
	return string(b)
}

// A B2 remote holds the bucket (and an optional prefix), so every object path
// is addressed inside it: rclone is asked for <remote>:<bucket>/<prefix>/<dir>/
// <file>, never for a bare <remote>:<file>. This is the half of "the remote
// knows where it writes" that a unit test on RootPath cannot show.
func TestOffsiteUploadAddressesTheRemoteBucketAndPrefix(t *testing.T) {
	calls := installRecordingRclone(t)
	st := openStore(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "offsite", Backend: model.BackendB2,
		Config:    map[string]string{"account": "keyid", "key": "appkey", "bucket": "my-backups", "path": "backups"},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "offsite",
		RetentionDays: 14, Enabled: true,
	})
	archive := filepath.Join(dir, "backup-20261118T100000Z-000.tar.gz")
	mkArchive(t, archive, now.Add(-time.Hour))

	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(filepath.Join(t.TempDir(), "rclone.conf")),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	log := rcloneCalls(t, calls)
	want := "copyto " + archive + " offsite:my-backups/backups/c/" + defaultPrefix("c") + "backup-20261118T100000Z-000.tar.gz"
	if !strings.Contains(log, want) {
		t.Fatalf("offsite upload did not address the bucket and prefix;\nwant an invocation containing: %s\ngot:\n%s", want, log)
	}
}

// A remote an older backitup configured with a backend this build dropped is
// kept but never used: the offsite worker must not upload through it, and the
// lifecycle must not verify against it. The hot copies stay too — "offsite is
// configured" is what protects an un-tiered snapshot from the pruner, and an
// unsupported remote has tiered nothing.
func TestUnsupportedRemoteSkipsOffsiteAndKeepsHotCopies(t *testing.T) {
	calls := installRecordingRclone(t)
	st := openStore(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "legacy-s3", Backend: "s3", Unsupported: true,
		Config:    map[string]string{"bucket": "s3-bucket"},
		CreatedAt: now.Add(-400 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("create unsupported remote: %v", err)
	}
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "legacy-s3",
		RetentionDays: 14, Enabled: true,
	})
	// Three snapshots: two well past the hot horizon and never offsited, one
	// fresh. Without the offsite-first guard the two old ones are deleted.
	mkArchive(t, filepath.Join(dir, "backup-20251001T100000Z-000.tar.gz"), now.Add(-400*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "backup-20261001T100000Z-000.tar.gz"), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "backup-20261118T100000Z-000.tar.gz"), now.Add(-time.Hour))

	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(filepath.Join(t.TempDir(), "rclone.conf")),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	if log := rcloneCalls(t, calls); log != "" {
		t.Fatalf("the offsite worker invoked rclone for a client whose remote is unsupported:\n%s", log)
	}

	ran := runOnceCapturingLog(t, deps)
	if log := rcloneCalls(t, calls); log != "" {
		t.Fatalf("the lifecycle pass invoked rclone for a client whose remote is unsupported:\n%s", log)
	}
	if !strings.Contains(ran, "unsupported remote type") {
		t.Fatalf("nothing said why offsite is not happening for this client:\n%s", ran)
	}

	for _, name := range []string{
		"backup-20251001T100000Z-000.tar.gz",
		"backup-20261001T100000Z-000.tar.gz",
		"backup-20261118T100000Z-000.tar.gz",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("hot snapshot %s was pruned although nothing is offsited: %v", name, err)
		}
	}
}

// A client whose remote has no row at all — a section the operator wrote in
// rclone.conf by hand — keeps the path it has always had: the bucket/prefix
// only exist when a remote row was configured through the UI. The backup's NAME
// is not affected either way: it still carries the client's prefix.
func TestHandWrittenRemoteKeepsTheUnprefixedPath(t *testing.T) {
	calls := installRecordingRclone(t)
	st := openStore(t)
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "byhand",
		RetentionDays: 14, Enabled: true,
	})
	archive := filepath.Join(dir, "backup-20261118T100000Z-000.tar.gz")
	mkArchive(t, archive, now.Add(-time.Hour))

	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(filepath.Join(t.TempDir(), "rclone.conf")),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	want := "copyto " + archive + " byhand:c/" + defaultPrefix("c") + "backup-20261118T100000Z-000.tar.gz"
	if log := rcloneCalls(t, calls); !strings.Contains(log, want) {
		t.Fatalf("a hand-written remote's path changed;\nwant an invocation containing: %s\ngot:\n%s", want, log)
	}
}
