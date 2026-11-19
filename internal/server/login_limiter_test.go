package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/store"
)

// These tests drive the real login handler with a chosen source address. An
// httptest.Server cannot do that — every request arrives from 127.0.0.1 — so the
// handler is served directly with RemoteAddr set, which is exactly what realIP
// reads. They use only the public HTTP surface, so they run unchanged against a
// pre-fix build; that is how the red-baseline evidence was produced.

const (
	// ipA is the address that runs up the failures; ipB is a different one.
	ipA = "192.0.2.10"
	ipB = "198.51.100.20"
)

func limiterStack(t *testing.T) (*store.Store, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	srv.dbTimeout = testDBTimeout
	return st, srv
}

// postLoginFrom sends the login form to the handler as if it came from ip.
func postLoginFrom(t *testing.T, srv *Server, ip, username, password string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{"username": {username}, "password": {password}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = ip + ":54321"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// failLogins spends n failed attempts for user from ip and asserts each one was
// answered as a wrong password rather than rate-limited — a fixture that trips
// the limiter early would make the test after it meaningless.
func failLogins(t *testing.T, srv *Server, ip, user string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		rec := postLoginFrom(t, srv, ip, user, "definitely-not-the-password")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password #%d for %q from %s = %d, want 401 (body %q)",
				i+1, user, ip, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestLoginCorrectPasswordFromAnotherAddressIsNotLockedOut: an attacker spending
// a user's budget from one address must not stop that user logging in from
// somewhere else. This is the guard against the obvious over-correction of
// keying the limiter on the username alone, which would be a global lockout.
func TestLoginCorrectPasswordFromAnotherAddressIsNotLockedOut(t *testing.T) {
	st, srv := limiterStack(t)
	setAdmin(t, st, "admin", "s3cret")

	failLogins(t, srv, ipA, "admin", loginMaxFails)

	rec := postLoginFrom(t, srv, ipB, "admin", "s3cret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("correct password for admin from %s after %d failures from %s = %d, want 303; "+
			"another address's failures must not lock this account out here (body %q)",
			ipB, loginMaxFails, ipA, rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}

// TestLoginLockoutIsScopedToTheAccount: the failures belong to (address, user).
// The same account from the same address is refused for the rest of the window —
// the budget did its job — and every other account arriving from that address
// still logs in, which is what a shared reverse-proxy address needs.
func TestLoginLockoutIsScopedToTheAccount(t *testing.T) {
	st, srv := limiterStack(t)
	setAdmin(t, st, "admin", "s3cret")
	setAdmin(t, st, "operator", "opsecret")

	failLogins(t, srv, ipA, "admin", loginMaxFails)

	rec := postLoginFrom(t, srv, ipA, "admin", "s3cret")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("correct password for admin from %s after %d of its own failures = %d, want 429 "+
			"(the account's budget from this address is spent); body %q",
			ipA, loginMaxFails, rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	rec = postLoginFrom(t, srv, ipA, "operator", "opsecret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a different account from the same address = %d, want 303; "+
			"admin's failures must not lock out everyone arriving from %s (body %q)",
			rec.Code, ipA, strings.TrimSpace(rec.Body.String()))
	}
}

// TestLoginSuccessClearsThatAccountsFailures: a correct password retires the
// failures recorded against that pair, so an attacker's earlier misses against
// an account are not carried forward by the account's real owner.
func TestLoginSuccessClearsThatAccountsFailures(t *testing.T) {
	st, srv := limiterStack(t)
	setAdmin(t, st, "admin", "s3cret")

	// Eight misses: under the budget, so the real owner can still get in.
	failLogins(t, srv, ipA, "admin", loginMaxFails-2)

	rec := postLoginFrom(t, srv, ipA, "admin", "s3cret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("correct password after %d failures = %d, want 303 (body %q)",
			loginMaxFails-2, rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	// A full budget of misses is available again: without the clear, the eight
	// above would leave only two, and the third attempt here would be a 429.
	failLogins(t, srv, ipA, "admin", loginMaxFails-1)
}

// TestLoginAddressFanOutIsStillLimited: per-pair budgets alone do not stop a
// password list sprayed across many usernames from one source, so the address
// keeps a fan-out budget of its own.
func TestLoginAddressFanOutIsStillLimited(t *testing.T) {
	st, srv := limiterStack(t)
	setAdmin(t, st, "admin", "s3cret")

	// One failure each against a long list of usernames — no single account comes
	// close to its own budget.
	for i := 0; i < loginMaxFailsPerIP; i++ {
		rec := postLoginFrom(t, srv, ipA, "user"+strconv.Itoa(i), "guess")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d of %d from %s = %d, want 401 (body %q)",
				i+1, loginMaxFailsPerIP, ipA, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}

	rec := postLoginFrom(t, srv, ipA, "admin", "s3cret")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("after %d failures spread across usernames from %s, login = %d, want 429; "+
			"one source spraying usernames must still be stopped (body %q)",
			loginMaxFailsPerIP, ipA, rec.Code, strings.TrimSpace(rec.Body.String()))
	}

	// The fan-out budget is the spraying address's, not the fleet's.
	rec = postLoginFrom(t, srv, ipB, "admin", "s3cret")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a different address = %d, want 303 (body %q)",
			rec.Code, strings.TrimSpace(rec.Body.String()))
	}
}
