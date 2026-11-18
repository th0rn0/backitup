package server

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// newMeterStack builds a Server over a temp backup dir with n files in it.
func newMeterStack(t *testing.T, clients, snapsPerClient, filesPerSnap int) (*Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "meter.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	base := filepath.Join(t.TempDir(), "backups")
	srv.backupBaseDir = base
	payload := make([]byte, 4096)
	for c := 0; c < clients; c++ {
		for s := 0; s < snapsPerClient; s++ {
			dir := filepath.Join(base, "client", "snapshots", "2026010"+string(rune('0'+s)), "data")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for f := 0; f < filesPerSnap; f++ {
				if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d-%04d.bin", f%26, f)), payload, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	return srv, base
}

// TestDashboardDoesNotWalkTheBackupTree is the regression test for the bug: no
// page load may trigger a filesystem walk of the backup tree. It counts walks
// rather than timing them, so it cannot pass by being fast on CI hardware.
func TestDashboardDoesNotWalkTheBackupTree(t *testing.T) {
	srv, base := newMeterStack(t, 6, 8, 120)
	ctx := context.Background()

	var walks int
	srv.measureStorage = func(ctx context.Context, dir string) (int64, error) {
		walks++
		return measureHotBytes(ctx, dir)
	}

	for i := 0; i < 5; i++ {
		v, err := srv.buildDashboard(ctx)
		if err != nil {
			t.Fatalf("buildDashboard: %v", err)
		}
		if v.Storage.HotMeasured {
			t.Fatal("dashboard claimed a measured total before the meter ran")
		}
		if v.Storage.HotBytes != 0 {
			t.Fatalf("dashboard reported %d hot bytes with no measurement", v.Storage.HotBytes)
		}
	}
	if walks != 0 {
		t.Fatalf("five dashboard builds triggered %d filesystem walk(s) of the backup tree", walks)
	}

	// The meter itself does walk, once per run.
	if _, err := srv.MeasureStorageOnce(ctx); err != nil {
		t.Fatalf("measure: %v", err)
	}
	if walks != 1 {
		t.Fatalf("MeasureStorageOnce triggered %d walks, want 1", walks)
	}
	if _, measured := srv.hotBytes(); !measured {
		t.Fatal("measurement did not mark the total as measured")
	}
	_ = base
}

func TestStorageMeterMeasuresAndExposesTheTotal(t *testing.T) {
	srv, base := newMeterStack(t, 2, 2, 10)

	if _, measured := srv.hotBytes(); measured {
		t.Fatal("measured before any run")
	}
	total, err := srv.MeasureStorageOnce(context.Background())
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if total <= 0 {
		t.Fatalf("expected a positive total, got %d", total)
	}
	got, measured := srv.hotBytes()
	if !measured || got != total {
		t.Fatalf("hotBytes() = (%d, %v), want (%d, true)", got, measured, total)
	}

	// A new file must show up on the next measurement.
	if err := os.WriteFile(filepath.Join(base, "client", "extra.bin"), make([]byte, 8192), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := srv.MeasureStorageOnce(context.Background())
	if err != nil {
		t.Fatalf("second measure: %v", err)
	}
	if after <= total {
		t.Fatalf("total did not grow after adding a file: %d -> %d", total, after)
	}
}

// TestStorageMeterCountsHardlinksOnce preserves the property the old request-path
// walk had: rsync snapshots share inodes, and the dashboard must not count the
// same bytes once per snapshot.
func TestStorageMeterCountsHardlinksOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "hl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv := New(st, false)
	base := filepath.Join(t.TempDir(), "backups")
	srv.backupBaseDir = base

	orig := filepath.Join(base, "a", "snapshots", "s1", "blob.bin")
	if err := os.MkdirAll(filepath.Dir(orig), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orig, make([]byte, 64*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second snapshot holding a hardlink to the same inode.
	link := filepath.Join(base, "a", "snapshots", "s2", "blob.bin")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(orig, link); err != nil {
		t.Skipf("hardlinks unavailable here: %v", err)
	}

	total, err := srv.MeasureStorageOnce(context.Background())
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	sys, ok := mustStat(t, orig).Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no inode information on this platform")
	}
	one := int64(sys.Blocks) * 512
	if total > one*2 {
		t.Fatalf("hardlinked file counted twice: total=%d, one copy=%d", total, one)
	}
	if total < one {
		t.Fatalf("total=%d smaller than a single copy=%d", total, one)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info
}

// TestStorageMeterRefreshesInBackground proves the loop actually runs and that a
// cancelled context stops it.
func TestStorageMeterRefreshesInBackground(t *testing.T) {
	srv, base := newMeterStack(t, 1, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	stop := srv.StartStorageMeter(ctx, 20*time.Millisecond)
	defer func() { stop(); cancel() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, measured := srv.hotBytes(); measured {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("meter never produced a first measurement")
		}
		time.Sleep(10 * time.Millisecond)
	}
	first, _ := srv.hotBytes()

	if err := os.WriteFile(filepath.Join(base, "client", "new.bin"), make([]byte, 32*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		got, _ := srv.hotBytes()
		if got > first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("meter did not refresh: still %d after adding a file (first %d)", got, first)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDashboardStorageRowSaysMeasuringUntilMeasured pins the template branch:
// before the background meter has produced a number the row must say so, rather
// than rendering a confident "0 B" for a fleet that may hold terabytes.
func TestDashboardStorageRowSaysMeasuringUntilMeasured(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "tmpl.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	srv := New(st, false)

	render := func(measured bool) string {
		t.Helper()
		var buf bytes.Buffer
		v := dashboardView{
			Clients: []clientRow{{Name: "c1", Health: string(model.HealthOK)}},
			Storage: storageStats{HotBytes: 1234, HotMeasured: measured},
		}
		if err := srv.tmpl.ExecuteTemplate(&buf, "dashboard.html", v); err != nil {
			t.Fatalf("render dashboard: %v", err)
		}
		return buf.String()
	}

	if got := render(false); !strings.Contains(got, "measuring") {
		t.Fatalf("unmeasured dashboard did not say it was measuring:\n%s", firstStatLine(got))
	}
	got := render(true)
	if strings.Contains(got, "measuring") {
		t.Fatalf("measured dashboard still says measuring")
	}
	if !strings.Contains(got, "1.2 KB") && !strings.Contains(got, "1.21 KB") && !strings.Contains(got, "1 KB") {
		t.Fatalf("measured dashboard did not render the size:\n%s", firstStatLine(got))
	}
}

func firstStatLine(html string) string {
	for _, line := range strings.Split(html, "\n") {
		if strings.Contains(line, "stat-value") {
			return line
		}
	}
	return "(no stat-value line)"
}
