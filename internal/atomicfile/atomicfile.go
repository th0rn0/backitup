// Package atomicfile writes a file by filling a temp file in the destination
// directory and renaming it over the target, so a reader — a human, sshd, or
// rclone — never observes a partially written file.
//
// Why this exists: the generated config files (rclone.conf, authorized_keys)
// are read by every offsite upload/prune/remote test and every SSH ingest. A
// truncate-then-write (os.WriteFile) on those paths leaves a zero-length or
// half-written file whenever the process dies or the disk fills mid-write, and
// every dependent operation then fails until an admin re-saves the setting.
// With temp-file + fsync + rename the previous contents stay live until the new
// bytes are durable: a failure at any point leaves the old file intact and the
// filesystem holding nothing but a stray temp file, which the deferred remove
// cleans up.
package atomicfile

import (
	"fmt"
	"os"
	"path/filepath"
)

// hookAfterWrite, when non-nil, runs after the temp file is fully written and
// before the rename. It exists only as a test seam so failures can be injected
// at the point where a real crash or ENOSPC would hit; production leaves it nil.
var hookAfterWrite func() error

// Write replaces path with data at the given permissions, atomically: the new
// content is written to a temp file in the same directory, synced, chmod'ed and
// renamed over path. On any error the original file is left untouched.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	// No-op once the rename succeeded; removes the temp file on every error path.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if hookAfterWrite != nil {
		if err := hookAfterWrite(); err != nil {
			return fmt.Errorf("injected fault: %w", err)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	// Best effort: make the rename itself durable against a host crash. The
	// data is already safe either way (the old file survives a lost rename).
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
