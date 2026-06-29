package lifecycle

import (
	"context"
	"log"
	"time"
)

// nextAlignedTick returns the duration until the next UTC clock boundary for
// the given interval. A 12h interval fires at 00:00 and 12:00 UTC; a 1h
// interval fires at the top of each hour — independent of when the process
// started.
func nextAlignedTick(interval time.Duration) time.Duration {
	now := time.Now().UTC()
	next := now.Truncate(interval).Add(interval)
	return time.Until(next)
}

// StartWorker runs the lifecycle maintenance worker on a cron-aligned schedule
// (retention pruning, remote verification, integrity checks, stale alerts).
// It does NOT upload — uploads are handled by StartOffsiteWorker.
// The first tick fires at the next UTC interval boundary (e.g. 12h fires at
// the next midnight or noon), not a fixed delay after container start.
func StartWorker(ctx context.Context, d Deps, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = time.Hour
	}
	d.staleAlerted = make(map[int64]time.Time)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		timer := time.NewTimer(nextAlignedTick(interval))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if err := RunOnce(ctx, d); err != nil {
				log.Printf("lifecycle: pass completed with errors: %v", err)
			}
			timer.Reset(nextAlignedTick(interval))
		}
	}()
	return cancel
}

// StartOffsiteWorker uploads new snapshots to cold storage on a cron-aligned
// schedule, independent of the maintenance lifecycle. pollInterval is how
// often all clients are checked; each client's OffsiteIntervalSecs controls
// whether an upload is actually due for that client on any given poll.
func StartOffsiteWorker(ctx context.Context, d Deps, pollInterval time.Duration) (stop func()) {
	if pollInterval <= 0 {
		pollInterval = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		timer := time.NewTimer(nextAlignedTick(pollInterval))
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if err := RunOffsiteOnce(ctx, d); err != nil {
				log.Printf("offsite worker: pass completed with errors: %v", err)
			}
			timer.Reset(nextAlignedTick(pollInterval))
		}
	}()
	return cancel
}
