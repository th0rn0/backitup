package server

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// unverifiedMarker is appended to a run's log_tail when the server could not
// corroborate a client's claimed success. It is part of what the operator sees
// on the run log, so the wording is deliberate.
const unverifiedMarker = "unverified by server"

// cannotVerifyPrefix explains, in the run log, that the CHECK did not run — as
// opposed to running and disagreeing with the client. The distinction is the
// whole point: a store that stalled for a few seconds is a fact about the
// server, and recording it as a `failed` run writes it into the backup history
// as a fact about the client's backup, where no later success edits it out.
const cannotVerifyPrefix = "verification could not run"

// uploadSkewAllowance is how far the client's reported run start may be ahead of
// the archive's own mtime before an upload stops counting as "this run's". The
// client stamps started_at from its own clock (and the host is untrusted); the
// archive's mtime is the server's. Fifteen minutes absorbs ordinary clock drift
// without turning an archive from last week into evidence for this run.
const uploadSkewAllowance = 15 * time.Minute

// uploadMaxLag is the other end of the same window, and it is measured on the
// SERVER's clock. started_at - skew alone is not a bound: started_at comes from
// the untrusted host, so a backdated start reaches back to any archive the
// client's store already holds, which is the claim this check exists to refuse.
// The archive's mtime is written when backitup-recv finishes — at the END of the
// run — so a slow backup is not penalised by a tight window: the cutoff is at
// least maxLag before the report arrives.
const uploadMaxLag = 2 * time.Hour

// corroborateSuccess checks a client's claim that a backup succeeded against what
// the server can see for itself. The bearer token proves WHICH client is talking;
// it proves nothing about whether a backup ran. The host is untrusted by design,
// so an "ok" is only worth recording when there is really something on the hot
// store behind it:
//
//	rsync      the reported snapshot directory is listed under <clientDir>/snapshots
//	tar.gz     a reported archive is listed AND is a non-empty regular file
//	tar.gz (no snapshot_id reported, which is what the bundled client does —
//	           backitup-recv names the archive server-side) a non-empty archive
//	           exists that was written during this run, where the window is
//	           bounded at BOTH ends: no earlier than the client's reported start
//	           (minus a skew allowance) and no earlier than maxLag before the
//	           report, so a backdated start cannot reach back to an old archive
//
// Returns ("", nil) when the claim checks out, (reason, nil) when it does not
// — a completed verification that disagrees — and a non-nil error when the
// check itself could not be performed. The two are NOT the same thing and
// callers must not treat them the same: only the first two say anything about
// the client's backup, while an error says only that the server could not look.
//
// Deliberately not done here: gunzipping the archive to prove it is intact. That
// would read every byte of every archive on every run report, on the same disk
// that is busy taking the next backup. Listing plus size is the cheap check;
// corruption of an archive that does exist is caught by the lifecycle's offsite
// pass, which has to read it anyway.
//
// Known limits, accepted rather than fixed here. (1) A client that names one of
// its OWN existing snapshots and inflates bytes still reads ok: this checks that
// something real stands behind the claim, not that it is fresh — tightening the
// id path to a freshness window would need proof that every real rsync/tar.gz
// shape still lands inside it. (2) finished_at is still client-supplied and is
// the timestamp health ages from, so a future-dated finished_at holds a client
// at "ok"; that predates this check and is tracked separately.
func (s *Server) corroborateSuccess(ctx context.Context, cl *model.Client, snapshotID string, startedAt time.Time) (string, error) {
	// A check that cannot run must fail as "could not run", never as a verdict.
	// postStatus passes the request's store context here, so a spent store
	// deadline already means the store is not answering; the listing below does
	// not itself look at the context, so without this it would go on to read the
	// directory and produce an answer nobody is waiting for — and an empty
	// listing reads exactly like "the client's snapshot is not there", which is a
	// statement about the client's backup that this request is in no position to
	// make.
	if err := ctx.Err(); err != nil {
		return "", err
	}
	sm, ok := mode.Server(cl.Mode)
	if !ok {
		// Cannot verify without the mode's idea of a snapshot. Fail closed and
		// say so: an unverifiable "ok" is not an "ok".
		return fmt.Sprintf("no server-side mode registered for mode %q", cl.Mode), nil
	}
	dir, err := s.clientDir(*cl)
	if err != nil {
		return "", err
	}
	snaps, err := sm.List(ctx, dir)
	if err != nil {
		return "", err
	}
	// The listing finished, but after the deadline the request had for it: the
	// store was stalled while the answer was produced, so the answer is not one
	// this run can be judged on. Saying "not in the store" here would be the
	// database's stall written down as the client's missing backup.
	if err := ctx.Err(); err != nil {
		return "", err
	}

	if snapshotID != "" {
		// Match the reported ID against the listed ones and then use the LISTED
		// value: the reported string is untrusted input and must never become a
		// path component on its own ("../other-client/x.tar.gz" matches nothing).
		listed := ""
		for _, snap := range snaps {
			if snap.ID == snapshotID {
				listed = snap.ID
				break
			}
		}
		if listed == "" {
			return fmt.Sprintf("snapshot %q is not in the client's store", snapshotID), nil
		}
		if cl.Mode != model.ModeTarGz {
			return "", nil // rsync: a listed snapshot directory is the whole claim
		}
		info, err := os.Stat(filepath.Join(dir, listed))
		if err != nil {
			return fmt.Sprintf("snapshot %q is listed but unreadable: %v", listed, err), nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Sprintf("snapshot %q is not a regular file", listed), nil
		}
		if info.Size() == 0 {
			return fmt.Sprintf("snapshot %q is empty", listed), nil
		}
		return "", nil
	}

	// No snapshot_id: only tar.gz can legitimately be here. The client streams its
	// archive to backitup-recv, which names the file, so the client has no name to
	// report (mode.BackupResult.SnapshotID is empty for targz). The claim that a
	// backup happened is then corroborated by the upload itself: a non-empty
	// archive written at or after the run started.
	if cl.Mode != model.ModeTarGz {
		return "no snapshot_id reported", nil
	}
	ref := startedAt
	if ref.IsZero() || ref.After(time.Now().UTC()) {
		// A missing start (the run_id update path need not resend it) or one the
		// client's clock placed in the future: judge the upload by "now".
		ref = time.Now().UTC()
	}
	// The client's clock may only NARROW this window, never widen it past what
	// the server itself observed: without the floor below, started_at
	// "2020-01-01" makes every archive already on disk look like this run's
	// upload, which is the fabrication this check exists to catch.
	now := time.Now().UTC()
	cutoff := ref.Add(-uploadSkewAllowance)
	if floor := now.Add(-uploadMaxLag); cutoff.Before(floor) {
		cutoff = floor
	}
	for _, snap := range snaps {
		if snap.Bytes == 0 || snap.CreatedAt.IsZero() {
			continue // empty archives and undated entries are not evidence
		}
		if snap.CreatedAt.Before(cutoff) {
			continue
		}
		return "", nil
	}
	return fmt.Sprintf("no non-empty archive written since %s (run start %s)",
		cutoff.Format(time.RFC3339), ref.Format(time.RFC3339)), nil
}

// markUnverified appends the unverified marker and its reason to a run's log
// tail, keeping whatever the client reported above it (the tail is capped at
// MaxLogTail on write, and the marker is last, so it is what survives).
func markUnverified(logTail, reason string) string {
	line := unverifiedMarker + ": " + reason
	if strings.TrimSpace(logTail) == "" {
		return line
	}
	return logTail + "\n" + line
}

// unverifiedReason explains, for the run log, why the verification did not run.
// The machine condition is named when the classifier recognises one, and the
// underlying error is kept verbatim after it: a flattened "unverified" is what
// sends an operator looking for a bug in their backup client.
func (s *Server) unverifiedReason(ctx context.Context, err error) string {
	switch classifyStoreFailure(ctx, err) {
	case storeDeadline:
		return cannotVerifyPrefix + ": the store did not answer (" + s.deadlineReason(ctx) + "): " + err.Error()
	case storeLocked:
		return cannotVerifyPrefix + ": the store is busy with another write: " + err.Error()
	default:
		return cannotVerifyPrefix + ": " + err.Error()
	}
}

// corroboratedStatus decides what to record for a reported run. Only a claimed
// success is checked; failures and overlaps are the client admitting something
// went wrong, which is not a claim worth doubting. The run is not rejected — the
// wire protocol is unchanged, the client still gets its run_id — the row just
// records what the server could confirm.
//
// "Could not verify" and "verified and it disagrees" are different outcomes and
// are recorded differently:
//
//   - the check ran and disagrees: `failed`, with the reason appended. The
//     client claimed a backup the store cannot see, which is a fact about the
//     client's run.
//   - the check could not run (a store deadline or a stalled or locked store —
//     see classifyStoreFailure): the client's own report is kept and flagged
//     `unverified by server: verification could not run: <why>` in its log tail.
//     The failure was the server's; writing `failed` here fabricated a durable
//     fact about a backup that the operator would then debug.
//
// There is no third status value to use for the second case: runs.status is
// CHECK-constrained to ok/failed/overlap/running and the dashboard, health
// derivation and alerts all read status, so an "unknown" would be a schema
// migration and a new health state, not a label. The log tail is the honest
// place for "the server could not look", and it survives on the row.
func (s *Server) corroboratedStatus(ctx context.Context, cl *model.Client, st model.RunStatus, snapshotID string, startedAt time.Time, logTail string) (model.RunStatus, string) {
	if st != model.StatusOK {
		return st, logTail
	}
	reason, err := s.corroborateSuccess(ctx, cl, snapshotID, startedAt)
	if err != nil {
		// The verification did not run. Leave the run the client reported, and
		// say on the row — and in the log, with the error — that it stands
		// unverified rather than disproved.
		why := s.unverifiedReason(ctx, err)
		log.Printf("status: client=%q claimed ok; %s", cl.Name, why)
		return st, markUnverified(logTail, why)
	}
	if reason == "" {
		return st, logTail
	}
	log.Printf("status: client=%q claimed ok but %s: %s", cl.Name, unverifiedMarker, reason)
	return model.StatusFailed, markUnverified(logTail, reason)
}
