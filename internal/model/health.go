package model

import "time"

// DeriveHealth computes the dashboard health state (DD2) for a client.
//
// latest is the most recent run row of any status; lastOK is the most recent run
// that actually SUCCEEDED (status=ok), which may be nil when nothing has ever
// succeeded. Health is aged from lastOK, never from latest: an overlap row says a
// trigger found the previous backup still running — evidence that a backup did
// not happen, not evidence that one did — and a failed row says the same. Keying
// off "the newest row" let a client whose cron overruns its backup post nothing
// but overlaps and stay green ("ok / just now") while its last real backup was
// hours old, which is precisely the client an operator needs to see.
//
//	latest == nil                        -> Never
//	latest running, < 6h old             -> Running
//	latest running, >= 6h old            -> Stale (orphaned by a crashed client)
//	latest failed                        -> Failed
//	no successful run ever               -> Never
//	last success older than 2x cadence   -> Stale
//	otherwise                            -> OK
//
// expectedInterval <= 0 disables staleness (we cannot judge cadence we were never
// told about), so a client with any successful run is simply OK.
func DeriveHealth(latest, lastOK *Run, expectedInterval time.Duration, now time.Time) Health {
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
	// Overlap and ok both fall through to the age of the last SUCCESSFUL run.
	if lastOK == nil {
		// Only overlaps and/or failures so far: nothing has ever been backed up.
		return HealthNever
	}
	if expectedInterval > 0 && now.Sub(lastOK.FinishedAt) > 2*expectedInterval {
		return HealthStale
	}
	return HealthOK
}
