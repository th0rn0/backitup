package lifecycle

import (
	"context"
	"log"
	"time"
)

// StartWorker runs the lifecycle maintenance worker on its own schedule
// (retention pruning, remote verification, integrity checks, stale alerts).
// It does NOT upload — uploads are handled by StartOffsiteWorker.
// Runs one pass shortly after start, then every interval.
func StartWorker(ctx context.Context, d Deps, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = time.Hour
	}
	d.staleAlerted = make(map[int64]time.Time)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			start := time.Now()
			if err := RunOnce(ctx, d); err != nil {
				log.Printf("lifecycle: pass completed with errors: %v", err)
			}
			next := interval - time.Since(start)
			if next < 0 {
				next = 0
			}
			timer.Reset(next)
		}
	}()
	return cancel
}

// StartOffsiteWorker uploads new snapshots to cold storage on its own schedule,
// independent of the maintenance lifecycle. pollInterval is how often all
// clients are checked; each client's OffsiteIntervalSecs controls whether an
// upload is actually due for that client on any given poll.
func StartOffsiteWorker(ctx context.Context, d Deps, pollInterval time.Duration) (stop func()) {
	if pollInterval <= 0 {
		pollInterval = 5 * time.Minute
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			start := time.Now()
			if err := RunOffsiteOnce(ctx, d); err != nil {
				log.Printf("offsite worker: pass completed with errors: %v", err)
			}
			next := pollInterval - time.Since(start)
			if next < 0 {
				next = 0
			}
			timer.Reset(next)
		}
	}()
	return cancel
}
