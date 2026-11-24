package rsync

import (
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
)

// The server attributes a snapshot directory to the run row that named it, which
// it can only do if the mode says what run start the name encodes. rsync names a
// snapshot after the clock it read at the top of the run, so the parse is the
// whole contract — and a name that is not that shape must be refused rather than
// guessed at (a hand-placed directory is nobody's run).
func TestRunStartReadsTheSnapshotName(t *testing.T) {
	start, ok := Server{}.RunStart(mode.Snapshot{ID: "20260102T150405Z"})
	if !ok {
		t.Fatal("RunStart refused a real snapshot name")
	}
	want := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	if !start.Equal(want) {
		t.Fatalf("RunStart = %s, want %s", start, want)
	}
	for _, name := range []string{"backup-20260102T150405Z-000.tar.gz", "2026-01-02T15:04:05", "not-a-timestamp", ""} {
		if ts, ok := (Server{}).RunStart(mode.Snapshot{ID: name}); ok {
			t.Errorf("RunStart(%q) = %s, true; want refused", name, ts)
		}
	}
	if _, isNamed := any(Server{}).(mode.RunStartNamed); !isNamed {
		t.Fatal("rsync must implement mode.RunStartNamed: nothing else can attribute its directories to runs")
	}
}
