package model

import "time"

// DeriveHealth computes the dashboard health state (DD2) for a client from its
// most recent run and its advisory expected interval.
//
//	latest == nil                         -> Never
//	latest.Status == failed               -> Failed
//	ok but older than 2x expected cadence -> Stale
//	otherwise                             -> OK
//
// expectedInterval <= 0 disables staleness (we cannot judge cadence we were never
// told about), so a recent successful run is simply OK.
func DeriveHealth(latest *Run, expectedInterval time.Duration, now time.Time) Health {
	if latest == nil {
		return HealthNever
	}
	// Running takes priority: show live activity regardless of past state.
	// Treat as stale if the run has been "running" for more than 6 hours (likely
	// orphaned by a crashed client that never posted a final status).
	if latest.Status == StatusRunning {
		if now.Sub(latest.StartedAt) > 6*time.Hour {
			return HealthStale
		}
		return HealthRunning
	}
	if latest.Status == StatusFailed {
		return HealthFailed
	}
	if latest.Status == StatusOK && expectedInterval > 0 {
		if now.Sub(latest.FinishedAt) > 2*expectedInterval {
			return HealthStale
		}
	}
	return HealthOK
}
