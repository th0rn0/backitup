package lifecycle

import (
	"context"
	"fmt"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// VisibleSnapshots splits a client's hot-store listing into the snapshots the
// server's own run record backs and the ones it says are not backups.
//
// The listing a mode returns is a statement about the filesystem: these
// directories exist. It is not a statement about backups, and the difference
// matters for exactly one case — a run that was killed before it could report
// (SIGKILL, host reboot, power loss, broken control channel, container
// OOM-reaped) leaves snapshots/<ts>/ behind and never says anything, so nothing
// marks it and the mode cannot tell it from a real backup. The server can: the
// run opened a row naming the second it started, and that row never reached ok.
// mode.UnbackedSnapshots is that judgement; this is the one place that fetches
// the run history for it, so the lifecycle worker and the HTTP handlers cannot
// disagree about which directories are backups.
//
// The answer is deliberately conservative in both directions:
//
//   - A snapshot with an `ok` run behind it is returned, always. Age does not
//     enter into it: a snapshot whose run row has been around for longer than
//     the log-retention window is still backed, and its log_tail being cleared
//     says nothing about the backup.
//   - A snapshot with no run row at all is returned, because absence is not
//     evidence: it may have been created outside backitup, or its run may have
//     been pruned by run-history retention. Only a row that is *there* and says
//     the run never reached ok can accuse a directory.
//
// On a store error it returns the listing unfiltered together with the error:
// a run history that could not be read is not evidence that something is not a
// backup, so the caller is handed the conservative answer. Callers that must not
// act on an unverified listing (the offsite worker, above all) treat the error as
// fatal instead.
func VisibleSnapshots(ctx context.Context, st *store.Store, c model.Client, sm mode.ServerMode, snaps []mode.Snapshot) (visible []mode.Snapshot, unbacked []mode.UnbackedSnapshot, err error) {
	named, ok := sm.(mode.RunStartNamed)
	if !ok || len(snaps) == 0 {
		// The mode's snapshot names are not the client's run start (tar.gz names
		// its archive server-side), so nothing can be attributed to a run row.
		return snaps, nil, nil
	}
	oldest := time.Time{}
	for _, s := range snaps {
		start, ok := named.RunStart(s)
		if !ok {
			continue // not the run-start shape; nothing to ask the history about
		}
		if oldest.IsZero() || start.Before(oldest) {
			oldest = start
		}
	}
	if oldest.IsZero() {
		return snaps, nil, nil
	}
	runs, err := st.ListRunsSince(ctx, c.ID, oldest.Add(-mode.RunStartWindow))
	if err != nil {
		return snaps, nil, fmt.Errorf("list run history: %w", err)
	}
	unbacked = mode.UnbackedSnapshots(sm, snaps, runs)
	if len(unbacked) == 0 {
		return snaps, nil, nil
	}
	hidden := make(map[string]bool, len(unbacked))
	for _, u := range unbacked {
		hidden[u.Snapshot.ID] = true
	}
	visible = make([]mode.Snapshot, 0, len(snaps)-len(unbacked))
	for _, s := range snaps {
		if !hidden[s.ID] {
			visible = append(visible, s)
		}
	}
	return visible, unbacked, nil
}
