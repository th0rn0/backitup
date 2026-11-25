// Package diskusage measures how much disk a directory tree actually occupies.
//
// It exists because "how big is this?" has two very different answers for a
// backup store, and the wrong one was on the dashboard:
//
//   - Apparent size (the sum of file lengths) is a lie for a store. A sparse VM
//     image claims 64 GiB and costs a few MiB, and rsync snapshots share one
//     inode across every snapshot that did not change a file, so summing file
//     sizes counts the same bytes once per snapshot. It overstates.
//   - Allocated blocks, counted once per inode, is what the operator is paying
//     for. That is the number this package returns: sum of st_blocks*512 over
//     every unique (st_dev, st_ino), which is also what `du` reports.
//
// A walk of a real store is O(files) and can take minutes, so Measure takes a
// cap. When the cap is hit the result says so (Partial) instead of quietly
// returning a number that looks exact: a truncated total is a lower bound, and
// the caller is expected to render it as one ("≥ 1.2 TB"), never as a total.
//
// The measurement is deliberately not taken on a request path: the lifecycle
// pass runs it once per client and persists the result, and pages read that.
package diskusage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DefaultMaxEntries bounds a measurement when the caller does not choose a cap.
//
// One million stat calls is a second or two of real time on a warm page cache
// and well under a minute on a cold spinning disk, which is a cost a background
// pass can pay once per client. It is deliberately not "unlimited": a
// pathological tree (a runaway snapshot, thousands of hardlinks, a mount that
// answers slowly) must not be able to stall the pass that also does retention.
const DefaultMaxEntries int64 = 1_000_000

// Result is one measurement.
type Result struct {
	// Bytes is the allocated space of every unique inode under the root, in
	// bytes. Directories are included: they hold blocks too and `du` counts
	// them.
	Bytes int64
	// Entries is how many directory entries were visited (and stat'd).
	Entries int64
	// Partial is true when the cap stopped the walk early. Bytes is then a
	// LOWER BOUND, not a total — render it as "≥".
	Partial bool
}

// Measure reports the allocated bytes under dir, counting each inode once.
//
// maxEntries > 0 caps the number of entries visited; 0 means DefaultMaxEntries
// and a negative value means no cap at all (used by tests that walk small
// trees and want an exact number). The cap bounds the work, not the answer: a
// capped walk returns Partial with the bytes it did see.
//
// An entry that cannot be stat'd is skipped rather than failing the total — a
// store with one unreadable file still has a size worth reporting. A root that
// cannot be stat'd IS an error: silently reporting 0 bytes for a mount that is
// not there is the failure mode this whole package exists to stop.
func Measure(ctx context.Context, dir string, maxEntries int64) (Result, error) {
	if maxEntries == 0 {
		maxEntries = DefaultMaxEntries
	}
	if _, err := os.Stat(dir); err != nil {
		return Result{}, fmt.Errorf("measure %s: %w", dir, err)
	}

	type inodeKey struct{ dev, ino uint64 }
	seen := make(map[inodeKey]struct{})
	var res Result

	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip it, keep the total
		}
		res.Entries++
		if maxEntries > 0 && res.Entries > maxEntries {
			res.Partial = true
			return fs.SkipAll
		}
		if res.Entries%2048 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		sys, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			// No inode information on this platform: fall back to apparent
			// size, counted per entry. Better than nothing, and documented.
			res.Bytes += info.Size()
			return nil
		}
		key := inodeKey{sys.Dev, sys.Ino}
		if _, dup := seen[key]; dup {
			return nil // a hardlink to an inode already counted
		}
		seen[key] = struct{}{}
		res.Bytes += int64(sys.Blocks) * 512
		return nil
	})
	if err != nil {
		return res, err
	}
	return res, nil
}
