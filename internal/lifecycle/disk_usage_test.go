package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"

	// register the rsync ServerMode, which is what the pass measures
	_ "github.com/th0rn0/backitup/internal/mode/rsync"
)

// writeSnapshotFile creates snapshots/<id>/<name> under a client directory and
// returns its path.
func writeSnapshotFile(t *testing.T, clientDir, id, name string, size int) string {
	t.Helper()
	p := filepath.Join(clientDir, "snapshots", id, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// linkLatest points snapshots/latest at the newest snapshot, as a real rsync run
// leaves it, so the pass's integrity check is not reporting a broken tree that
// only the test's shortcut created.
func linkLatest(t *testing.T, clientDir, id string) {
	t.Helper()
	link := filepath.Join(clientDir, "snapshots", "latest")
	_ = os.Remove(link)
	if err := os.Symlink(id, link); err != nil {
		t.Fatalf("link latest -> %s: %v", id, err)
	}
}

// TestRunOnceMeasuresRsyncStoreCountingHardlinksOnce is the regression test for
// the "11 snapshots, 0 B" report: rsync reports no per-snapshot size, so the page
// must show a measurement of the store — and that measurement must count an
// unchanged file once, because in rsync mode every unchanged file is a hardlink
// shared with the previous snapshot.
func TestRunOnceMeasuresRsyncStoreCountingHardlinksOnce(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "vm-host", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	first := writeSnapshotFile(t, dir, "20260617T120000Z", "payload.bin", 256*1024)
	second := filepath.Join(dir, "snapshots", "20260618T120000Z", "payload.bin")
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hardlinks are unavailable here: %v", err)
	}
	linkLatest(t, dir, "20260618T120000Z")

	deps := Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }, DiskUsageMaxEntries: -1}
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	got, err := st.GetClientStorage(context.Background(), id)
	if err != nil {
		t.Fatalf("load measurement: %v", err)
	}
	if got == nil {
		t.Fatal("the pass did not record a disk measurement for an rsync client with snapshots")
	}
	if !got.MeasuredAt.Equal(now) {
		t.Errorf("measured_at = %v, want the pass's clock %v", got.MeasuredAt, now)
	}
	info, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no inode information on this platform")
	}
	one := int64(sys.Blocks) * 512
	if got.Bytes >= 2*one {
		t.Fatalf("the shared hardlink was counted once per snapshot: measured %d bytes, one copy is %d", got.Bytes, one)
	}
	if got.Bytes < one {
		t.Fatalf("measured %d bytes, less than the single copy %d the store holds", got.Bytes, one)
	}
	if got.Partial {
		t.Fatal("an uncapped pass reported a lower bound")
	}
}

// TestRunOnceDiskUsageCapStoresALowerBound: a pathological tree must cost the
// cap, not the pass, and the stored figure must be marked as a lower bound so the
// page can say "≥" instead of presenting a truncated number as a total.
func TestRunOnceDiskUsageCapStoresALowerBound(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "runaway", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	for i := 0; i < 40; i++ {
		writeSnapshotFile(t, dir, "20260618T120000Z", "f"+string(rune('a'+i%26))+string(rune('0'+i/26))+".bin", 4096)
	}
	linkLatest(t, dir, "20260618T120000Z")

	deps := Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }, DiskUsageMaxEntries: 5}
	logs := runOnceCapturingLog(t, deps)

	got, err := st.GetClientStorage(context.Background(), id)
	if err != nil {
		t.Fatalf("load measurement: %v", err)
	}
	if got == nil {
		t.Fatal("the capped pass recorded nothing; a lower bound is more useful than no figure")
	}
	if !got.Partial {
		t.Fatal("the walk hit its cap but the stored figure is not marked partial")
	}
	if got.Bytes <= 0 {
		t.Fatalf("the capped measurement is %d bytes; a lower bound must cover what it did see", got.Bytes)
	}
	if !strings.Contains(logs, "lower bound") {
		t.Fatalf("the pass did not say the figure is a lower bound; it logged:\n%s", logs)
	}
}

// TestRunOnceMeasuresTargzStoreTheSameWay: tar.gz already reports real
// per-snapshot sizes, but its ON DISK total uses the same measurement as rsync so
// the two modes read consistently — and the figure is allocated bytes, not the
// archive's apparent length.
func TestRunOnceMeasuresTargzStoreTheSameWay(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "docs", Mode: model.ModeTarGz, RetentionDays: 14, Enabled: true,
	})
	mkArchiveContent(t, filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz"), now.Add(-time.Hour), strings.Repeat("payload", 4096))

	deps := Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }, DiskUsageMaxEntries: -1}
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := st.GetClientStorage(context.Background(), id)
	if err != nil {
		t.Fatalf("load measurement: %v", err)
	}
	if got == nil || got.Bytes <= 0 {
		t.Fatalf("tar.gz client has no positive measured figure: %+v", got)
	}
}

// TestRunOnceDoesNotStoreAZeroForAClientWithNoStore: a client whose first backup
// has not landed has no directory to measure. That is "no measurement yet", not
// "0 B" — a row saying zero would be a confident claim about an empty store.
func TestRunOnceDoesNotStoreAZeroForAClientWithNoStore(t *testing.T) {
	st := openStore(t)
	base := t.TempDir()
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, err := st.CreateClient(context.Background(), model.Client{
		Name: "brand-new", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	deps := Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := st.GetClientStorage(context.Background(), id)
	if err != nil {
		t.Fatalf("load measurement: %v", err)
	}
	if got != nil {
		t.Fatalf("a client with no store was measured as %+v; there is nothing to measure yet", got)
	}
}

// TestRunOnceRefreshesTheMeasurement: the figure is refreshed every pass, so a
// store that grew is reported at its new size with a new timestamp.
func TestRunOnceRefreshesTheMeasurement(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "grower", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	writeSnapshotFile(t, dir, "20260618T120000Z", "one.bin", 64*1024)
	linkLatest(t, dir, "20260618T120000Z")

	deps := Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }, DiskUsageMaxEntries: -1}
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	first, err := st.GetClientStorage(context.Background(), id)
	if err != nil || first == nil {
		t.Fatalf("first measurement: %v (got %+v)", err, first)
	}

	writeSnapshotFile(t, dir, "20260618T130000Z", "two.bin", 2*1024*1024)
	linkLatest(t, dir, "20260618T130000Z")
	later := now.Add(5 * time.Minute)
	deps.Now = func() time.Time { return later }
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
	second, err := st.GetClientStorage(context.Background(), id)
	if err != nil || second == nil {
		t.Fatalf("second measurement: %v (got %+v)", err, second)
	}
	if second.Bytes <= first.Bytes {
		t.Fatalf("the figure did not grow with the store: %d -> %d", first.Bytes, second.Bytes)
	}
	if !second.MeasuredAt.Equal(later) {
		t.Errorf("measured_at = %v, want the second pass's clock %v", second.MeasuredAt, later)
	}
}
