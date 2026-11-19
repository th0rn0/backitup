package server

import (
	"context"
	"strings"
	"testing"
	"time"

	_ "github.com/th0rn0/backitup/internal/mode/rsync" // registers the server-side mode, as cmd/server does
	_ "github.com/th0rn0/backitup/internal/mode/targz"
	"github.com/th0rn0/backitup/internal/model"
)

// The tests in this file pin down the difference between a verification that
// RAN and disagreed with the client (a real failure, recorded as `failed`) and
// one that COULD NOT RUN (a server fault, which must not be written into the
// backup history as a fact about the client's backup).
//
// They are deterministic by construction, in the style of store_timeout_test.go:
// the verifier is handed a context whose deadline has already passed. Waiting for
// a real store to stall and hoping it stalls in the window that matters is not a
// test, it is a coin toss — and the state a stall leaves behind is exactly a spent
// deadline.

// spentVerifierCtx is a context whose deadline is already in the past, which is
// what postStatus's storeCtx hands a handler when the store does not answer
// within Server.dbTimeout.
func spentVerifierCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(expiredDeadline))
	t.Cleanup(cancel)
	if ctx.Err() == nil {
		t.Fatal("the test's deadline is not spent, so it would not exercise the path it claims to")
	}
	return ctx
}

// TestUnrunnableVerificationDoesNotFailTheRun is the defect this file exists for.
//
// A store that stalls while POST /api/v1/status is being corroborated cannot say
// whether the client's archive is there. Recording that as `failed` turns a few
// seconds of database trouble into a durable claim that the client's backup
// broke — and a later successful run does not edit that row out, so the operator
// goes and debugs a backup that worked.
//
// The snapshot named below is deliberately NOT on disk: with a store that
// answers, this is the disagreement case and a real failure (see
// TestDisagreeingVerificationStillFails). With a store that cannot answer, it is
// nothing at all, and the run must keep the report the client made.
func TestUnrunnableVerificationDoesNotFailTheRun(t *testing.T) {
	db, srv, cl := statusStack(t, "stalled-store", model.ModeTarGz)
	logged := captureServerLog(t)
	ctx := spentVerifierCtx(t)

	const clientTail = "tar: wrote 4096 bytes, finished cleanly"
	status, tail := srv.corroboratedStatus(ctx, &cl, model.StatusOK,
		"backup-20260101T000000Z-000.tar.gz",
		time.Now().UTC().Add(-2*time.Minute), clientTail)

	if status != model.StatusOK {
		t.Fatalf("status = %q after an un-runnable verification, want the client's own %q "+
			"(a store deadline is not a failed backup); log_tail = %q", status, model.StatusOK, tail)
	}
	if !strings.Contains(tail, clientTail) {
		t.Fatalf("log_tail = %q, want the client's own report kept above the marker", tail)
	}
	if !strings.Contains(tail, unverifiedMarker) {
		t.Fatalf("log_tail = %q, want it flagged %q — an unverified run must not read as a verified one",
			tail, unverifiedMarker)
	}
	if !strings.Contains(tail, cannotVerifyPrefix) {
		t.Fatalf("log_tail = %q, want it to say the check did not run (%q) rather than that it disagreed",
			tail, cannotVerifyPrefix)
	}
	// The wording that belongs to a completed, disagreeing check must not appear:
	// this run was never compared against the store at all.
	for _, disagreement := range []string{"is not in the client's store", "no non-empty archive written since"} {
		if strings.Contains(tail, disagreement) {
			t.Fatalf("log_tail = %q states a finding (%q) the verification never made", tail, disagreement)
		}
	}

	// The cause has to be in the log with the store's own error: "#26 fixed the
	// handler half" by naming the timeout, and the history half must not swallow
	// it. The deadline that expired is named too, so the log says which clock ran
	// out rather than just "context deadline exceeded".
	for _, want := range []string{
		`client="stalled-store"`, cannotVerifyPrefix, "the store did not answer",
		"deadline of " + srv.dbTimeout.String() + " expired", "context deadline exceeded",
	} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log is missing %q; got %q", want, logged.String())
		}
	}

	// And the row the handler would write — the thing the dashboard reads — is
	// the client's report, not a failure.
	now := time.Now().UTC()
	id, err := db.RecordRun(context.Background(), model.Run{
		ClientID: cl.ID, StartedAt: now.Add(-2 * time.Minute), FinishedAt: now,
		Status: status, LogTail: tail,
	})
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	if run := storedRun(t, db, id); run.Status != model.StatusOK {
		t.Fatalf("stored status = %q for a run whose verification could not run, want %q",
			run.Status, model.StatusOK)
	}
}

// A request cancelled mid-flight (the caller hung up) is the same condition: the
// check did not run, so it says nothing about the backup.
func TestCancelledVerificationDoesNotFailTheRun(t *testing.T) {
	_, srv, cl := statusStack(t, "hung-up", model.ModeRsync)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	status, tail := srv.corroboratedStatus(ctx, &cl, model.StatusOK, "20260101T000000Z", time.Time{}, "")
	if status != model.StatusOK {
		t.Fatalf("status = %q after a cancelled verification, want %q (log_tail: %q)",
			status, model.StatusOK, tail)
	}
	if !strings.Contains(tail, cannotVerifyPrefix) || !strings.Contains(tail, "context canceled") {
		t.Fatalf("log_tail = %q, want it to name the cancellation", tail)
	}
}

// The classifier that decides "could not run" is store_timeout.go's, not a
// second copy: a spent deadline has to come out of classifyStoreFailure as a
// store deadline, otherwise the log tail would describe it as an ordinary error.
func TestUnrunnableVerificationClassifiesAsAStoreFailure(t *testing.T) {
	_, srv, cl := statusStack(t, "classifier", model.ModeTarGz)
	ctx := spentVerifierCtx(t)

	reason, err := srv.corroborateSuccess(ctx, &cl, "anything.tar.gz", time.Now().UTC())
	if err == nil {
		t.Fatalf("corroborateSuccess under a spent deadline returned (%q, nil); "+
			"it must report that it could not run", reason)
	}
	if reason != "" {
		t.Fatalf("reason = %q alongside an error, want no finding at all: an un-runnable check makes none", reason)
	}
	if got := classifyStoreFailure(ctx, err); got != storeDeadline {
		t.Fatalf("classifyStoreFailure(spent ctx, err) = %v, want %v", got, storeDeadline)
	}
}

// The behaviour that must NOT change: a verification that ran and could not find
// the claimed snapshot is a real `failed`, with the message it always had. This
// test passes on main before the fix and must keep passing.
func TestDisagreeingVerificationStillFails(t *testing.T) {
	db, srv, cl := statusStack(t, "disagrees", model.ModeTarGz)

	status, tail := srv.corroboratedStatus(context.Background(), &cl, model.StatusOK,
		"never-written.tar.gz", time.Now().UTC().Add(-time.Minute), "client says ok")

	if status != model.StatusFailed {
		t.Fatalf("status = %q for a snapshot the store does not hold, want %q (log_tail: %q)",
			status, model.StatusFailed, tail)
	}
	for _, want := range []string{
		unverifiedMarker,
		`snapshot "never-written.tar.gz" is not in the client's store`,
		"client says ok",
	} {
		if !strings.Contains(tail, want) {
			t.Fatalf("log_tail = %q, want %q", tail, want)
		}
	}
	if strings.Contains(tail, cannotVerifyPrefix) {
		t.Fatalf("log_tail = %q blames the server's ability to look for a check that ran", tail)
	}

	// The same shape through the store, so the durable row is what is asserted.
	now := time.Now().UTC()
	id, err := db.RecordRun(context.Background(), model.Run{
		ClientID: cl.ID, StartedAt: now.Add(-time.Minute), FinishedAt: now,
		Status: status, LogTail: tail,
	})
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	if run := storedRun(t, db, id); run.Status != model.StatusFailed {
		t.Fatalf("stored status = %q for a disagreeing verification, want %q", run.Status, model.StatusFailed)
	}
}

// A store that answers and agrees is still recorded as ok and is NOT flagged:
// the new "could not run" branch must not swallow the happy path.
func TestVerifiedClaimIsNotFlagged(t *testing.T) {
	_, srv, cl := statusStack(t, "verified", model.ModeTarGz)
	tarGzPath(t, srv, cl, "backup-20260101T000000Z-000.tar.gz", "streamed payload")

	status, tail := srv.corroboratedStatus(context.Background(), &cl, model.StatusOK,
		"backup-20260101T000000Z-000.tar.gz", time.Now().UTC().Add(-2*time.Minute), "")

	if status != model.StatusOK {
		t.Fatalf("status = %q for an archive that is there, want %q (log_tail: %q)", status, model.StatusOK, tail)
	}
	if strings.TrimSpace(tail) != "" {
		t.Fatalf("log_tail = %q for a corroborated run, want it untouched", tail)
	}
}
