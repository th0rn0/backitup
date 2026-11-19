package lifecycle

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// syncBuffer collects what the lifecycle pass logged. The standard logger is a
// single global, so the buffer is guarded: a pass is sequential, but an alert
// goroutine or a future parallel test must not turn that into a data race.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// runOnceCapturingLog runs one real lifecycle pass and returns everything it
// logged — the operator's view of the pass, taken at the entry point rather than
// from a hand-copied fragment of it.
func runOnceCapturingLog(t *testing.T, d Deps) string {
	t.Helper()
	logs := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	if err := RunOnce(context.Background(), d); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	return logs.String()
}

// A healthy rsync client must produce no integrity finding at all. Pre-fix this
// logged "INTEGRITY WARN: latest snapshot <id> is empty" on every pass, for every
// rsync client, because rsync reports Bytes: 0 by design (summing a hardlinked
// tree is not free) and the check read 0 as "empty".
func TestRunOnceHealthyRsyncSnapshotLogsNoIntegrityFinding(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T080000Z", "20260618T100000Z", "20260618T110000Z")
	if err := os.Symlink("20260618T110000Z", filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatalf("symlink latest: %v", err)
	}

	logs := runOnceCapturingLog(t, Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }})

	if strings.Contains(logs, "INTEGRITY") {
		t.Fatalf("a healthy rsync pass must not report an integrity finding, logged:\n%s", logs)
	}
}

// The other half: a snapshot that really is unusable must still reach the
// operator, naming the snapshot and what is wrong with it.
func TestRunOnceReportsUnusableRsyncSnapshot(t *testing.T) {
	const snapID = "20260618T110000Z"
	const olderID = "20260618T100000Z"

	cases := []struct {
		name   string
		damage func(t *testing.T, clientDir string)
		want   string // the actionable detail in the line
	}{
		{
			name: "empty snapshot directory",
			damage: func(t *testing.T, clientDir string) {
				// A run that produced a directory with nothing in it: nothing
				// was copied, and that is not a backup.
				if err := os.MkdirAll(filepath.Join(clientDir, "snapshots", snapID), 0o755); err != nil {
					t.Fatal(err)
				}
				// latest legitimately points at the older, intact snapshot.
				if err := os.Remove(filepath.Join(clientDir, "snapshots", "latest")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(olderID, filepath.Join(clientDir, "snapshots", "latest")); err != nil {
					t.Fatal(err)
				}
			},
			want: "is empty",
		},
		{
			name: "dangling latest pointer",
			damage: func(t *testing.T, clientDir string) {
				// The newest snapshot is fine; `latest` points at one that is
				// not there, so the next run's --link-dest base is gone and
				// hardlinking silently stops.
				mkRsyncSnapshots(t, clientDir, snapID)
				latest := filepath.Join(clientDir, "snapshots", "latest")
				if err := os.Remove(latest); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("20260618T090000Z", latest); err != nil {
					t.Fatal(err)
				}
			},
			want: "hardlink base",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
			_, base, dir := newClient(t, st, model.Client{
				Name: "c", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
			})
			// The older snapshot is intact and `latest` legitimately points at
			// it; each case below builds the newest snapshot in the state it is
			// about — the one the pass verifies.
			mkRsyncSnapshots(t, dir, olderID)
			if err := os.Symlink(olderID, filepath.Join(dir, "snapshots", "latest")); err != nil {
				t.Fatalf("symlink latest: %v", err)
			}
			tc.damage(t, dir)

			logs := runOnceCapturingLog(t, Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }})

			if !strings.Contains(logs, "INTEGRITY FAIL") {
				t.Fatalf("a pass over an unusable snapshot must report an integrity finding, logged:\n%s", logs)
			}
			if !strings.Contains(logs, snapID) {
				t.Errorf("the finding must name the snapshot %s so the operator can act on it, logged:\n%s", snapID, logs)
			}
			if !strings.Contains(logs, tc.want) {
				t.Errorf("the finding must say what is wrong (%q), logged:\n%s", tc.want, logs)
			}
		})
	}
}

// A tar.gz archive that is present but corrupt is still caught by the same pass;
// the check moved into the mode, it did not lose its teeth.
func TestRunOnceStillReportsCorruptTarGzSnapshot(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, RetentionDays: 14, Enabled: true,
	})
	// Not a gzip stream: gzip.NewReader fails on the header.
	if err := os.WriteFile(filepath.Join(dir, "backup-20260618T110000Z.tar.gz"), []byte("not a gzip stream"), 0o644); err != nil {
		t.Fatal(err)
	}

	logs := runOnceCapturingLog(t, Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }})

	if !strings.Contains(logs, "INTEGRITY FAIL") || !strings.Contains(logs, "backup-20260618T110000Z.tar.gz") {
		t.Fatalf("a corrupt archive must still be reported, logged:\n%s", logs)
	}
}
