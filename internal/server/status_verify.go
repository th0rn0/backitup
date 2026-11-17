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

// uploadSkewAllowance is how far the client's reported run start may be ahead of
// the archive's own mtime before an upload stops counting as "this run's". The
// client stamps started_at from its own clock (and the host is untrusted); the
// archive's mtime is the server's. Fifteen minutes absorbs ordinary clock drift
// without turning an archive from last week into evidence for this run.
const uploadSkewAllowance = 15 * time.Minute

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
//	           exists that was written at or after the run started, which is the
//	           upload this run claims to have made
//
// Returns ("", nil) when the claim checks out, (reason, nil) when it does not,
// and a non-nil error when the check itself could not be performed. Callers treat
// both of the last two as "not corroborated" and record a failure.
//
// Deliberately not done here: gunzipping the archive to prove it is intact. That
// would read every byte of every archive on every run report, on the same disk
// that is busy taking the next backup. Listing plus size is the cheap check;
// corruption of an archive that does exist is caught by the lifecycle's offsite
// pass, which has to read it anyway.
func (s *Server) corroborateSuccess(ctx context.Context, cl *model.Client, snapshotID string, startedAt time.Time) (string, error) {
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
	cutoff := ref.Add(-uploadSkewAllowance)
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

// corroboratedStatus decides what to record for a reported run. Only a claimed
// success is checked; failures and overlaps are the client admitting something
// went wrong, which is not a claim worth doubting. The run is not rejected — the
// wire protocol is unchanged, the client still gets its run_id — the row just
// records what the server could confirm.
func (s *Server) corroboratedStatus(ctx context.Context, cl *model.Client, st model.RunStatus, snapshotID string, startedAt time.Time, logTail string) (model.RunStatus, string) {
	if st != model.StatusOK {
		return st, logTail
	}
	reason, err := s.corroborateSuccess(ctx, cl, snapshotID, startedAt)
	if err != nil {
		reason = "check failed: " + err.Error()
	}
	if reason == "" {
		return st, logTail
	}
	log.Printf("status: client=%q claimed ok but %s: %s", cl.Name, unverifiedMarker, reason)
	return model.StatusFailed, markUnverified(logTail, reason)
}
