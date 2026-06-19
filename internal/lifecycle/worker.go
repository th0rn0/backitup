package lifecycle

import (
	"context"
	"log"
	"time"
)

// StartWorker runs the lifecycle on its own schedule (the server's maintenance
// cadence — distinct from client backup schedules, which it never owns). It runs
// one pass shortly after start, then every interval, until the returned stop
// function is called or ctx is cancelled.
func StartWorker(ctx context.Context, d Deps, interval time.Duration) (stop func()) {
	if interval <= 0 {
		interval = time.Hour
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		// Small initial delay so the server finishes booting first.
		timer := time.NewTimer(15 * time.Second)
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
			timer.Reset(interval)
		}
	}()
	return cancel
}
