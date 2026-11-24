package mode

import (
	"fmt"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// RunStartWindow is how far a snapshot's name and a run row's started_at may
// differ and still describe the same run.
//
// The two are read from the same clock inside the same run: the client posts
// `running` with started_at, then the mode formats the snapshot name. They are
// separated by one control-channel round trip, so normally by milliseconds; the
// window is a second of slack for a slow link, not a policy. A client holds a
// lockfile for the whole run, so a *second* run of the same client cannot start
// inside the window — which is what keeps this from blaming a neighbour.
const RunStartWindow = time.Minute

// reasonNoteLimit bounds how much of a run's log tail is copied into the reason
// line. The reason is printed verbatim by the lifecycle, once per client per
// pass, so it has to stay one readable line.
const reasonNoteLimit = 160

// UnbackedSnapshot is a snapshot on the hot store that the server's own run
// record says is not a backup: a directory that no run ever reported as ok.
//
// It is the second half of the failed-run guard. The first half is the marker a
// failed rsync run writes into its own directory (rsync.IncompleteMarker), which
// covers a run that reaches its own error path. This covers the run that does
// not: killed by SIGKILL, the host rebooted, power lost, the control channel
// broken, the container OOM-reaped. Nothing arrives from that client at all, so
// nothing marks the half-copied tree — and the server has to work it out from
// what it already knows, which is that the run never reached ok.
type UnbackedSnapshot struct {
	Snapshot Snapshot
	// RunID is the run that created this directory and never reached ok.
	RunID int64
	// Status is what that run is recorded as: failed, or overlap. Never ran nor ok.
	Status model.RunStatus
	// Since is when the server learned the run would not reach ok — the run's
	// finished_at for a run that reported its failure, the moment the server
	// declared a silent run dead for one it had to mark itself. It is the age a
	// caller's grace period should be measured against: the run's start would be
	// the wrong end, because a slow-but-alive run has a start long before it is
	// known to be dead.
	Since time.Time
	// Reason says why, in one line, for the log and for the operator.
	Reason string
}

// UnbackedSnapshots returns the snapshots that no `ok` run of the client backs.
//
// The rule, and why it is safe to act on:
//
//   - An `ok` run whose snapshot_id names the entry settles it: that is a
//     backup, whatever else the history holds. This is checked first, and it is
//     what keeps an old snapshot whose run row is still there from being
//     touched, however old the row is.
//   - A run row that started when the snapshot was named (within RunStartWindow)
//     and is recorded as anything other than ok — failed, or overlap — says the
//     run that produced this directory never finished it. hide/reclaim is
//     measured from that run's own record, never from the client's word.
//   - A run row in that window that is still `running` says the opposite: the
//     backup may be writing into the directory right now, so it is left alone
//     whatever its age. (The lifecycle marks runs stale as failed before it does
//     anything else in a pass, so a run that was killed does not stay `running`
//     past BACKITUP_STALE_RUN_TIMEOUT.)
//   - No row in the window says nothing at all, and nothing is returned. Absence
//     of evidence is not evidence: the directory may have been created outside
//     backitup, it may come from a client version that predates the runs table,
//     or its run row may have been pruned by BACKITUP_LOG_RETENTION_DAYS/
//     BACKITUP_RUNS_KEEP_DAYS. There is no way to tell those apart from the
//     database, so the fallback is the conservative one: such a snapshot stays a
//     snapshot. Those two cases are distinguished by the *presence* of a row, not
//     by its age — a row that was pruned cannot be mistaken for a run that never
//     finished, because it is simply gone, and only a row that is there can
//     accuse a directory.
//
// It returns nothing for a mode that does not name its snapshots after the run
// start (see RunStartNamed): there is nothing to attribute.
func UnbackedSnapshots(sm ServerMode, snaps []Snapshot, runs []model.Run) []UnbackedSnapshot {
	named, ok := sm.(RunStartNamed)
	if !ok {
		return nil
	}
	backed := make(map[string]bool, len(runs))
	for _, r := range runs {
		if r.Status == model.StatusOK && r.SnapshotID != "" {
			backed[r.SnapshotID] = true
		}
	}
	var out []UnbackedSnapshot
	for _, s := range snaps {
		if backed[s.ID] {
			continue
		}
		start, ok := named.RunStart(s)
		if !ok {
			continue
		}
		var dead []model.Run
		alive := false
		for _, r := range runs {
			if !runNamesStart(r, start) {
				continue
			}
			switch r.Status {
			case model.StatusOK, model.StatusRunning:
				// Something real, or something still going, names this second:
				// the directory is not a killed run's leftovers.
				alive = true
			case model.StatusFailed:
				dead = append(dead, r)
			}
			// `overlap` is neither: that run took the lock and did nothing, so it
			// cannot have created a directory. It is not evidence either way, and
			// if it is the only row in the window the directory is left alone.
		}
		if alive || len(dead) == 0 {
			continue
		}
		// The closest start in the window is the run that named the directory;
		// the window is small enough that this is normally the only candidate.
		culprit := dead[0]
		for _, r := range dead[1:] {
			if r.StartedAt.After(culprit.StartedAt) {
				culprit = r
			}
		}
		since := culprit.FinishedAt
		if since.IsZero() || since.Before(culprit.StartedAt) {
			since = culprit.StartedAt
		}
		if since.IsZero() {
			since = s.CreatedAt
		}
		out = append(out, UnbackedSnapshot{
			Snapshot: s,
			RunID:    culprit.ID,
			Status:   culprit.Status,
			Since:    since,
			Reason:   unbackedReason(culprit, start),
		})
	}
	return out
}

// runNamesStart reports whether r is the run that named a snapshot whose start
// is start: its started_at at or before the snapshot name, and no further behind
// it than RunStartWindow.
//
// The direction is not symmetric on purpose. The client takes started_at just
// before it posts `running` and formats the name just after, so a run that named
// this directory started at or before it. A row that started *after* the name is
// a later run and cannot have created the directory.
//
// Both sides name a second and neither keeps it to the second, so the comparison
// is made in whole seconds: a started_at of 12:00:00.9 with a directory named
// 12:00:00 is the same instant, not a run from the future.
func runNamesStart(r model.Run, start time.Time) bool {
	if r.StartedAt.IsZero() {
		return false
	}
	lag := start.Sub(r.StartedAt.UTC().Truncate(time.Second))
	return lag >= 0 && lag <= RunStartWindow
}

// unbackedReason is the one line the lifecycle logs, in the operator's terms:
// which run this was, what it ended up recorded as, and — when the server wrote
// the explanation itself — why.
func unbackedReason(r model.Run, start time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %d started %s is recorded %s",
		r.ID, start.UTC().Format(time.RFC3339), r.Status)
	if note := firstLine(r.LogTail); note != "" {
		b.WriteString(": ")
		b.WriteString(note)
	}
	return b.String()
}

// firstLine returns the first non-empty line of text, bounded to
// reasonNoteLimit bytes, flattened to one line.
func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > reasonNoteLimit {
				line = line[:reasonNoteLimit] + "…"
			}
			return line
		}
	}
	return ""
}
