package client

import (
	"fmt"
	"os"
	"syscall"
)

// Lock is a held advisory file lock that prevents two runs of the SAME client
// from overlapping (a slow backup outrunning the next cron tick would corrupt
// the rsync snapshot chain; design doc client lockfile).
type Lock struct {
	f *os.File
}

// Acquire takes a non-blocking exclusive lock on path. held=false (with no
// error) means another run currently holds it — the caller should report
// "overlap" and exit cleanly.
func Acquire(path string) (l *Lock, held bool, err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("open lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, true, nil // someone else holds it
		}
		return nil, false, fmt.Errorf("flock %s: %w", path, err)
	}
	return &Lock{f: f}, false, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
