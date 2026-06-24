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
		interval time.Duration
		want     Health
	}{
		{"no runs ever", nil, hour, HealthNever},
		{"last run failed", &Run{Status: StatusFailed, FinishedAt: now.Add(-2 * hour)}, hour, HealthFailed},
		{"ok within cadence", ok(30 * time.Minute), hour, HealthOK},
		{"ok just over 2x cadence", ok(150 * time.Minute), hour, HealthStale},
		{"ok but no cadence known", ok(1000 * hour), 0, HealthOK},
		{"failed beats staleness", &Run{Status: StatusFailed, FinishedAt: now.Add(-1000 * hour)}, hour, HealthFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveHealth(tc.latest, tc.interval, now); got != tc.want {
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
