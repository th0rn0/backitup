package model

import (
	"testing"
	"time"
)

func TestDeriveHealth(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	hour := time.Hour
	ok := func(ago time.Duration) *Run {
		return &Run{Status: StatusOK, FinishedAt: now.Add(-ago)}
	}

	cases := []struct {
		name     string
		latest   *Run
		lastOK   *Run
		interval time.Duration
		want     Health
	}{
		{"no runs ever", nil, nil, hour, HealthNever},
		{"last run failed", &Run{Status: StatusFailed, FinishedAt: now.Add(-2 * hour)}, ok(2 * hour), hour, HealthFailed},
		{"ok within cadence", ok(30 * time.Minute), ok(30 * time.Minute), hour, HealthOK},
		{"ok just over 2x cadence", ok(150 * time.Minute), ok(150 * time.Minute), hour, HealthStale},
		{"ok but no cadence known", ok(1000 * hour), ok(1000 * hour), 0, HealthOK},
		{"failed beats staleness", &Run{Status: StatusFailed, FinishedAt: now.Add(-1000 * hour)}, nil, hour, HealthFailed},

		// S8: an overlap is not a backup. Neither the overlap row's freshness nor
		// its presence may talk the client back to "ok".
		{"fresh overlap after an old success", &Run{Status: StatusOverlap, FinishedAt: now.Add(-time.Minute)}, ok(5 * hour), hour, HealthStale},
		{"overlaps only, nothing ever succeeded", &Run{Status: StatusOverlap, FinishedAt: now.Add(-time.Minute)}, nil, hour, HealthNever},
		{"overlap after a recent success", &Run{Status: StatusOverlap, FinishedAt: now.Add(-time.Minute)}, ok(10 * time.Minute), hour, HealthOK},
		{"overlap, no cadence known, success exists", &Run{Status: StatusOverlap, FinishedAt: now.Add(-time.Minute)}, ok(100 * hour), 0, HealthOK},

		// Running still wins (in-flight activity is honest), and an orphaned one
		// is stale regardless of past successes.
		{"fresh running", &Run{Status: StatusRunning, StartedAt: now.Add(-5 * time.Minute)}, ok(5 * hour), hour, HealthRunning},
		{"orphaned running", &Run{Status: StatusRunning, StartedAt: now.Add(-7 * hour)}, ok(10 * time.Minute), hour, HealthStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveHealth(tc.latest, tc.lastOK, tc.interval, now); got != tc.want {
				t.Fatalf("DeriveHealth = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCapLogTail(t *testing.T) {
	if got := CapLogTail("short"); got != "short" {
		t.Fatalf("short log changed: %q", got)
	}
	big := make([]byte, MaxLogTail+500)
	for i := range big {
		big[i] = 'x'
	}
	if got := CapLogTail(string(big)); len(got) != MaxLogTail {
		t.Fatalf("cap = %d bytes, want %d", len(got), MaxLogTail)
	}
}
