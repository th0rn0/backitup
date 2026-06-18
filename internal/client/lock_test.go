package client

import (
	"path/filepath"
	"testing"
)

func TestLockExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.lock")

	l1, held, err := Acquire(path)
	if err != nil || held {
		t.Fatalf("first acquire: held=%v err=%v", held, err)
	}

	// Second acquire while the first is held -> overlap (held=true, no error).
	l2, held, err := Acquire(path)
	if err != nil {
		t.Fatalf("second acquire err: %v", err)
	}
	if !held {
		t.Fatal("expected held=true while lock is taken")
	}
	if l2 != nil {
		t.Fatal("held lock should return nil *Lock")
	}

	// After release, it can be acquired again.
	l1.Release()
	l3, held, err := Acquire(path)
	if err != nil || held {
		t.Fatalf("re-acquire after release: held=%v err=%v", held, err)
	}
	l3.Release()
}

func TestLockBadPath(t *testing.T) {
	if _, _, err := Acquire(filepath.Join(t.TempDir(), "no-such-dir", "x.lock")); err == nil {
		t.Fatal("expected error opening lock in nonexistent dir")
	}
}

func TestReleaseNilSafe(t *testing.T) {
	var l *Lock
	l.Release() // must not panic
}
