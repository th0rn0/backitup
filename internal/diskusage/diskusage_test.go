package diskusage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestMeasureCountsHardlinkedInodesOnce is the property the dashboard depends
// on: every unchanged file in an rsync snapshot is a hardlink into the previous
// snapshot, so a total that counts file bytes once per directory entry would
// multiply the store by the number of snapshots it holds.
func TestMeasureCountsHardlinkedInodesOnce(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "snapshots", "20260101T000000Z", "payload.bin")
	if err := os.MkdirAll(filepath.Dir(first), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, make([]byte, 256*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	// A second snapshot holding a hardlink to the same inode, which is exactly
	// what rsync --link-dest produces for an unchanged file.
	second := filepath.Join(root, "snapshots", "20260102T000000Z", "payload.bin")
	if err := os.MkdirAll(filepath.Dir(second), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hardlinks are unavailable here: %v", err)
	}

	res, err := Measure(t.Context(), root, -1)
	if err != nil {
		t.Fatalf("measure: %v", err)
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
	if one == 0 {
		t.Fatalf("the test file allocated no blocks, so it cannot prove anything")
	}
	// The two leaves share one inode, so the tree must cost about one copy plus
	// directory blocks — never close to two copies.
	if res.Bytes >= 2*one {
		t.Fatalf("hardlinked file counted twice: measured %d bytes, one copy is %d", res.Bytes, one)
	}
	if res.Bytes < one {
		t.Fatalf("measured %d bytes, smaller than the single copy %d it holds", res.Bytes, one)
	}
	if res.Partial {
		t.Fatal("an uncapped walk reported a lower bound")
	}
}

// TestMeasureCountsSparseFilesByAllocatedBlocks pins the other half of "honest":
// a sparse file costs its allocated blocks, not the length it claims. The figure
// must agree with `du -sk` for the same tree; the tolerance is 64 KiB because
// GNU du reports whole 1 KiB blocks and filesystem bookkeeping (directory
// blocks, block rounding) differs slightly between tools. It is deliberately
// small compared with the 8 MiB apparent size.
func TestMeasureCountsSparseFilesByAllocatedBlocks(t *testing.T) {
	duPath, err := exec.LookPath("du")
	if err != nil {
		t.Skip("du is not installed here")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "snapshots", "s1"), 0o755); err != nil {
		t.Fatal(err)
	}
	sparse := filepath.Join(root, "snapshots", "s1", "disk.img")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	const apparent = 8 << 20
	if err := f.Truncate(apparent); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	res, err := Measure(t.Context(), root, -1)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if res.Bytes >= apparent {
		t.Fatalf("a sparse file was counted by apparent size: measured %d bytes for a %d-byte file", res.Bytes, int64(apparent))
	}

	out, err := exec.Command(duPath, "-sk", root).Output()
	if err != nil {
		t.Fatalf("du -sk: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("du printed %q", out)
	}
	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		t.Fatalf("parse du output %q: %v", out, err)
	}
	duBytes := kib * 1024
	const tolerance = 64 << 10
	if diff := res.Bytes - duBytes; diff > tolerance || diff < -tolerance {
		t.Fatalf("measured %d bytes but du -sk says %d bytes (difference %d exceeds the %d-byte tolerance)",
			res.Bytes, duBytes, diff, int64(tolerance))
	}
}

// TestMeasureStopsAtTheCapAndReportsALowerBound is the bound the lifecycle needs:
// a pathological tree must cost the cap, not the pass, and the caller must be
// able to tell a truncated number from a total.
func TestMeasureStopsAtTheCapAndReportsALowerBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 40; i++ {
		name := filepath.Join(root, "f"+strconv.Itoa(i))
		if err := os.WriteFile(name, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	full, err := Measure(t.Context(), root, -1)
	if err != nil {
		t.Fatalf("uncapped measure: %v", err)
	}
	capped, err := Measure(t.Context(), root, 5)
	if err != nil {
		t.Fatalf("capped measure: %v", err)
	}
	if !capped.Partial {
		t.Fatal("the walk hit its cap but did not say so; a truncated total would be rendered as exact")
	}
	if capped.Entries != 6 {
		// The entry that exceeds the cap is visited but not counted.
		t.Fatalf("capped walk visited %d entries, want cap+1 = 6", capped.Entries)
	}
	if capped.Bytes <= 0 || capped.Bytes >= full.Bytes {
		t.Fatalf("capped bytes = %d, want a positive lower bound below the full %d", capped.Bytes, full.Bytes)
	}
	if full.Partial {
		t.Fatal("the uncapped walk reported a lower bound")
	}
}

// TestMeasureMissingRootIsAnError: the store's root is the one thing that must
// not degrade to zero. A dashboard reporting "0 B" for a volume that is not
// mounted is the bug this package exists to prevent.
func TestMeasureMissingRootIsAnError(t *testing.T) {
	if _, err := Measure(t.Context(), filepath.Join(t.TempDir(), "absent"), -1); err == nil {
		t.Fatal("measuring a directory that does not exist returned no error")
	}
}
