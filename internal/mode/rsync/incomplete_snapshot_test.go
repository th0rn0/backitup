package rsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

// markSnap turns an existing snapshot directory into what a failed run leaves:
// whatever rsync copied plus the marker that says so.
func markSnap(t *testing.T, clientDir, id, runErr string) string {
	t.Helper()
	dir := filepath.Join(clientDir, "snapshots", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, IncompleteMarker), []byte(incompleteReasonText("", errStr(runErr))), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func errStr(s string) error { return &staticErr{s} }

type staticErr struct{ s string }

func (e *staticErr) Error() string { return e.s }

// A directory a failed run left behind must not be handed to anything as a
// snapshot: the offsite worker tars whatever List returns, so listing it uploads
// four empty files to B2 as this client's backup.
func TestListHidesIncompleteSnapshot(t *testing.T) {
	dir := t.TempDir()
	makeSnap(t, dir, "20260101T000000Z", "the good one")
	if err := os.Symlink("20260101T000000Z", filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	markSnap(t, dir, "20260102T000000Z", "rsync: exit status 23: send_files failed to open")

	snaps, err := Server{}.List(context.Background(), dir)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(snaps) != 1 || snaps[0].ID != "20260101T000000Z" {
		t.Fatalf("List returned %+v, want only the previous good snapshot", snaps)
	}

	// The marked directory is still on disk — it is hidden from the listing, not
	// silently deleted by it — and ListIncomplete is the one caller that sees it.
	if _, err := os.Stat(filepath.Join(dir, "snapshots", "20260102T000000Z")); err != nil {
		t.Fatalf("the incomplete directory should still be on disk for the lifecycle to reclaim: %v", err)
	}
	incomplete, err := Server{}.ListIncomplete(context.Background(), dir)
	if err != nil {
		t.Fatalf("list incomplete: %v", err)
	}
	if len(incomplete) != 1 || incomplete[0].ID != "20260102T000000Z" {
		t.Fatalf("ListIncomplete = %+v, want the one marked directory", incomplete)
	}
}

// VerifySnapshot used to accept the shape a failed run leaves ("directory
// exists, has entries"), which four zero-byte files satisfy. It must now name the
// failure, and name it in the operator's terms: the run's own error.
func TestVerifySnapshotRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	makeSnap(t, dir, "20260101T000000Z", "the good one")
	if err := os.Symlink("20260101T000000Z", filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	snapDir := markSnap(t, dir, "20260102T000000Z", "rsync: exit status 23: Permission denied (13)")
	for _, name := range []string{"a.qcow2", "b.qcow2"} {
		if err := os.WriteFile(filepath.Join(snapDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	err := Server{}.VerifySnapshot(context.Background(), dir, mode.Snapshot{ID: "20260102T000000Z"})
	if err == nil {
		t.Fatal("an incomplete snapshot must not verify clean")
	}
	for _, want := range []string{"incomplete", "exit status 23", "Permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("verify error %q should mention %q", err, want)
		}
	}
}

// The marker does not hide anything else: healthy snapshots are listed and
// verify clean, and `latest` is not confused for an incomplete one.
func TestIncompleteMarkerDoesNotAffectHealthySnapshots(t *testing.T) {
	dir := t.TempDir()
	makeSnap(t, dir, "20260101T000000Z", "one")
	makeSnap(t, dir, "20260102T000000Z", "two")
	if err := os.Symlink("20260102T000000Z", filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}

	snaps, err := Server{}.List(context.Background(), dir)
	if err != nil || len(snaps) != 2 {
		t.Fatalf("List = %+v, %v; want both healthy snapshots", snaps, err)
	}
	for _, s := range snaps {
		if err := (Server{}).VerifySnapshot(context.Background(), dir, s); err != nil {
			t.Errorf("healthy snapshot %s did not verify: %v", s.ID, err)
		}
	}
	incomplete, err := Server{}.ListIncomplete(context.Background(), dir)
	if err != nil || len(incomplete) != 0 {
		t.Fatalf("ListIncomplete = %+v, %v; want nothing for a healthy client", incomplete, err)
	}
}

// A directory that is missing, empty or unreadable is still reported by the
// checks that were there before; ListIncomplete must not turn those into
// "incomplete" (they are not marked, and their age is unknown).
func TestListIncompleteIgnoresUnmarkedDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "snapshots", "20260101T000000Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A stray file in snapshots/ is not a snapshot either.
	if err := os.WriteFile(filepath.Join(dir, "snapshots", "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	incomplete, err := Server{}.ListIncomplete(context.Background(), dir)
	if err != nil {
		t.Fatalf("list incomplete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Fatalf("ListIncomplete = %+v, want nothing (no marker)", incomplete)
	}
	// And no snapshots directory at all is not an error.
	if got, err := (Server{}).ListIncomplete(context.Background(), t.TempDir()); err != nil || len(got) != 0 {
		t.Fatalf("ListIncomplete on a client with no snapshots = %+v, %v", got, err)
	}
}

// The shape test that would have caught the reported failure: listeners whose
// every file is 0 bytes. This is the real log — four .qcow2 files the sender
// could not read, "Literal data: 0 bytes".
func TestVerifySnapshotRejectsAllZeroByteFiles(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, "snapshots", "20260102T000000Z")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"drone-runner.qcow2", "gitlab-runner.qcow2", "a.qcow2", "b.qcow2"} {
		if err := os.WriteFile(filepath.Join(d, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("20260102T000000Z", filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}

	err := Server{}.VerifySnapshot(context.Background(), dir, mode.Snapshot{ID: "20260102T000000Z"})
	if err == nil || !strings.Contains(err.Error(), "0 bytes") {
		t.Fatalf("a snapshot of 4 zero-byte files must be reported, got: %v", err)
	}
}

// ...but the negative cases, so this cannot become "reject anything odd":
//
//   - one file with content is a real snapshot, whatever else is in there;
//   - a snapshot with any subdirectory is not judged (the content may be inside
//     it, and finding out is the WalkDir this mode must never do);
//   - more files than the sample bound is not judged either — a big real
//     snapshot must never be held responsible for one stat per file;
//   - a genuinely empty source directory stays a legitimate snapshot, which is
//     the "is empty" case rather than this one.
func TestVerifySnapshotDoesNotJudgeTheNegativeCases(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, snapDir string)
	}{
		{"one file with content", func(t *testing.T, d string) {
			if err := os.WriteFile(filepath.Join(d, "a.qcow2"), []byte("real bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "b.qcow2"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a subdirectory may hold the content", func(t *testing.T, d string) {
			if err := os.MkdirAll(filepath.Join(d, "vms"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "vms", "a.qcow2"), []byte("inside"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(d, "empty.qcow2"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"more files than the sample bound", func(t *testing.T, d string) {
			for i := 0; i <= zeroByteSample+1; i++ {
				name := filepath.Join(d, fmt.Sprintf("f%03d.txt", i))
				if err := os.WriteFile(name, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			snapDir := filepath.Join(dir, "snapshots", "20260102T000000Z")
			if err := os.MkdirAll(snapDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.build(t, snapDir)
			if err := os.Symlink("20260102T000000Z", filepath.Join(dir, "snapshots", "latest")); err != nil {
				t.Fatal(err)
			}
			if err := (Server{}).VerifySnapshot(context.Background(), dir, mode.Snapshot{ID: "20260102T000000Z"}); err != nil {
				t.Fatalf("must not be judged a failure: %v", err)
			}
		})
	}
}

// A symlink (or any non-regular entry) is not content this check can size, so it
// neither proves the snapshot has data nor makes it look empty.
func TestVerifySnapshotIgnoresNonRegularEntries(t *testing.T) {
	dir := t.TempDir()
	snapDir := filepath.Join(dir, "snapshots", "20260102T000000Z")
	if err := os.MkdirAll(snapDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/nonexistent/target", filepath.Join(snapDir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("20260102T000000Z", filepath.Join(dir, "snapshots", "latest")); err != nil {
		t.Fatal(err)
	}
	if err := (Server{}).VerifySnapshot(context.Background(), dir, mode.Snapshot{ID: "20260102T000000Z"}); err != nil {
		t.Fatalf("a symlink-only snapshot is not the all-empty-files shape: %v", err)
	}
}

// The marker's own content is what an operator reads, and it must not be
// mistakable for a successful run's stats.
func TestIncompleteReasonTextIsHonest(t *testing.T) {
	out := `
Number of files: 5 (reg: 4, dir: 1)
Number of created files: 5 (reg: 4, dir: 1)
Literal data: 0 bytes
`
	got := incompleteReasonText(out, &staticErr{"rsync: exit status 23: rsync: [sender] send_files failed to open \"/source/drone-runner.qcow2\": Permission denied (13)"})
	for _, want := range []string{"NOT a usable backup", "run failed: rsync: exit status 23", "files in rsync's file list: 5"} {
		if !strings.Contains(got, want) {
			t.Errorf("marker text missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "synced") {
		t.Errorf("marker text must not read like a successful run:\n%s", got)
	}
	// Multi-line rsync stderr is flattened: the marker's reason is one line that
	// survives being logged verbatim.
	multi := incompleteReasonText("", &staticErr{"rsync: failed to open a\nrsync: failed to open b\nrsync: failed to open c"})
	for _, line := range strings.Split(strings.TrimSpace(multi), "\n") {
		if strings.HasPrefix(line, "rsync: failed to open") && strings.Count(line, "rsync:") != 1 {
			t.Errorf("reason line not flattened: %q", line)
		}
	}
}

// incompleteReason on a directory with no marker reports "not incomplete", and
// on a marked one returns the run's error even if the marker cannot be parsed.
func TestIncompleteReason(t *testing.T) {
	if _, ok := incompleteReason(t.TempDir()); ok {
		t.Fatal("an unmarked directory must not be reported as incomplete")
	}
	dir := t.TempDir()
	if _, ok := incompleteReason(dir); ok {
		t.Fatal("a missing directory must not be reported as incomplete")
	}
	if err := os.WriteFile(filepath.Join(dir, IncompleteMarker), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reason, ok := incompleteReason(dir)
	if !ok {
		t.Fatal("a marked directory must be reported as incomplete even if its reason is unparseable")
	}
	if !strings.Contains(reason, "incomplete") {
		t.Errorf("reason = %q", reason)
	}
}
