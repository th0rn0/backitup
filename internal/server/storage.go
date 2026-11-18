package server

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// defaultStorageRefresh is how often the backup-tree size is re-measured.
const defaultStorageRefresh = 60 * time.Second

// storageMeter keeps the on-disk size of the backup tree available to the
// dashboard without ever walking it on a request path.
//
// The walk is O(files) and the backup tree is the largest thing this process
// touches: rsync snapshots turn it into hundreds of thousands of files, and the
// count grows with exactly the thing the dashboard is there to report. Measuring
// inside the handler meant the storage row cost as long as the fleet was big —
// measured at 263ms for 50k files on fast local NVMe, so minutes on a real
// volume on a busy disk — and the measurement held a mutex for the whole walk,
// so every concurrent page load queued behind the same walk. The busier the
// fleet, the more reliably the page that reports its health was the page that
// would not load.
//
// A background goroutine now refreshes the number and handlers read an atomic.
// Until the first measurement finishes the dashboard says "measuring" rather
// than reporting a confident zero.
type storageMeter struct {
	bytes    atomic.Int64
	measured atomic.Bool
	interval atomic.Int64 // nanoseconds
	start    sync.Once
}

// measureHotBytes walks baseDir and returns the space its files occupy, counting
// hardlinked files once (rsync snapshots share inodes across snapshots).
//
// Allocated blocks (st_blocks, 512-byte units) are used rather than logical
// size, to match what `du` reports: logical size overstates usage on compressed
// or sparse filesystems.
func measureHotBytes(ctx context.Context, baseDir string) (int64, error) {
	// A walk whose root cannot be stat'd must say so. Swallowing it reports a
	// confident 0 bytes for a misconfigured backup dir, which is the failure
	// mode this product keeps producing: silence that reads as health.
	if _, err := os.Stat(baseDir); err != nil {
		return 0, fmt.Errorf("measure %s: %w", baseDir, err)
	}
	type inodeKey struct{ dev, ino uint64 }
	seen := make(map[inodeKey]struct{})
	var total int64
	var walked int
	err := filepath.WalkDir(baseDir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable entry must not fail the whole total
		}
		if walked++; walked%2048 == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		sys, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			// No inode information on this platform: count every entry.
			total += info.Size()
			return nil
		}
		key := inodeKey{sys.Dev, sys.Ino}
		if _, dup := seen[key]; dup {
			return nil
		}
		seen[key] = struct{}{}
		total += int64(sys.Blocks) * 512
		return nil
	})
	if err != nil {
		return total, err
	}
	return total, nil
}

// hotBytes returns the last measured size and whether a measurement has ever
// completed. It never touches the filesystem.
func (s *Server) hotBytes() (int64, bool) {
	return s.storage.bytes.Load(), s.storage.measured.Load()
}

// MeasureStorageOnce runs a single measurement now and records it. Exported for
// the wiring and for tests; the dashboard never calls it.
func (s *Server) MeasureStorageOnce(ctx context.Context) (int64, error) {
	measure := s.measureStorage
	if measure == nil {
		measure = measureHotBytes
	}
	total, err := measure(ctx, s.backupBaseDir)
	if err != nil && total == 0 {
		return 0, err
	}
	s.storage.bytes.Store(total)
	s.storage.measured.Store(true)
	return total, err
}

// StartStorageMeter refreshes the backup-tree size in the background until the
// context is cancelled or the returned stop function is called. It is safe to
// call more than once; only the first call starts a loop.
func (s *Server) StartStorageMeter(ctx context.Context, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = defaultStorageRefresh
	}
	s.storage.interval.Store(int64(interval))
	ctx, cancel := context.WithCancel(ctx)
	s.storage.start.Do(func() {
		go func() {
			tick := time.NewTicker(interval)
			defer tick.Stop()
			for {
				// Measure first so the dashboard is populated within one walk
				// rather than one interval plus one walk.
				if _, err := s.MeasureStorageOnce(ctx); err != nil && ctx.Err() == nil {
					// Unreadable tree: keep the previous value, try again next tick.
					_ = err
				}
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
				}
			}
		}()
	})
	return cancel
}
