package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	loginWindow = time.Minute

	// loginMaxFails is the number of failed attempts one (source address,
	// username) pair may make per loginWindow. It is the budget that belongs to
	// an account: ten wrong passwords for "admin" lock "admin" out for that
	// address, and nothing else.
	loginMaxFails = 10

	// loginMaxFailsPerIP is the number of failed attempts one source address may
	// make per loginWindow across *all* usernames. It exists because the pair
	// budget alone does not stop credential stuffing: an attacker cycling a
	// password list through a thousand usernames would get ten tries each. It is
	// deliberately far above loginMaxFails so that a handful of mistyped
	// usernames — or a fleet of operators arriving through the same reverse
	// proxy, which is the recommended deployment and shares one source address —
	// never trips it; only a real spray does. It also bounds how many pair keys
	// one source can create per window, which is what keeps the maps finite.
	loginMaxFailsPerIP = 100

	limiterGCRate = 5 * time.Minute
)

// loginLimiter tracks failed login attempts in two sliding windows:
//
//   - (source address, username): the per-account budget. Ten failures from one
//     address lock that pair out for the rest of the window, so a hostile client
//     cannot spend another account's budget and a shared proxy address alone
//     does not collapse every user into one bucket.
//   - source address: the fan-out budget across every username that address
//     tries, which is what stops credential stuffing from a single source.
//
// Only failures are counted. A successful login clears the pair's failures (the
// account proved its password from that address, so an attacker's earlier misses
// are not this user's to carry); it does not clear the address's fan-out count,
// because then an attacker with any one valid account could reset its own budget.
type loginLimiter struct {
	mu    sync.Mutex
	ips   map[string][]time.Time // per source address, all usernames
	pairs map[string][]time.Time // per (source address, username)
}

func newLoginLimiter() *loginLimiter {
	l := &loginLimiter{
		ips:   make(map[string][]time.Time),
		pairs: make(map[string][]time.Time),
	}
	go l.gc()
	return l
}

// pairKey names one (source address, username) bucket. NUL cannot appear in an
// address or a submitted username, so the two parts cannot run together.
func pairKey(ip, username string) string {
	return ip + "\x00" + username
}

// gc periodically sweeps stale entries from both maps so memory does not grow
// without bound — neither under a distributed scan with many unique attacker
// addresses nor under one address cycling usernames.
func (l *loginLimiter) gc() {
	t := time.NewTicker(limiterGCRate)
	defer t.Stop()
	for range t.C {
		l.sweep(time.Now())
	}
}

// sweep drops every bucket whose newest failure is older than the window. It is
// separate from gc so a test can assert on the result without waiting for a
// ticker: "bounded" is a property of the map after a sweep, not a stopwatch.
func (l *loginLimiter) sweep(now time.Time) {
	cutoff := now.Add(-loginWindow)
	l.mu.Lock()
	defer l.mu.Unlock()
	sweepStale(l.ips, cutoff)
	sweepStale(l.pairs, cutoff)
}

func sweepStale(m map[string][]time.Time, cutoff time.Time) {
	for k, ts := range m {
		if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
			delete(m, k)
		}
	}
}

// within prunes the bucket for key and reports whether it still has budget.
// Empty buckets are removed rather than kept as empty slices.
func within(m map[string][]time.Time, key string, limit int, now time.Time) bool {
	ts := m[key]
	cutoff := now.Add(-loginWindow)
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(m, key)
	} else {
		m[key] = ts
	}
	return len(ts) < limit
}

// allow reports whether the source address may attempt a login at all, whatever
// the username. It is the check that does not need the request body, so it runs
// before the form is parsed.
func (l *loginLimiter) allow(r *http.Request) bool {
	ip := realIP(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	return within(l.ips, ip, loginMaxFailsPerIP, time.Now())
}

// allowUser reports whether this (source address, username) pair may attempt a
// login. Call it with the username the request claims to be for — the budget is
// spent by the attempt, not by whether the account exists.
func (l *loginLimiter) allowUser(r *http.Request, username string) bool {
	ip := realIP(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	return within(l.pairs, pairKey(ip, username), loginMaxFails, time.Now())
}

// record adds a failed-attempt timestamp to both buckets. It must be called only
// for attempts that really were a wrong credential: a store that could not
// answer is a fault of the machine and must not spend the operator's budget.
func (l *loginLimiter) record(r *http.Request, username string) {
	ip := realIP(r)
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ips[ip] = append(l.ips[ip], now)
	k := pairKey(ip, username)
	l.pairs[k] = append(l.pairs[k], now)
}

// clearUser forgets the failures recorded against one (source address, username)
// pair, so a legitimate login is not punished for an attacker's earlier failures
// against the same account from the same place.
func (l *loginLimiter) clearUser(r *http.Request, username string) {
	ip := realIP(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.pairs, pairKey(ip, username))
}

// realIP returns the client address from RemoteAddr. X-Forwarded-For is not
// trusted: an attacker can spoof it to bypass the rate limiter, and honouring it
// would mean trusting a header the reverse proxy in front may not overwrite.
// Reverse proxies should be placed in front and strip/rewrite RemoteAddr at the
// TCP layer instead. The consequence, and the reason the limiter keys on the
// username as well as the address, is that behind a proxy every user arrives
// from the proxy's address and shares the per-address budget.
func realIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// No port (or a unix socket): the address is all we have. Falling back to
		// the raw value keeps every such request out of one shared "" bucket.
		return r.RemoteAddr
	}
	return host
}
