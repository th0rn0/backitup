package server

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// defaultDBTimeout bounds the store work one request may do. It is a safety
// bound against a wedged store, not a latency target: on an idle machine a
// query against the local SQLite file takes about a millisecond, so a request
// that reaches this bound is not slow, it is stuck.
//
// It is a field on Server rather than a constant in each handler so a test can
// make it expire on purpose. That is the only way to exercise the timeout path
// deterministically: waiting five seconds and hoping the machine stalls is not
// a test, it is a coin toss.
const defaultDBTimeout = 5 * time.Second

// storeTimeoutBody is shown when the store did not answer. It deliberately does
// not blame the request: the same handler used to answer 400 "Could not create
// client (is the name already taken?)" for a spent deadline, which sends the
// operator after their input instead of after the server.
const storeTimeoutBody = "The database did not respond in time — the server is under load. Try again in a moment."

// storeLockedBody is shown when SQLite gave up waiting for another writer.
const storeLockedBody = "The database is busy with another write. Try again in a moment."

// storeCtx is the context a handler's store calls run under. Handlers must not
// pass r.Context() to the store directly: the deadline is what turns a wedged
// store into a response instead of a hung request, and it has to be one knob.
func (s *Server) storeCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), s.dbTimeout)
}

// storeTimeoutKind names why the store could not answer.
type storeTimeoutKind int

const (
	storeOK       storeTimeoutKind = iota // not an availability failure
	storeDeadline                         // s.dbTimeout elapsed, or the request was cancelled
	storeLocked                           // SQLite reported the database as locked
)

// classifyStoreFailure reports whether err means "the store could not answer
// right now" rather than "the call was wrong":
//
//   - our own deadline expired, in the error or in ctx — a timeout is not a
//     statement about the request, because the store never got to answer it;
//   - the request context was cancelled (the caller hung up mid-request);
//   - SQLite gave up waiting for another writer and reported the database as
//     locked.
//
// All three are conditions of the machine. Reporting them as 400 "invalid form"
// or 500 "rotate failed" is how two CI builds (163, 165) were spent looking for
// a bug in a handler that had actually just run out of clock.
func classifyStoreFailure(ctx context.Context, err error) storeTimeoutKind {
	if err == nil {
		return storeOK
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return storeDeadline
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return storeDeadline
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") {
		return storeLocked
	}
	return storeOK
}

// storeFailure logs a failed store call and, when the store is the thing that
// failed, answers 503 + Retry-After and reports true so the caller stops:
//
//	if s.storeFailure(w, ctx, "rotate client "+c.Name, err) {
//		return
//	}
//	http.Error(w, "rotate failed", http.StatusInternalServerError)
//
// The error is logged in every case, including the ones the caller reports
// itself, because an unexplained 500 in a step log is what started this
// investigation: build 163's `rotate status = 500` had no matching server log
// line at all, so the cause had to be inferred from timings.
//
// op names the operation precisely — include the client or user, not just the
// verb. It is only interpolated on a failure path.
func (s *Server) storeFailure(w http.ResponseWriter, ctx context.Context, op string, err error) bool {
	switch classifyStoreFailure(ctx, err) {
	case storeDeadline:
		log.Printf("%s: store did not answer (%s): %v", op, s.deadlineReason(ctx), err)
		w.Header().Set("Retry-After", "1")
		http.Error(w, storeTimeoutBody, http.StatusServiceUnavailable)
		return true
	case storeLocked:
		log.Printf("%s: store locked: %v", op, err)
		w.Header().Set("Retry-After", "1")
		http.Error(w, storeLockedBody, http.StatusServiceUnavailable)
		return true
	default:
		log.Printf("%s: %v", op, err)
		return false
	}
}

// deadlineReason describes which clock ran out, for the log line.
func (s *Server) deadlineReason(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "request cancelled before the store answered"
	}
	return "deadline of " + s.dbTimeout.String() + " expired"
}
