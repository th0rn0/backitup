package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func loginReq(ip string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/login", nil)
	r.RemoteAddr = ip + ":12345"
	return r
}

// bucketCounts reports how many buckets each dimension holds, under the lock.
func bucketCounts(l *loginLimiter) (ips, pairs int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ips), len(l.pairs)
}

// TestLoginLimiterPairBudget: ten failures for one (address, user) pair lock that
// pair out, and neither the account from another address nor another account from
// the same address is affected.
func TestLoginLimiterPairBudget(t *testing.T) {
	l := newLoginLimiter()
	a, b := loginReq(ipA), loginReq(ipB)

	for i := 0; i < loginMaxFails; i++ {
		if !l.allow(a) || !l.allowUser(a, "admin") {
			t.Fatalf("refused before the limit at attempt %d", i+1)
		}
		l.record(a, "admin")
	}
	if l.allowUser(a, "admin") {
		t.Fatalf("allowUser returned true after %d failures; expected the pair to be limited", loginMaxFails)
	}
	if !l.allowUser(a, "operator") {
		t.Fatal("another account from the same address was limited by admin's failures")
	}
	if !l.allowUser(b, "admin") {
		t.Fatal("the same account from another address was limited by this address's failures")
	}
}

// TestLoginLimiterFanOutBudget: a source that spreads one failure across many
// usernames is still stopped, and only that source is.
func TestLoginLimiterFanOutBudget(t *testing.T) {
	l := newLoginLimiter()
	a, b := loginReq(ipA), loginReq(ipB)

	for i := 0; i < loginMaxFailsPerIP; i++ {
		if !l.allow(a) {
			t.Fatalf("address refused before the fan-out limit at attempt %d of %d", i+1, loginMaxFailsPerIP)
		}
		l.record(a, "user"+strconv.Itoa(i))
	}
	if l.allow(a) {
		t.Fatalf("allow returned true after %d failures spread across usernames", loginMaxFailsPerIP)
	}
	if !l.allow(b) {
		t.Fatal("one address's fan-out budget was charged to another address")
	}
}

// TestLoginLimiterSuccessClearsOnlyThatPair: clearing on success must forget the
// pair's failures and leave the address's fan-out count standing — otherwise a
// sprayer holding one valid account could reset its own budget by logging in.
func TestLoginLimiterSuccessClearsOnlyThatPair(t *testing.T) {
	l := newLoginLimiter()
	a := loginReq(ipA)

	for i := 0; i < loginMaxFails-1; i++ {
		l.record(a, "admin")
	}
	if ips, pairs := bucketCounts(l); ips != 1 || pairs != 1 {
		t.Fatalf("buckets before clear = %d ip, %d pair; want 1, 1", ips, pairs)
	}

	l.clearUser(a, "admin")
	if _, pairs := bucketCounts(l); pairs != 0 {
		t.Fatalf("%d pair bucket(s) survived the clear; want 0", pairs)
	}
	if ips, _ := bucketCounts(l); ips != 1 {
		t.Fatalf("%d ip bucket(s) after the clear; want the address's count kept", ips)
	}
}

// TestLoginLimiterSweepBoundsCycledUsernames: an attacker cycling usernames must
// not grow the maps without bound. The bound is asserted after an explicit sweep,
// never by waiting for the gc ticker: the property is "stale keys are dropped",
// not "the sweep happens within five minutes".
func TestLoginLimiterSweepBoundsCycledUsernames(t *testing.T) {
	l := newLoginLimiter()
	a := loginReq(ipA)

	// Through the handler's path, the address's fan-out budget caps how many pair
	// keys one source can create per window — this is the real bound in service.
	created := 0
	for i := 0; i < 10*loginMaxFailsPerIP; i++ {
		if !l.allow(a) {
			break
		}
		l.record(a, "cycled"+strconv.Itoa(i))
		created++
	}
	if created != loginMaxFailsPerIP {
		t.Fatalf("one address created %d pair keys before its fan-out budget stopped it; want %d",
			created, loginMaxFailsPerIP)
	}
	if _, pairs := bucketCounts(l); pairs > loginMaxFailsPerIP {
		t.Fatalf("%d pair keys tracked for one address; the fan-out budget must bound it to %d",
			pairs, loginMaxFailsPerIP)
	}

	// Even bypassing the budget (a direct record, e.g. a fleet of addresses
	// cycling names), a sweep drops every bucket whose window has passed.
	for i := 0; i < 500; i++ {
		l.record(loginReq("203.0.113."+strconv.Itoa(i%250)), "cycled"+strconv.Itoa(i))
	}
	if ips, pairs := bucketCounts(l); pairs < 500 || ips < 250 {
		t.Fatalf("fixture did not grow the maps: %d ip, %d pair", ips, pairs)
	}

	l.sweep(time.Now().Add(loginWindow + time.Second))
	if ips, pairs := bucketCounts(l); ips != 0 || pairs != 0 {
		t.Fatalf("after a sweep past the window: %d ip, %d pair buckets remain; want 0, 0", ips, pairs)
	}
}

// TestRealIPDoesNotTrustForwardedFor: the limiter's whole budget depends on this
// address being the one the connection came from. A spoofable header would hand
// the attacker a fresh bucket per request.
func TestRealIPDoesNotTrustForwardedFor(t *testing.T) {
	r := loginReq(ipA)
	r.Header.Set("X-Forwarded-For", "203.0.113.99")
	r.Header.Set("X-Real-IP", "203.0.113.98")
	if got := realIP(r); got != ipA {
		t.Fatalf("realIP = %q with X-Forwarded-For set, want %q (RemoteAddr)", got, ipA)
	}

	// No port, and no header at all: the address is still the connection's.
	r = &http.Request{RemoteAddr: "203.0.113.7"}
	if got := realIP(r); got != "203.0.113.7" {
		t.Fatalf("realIP of a portless RemoteAddr = %q, want 203.0.113.7", got)
	}
}
