package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	loginWindow   = time.Minute
	loginMaxFails = 10 // failed attempts per IP per minute
	limiterGCRate = 5 * time.Minute
)

// loginLimiter tracks failed login attempts with a per-IP sliding window.
// Only failed attempts are counted; successful logins reset nothing (not needed:
// the counter drops naturally when the window expires).
type loginLimiter struct {
	mu  sync.Mutex
	ips map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	l := &loginLimiter{ips: make(map[string][]time.Time)}
	go l.gc()
	return l
}

// gc periodically removes stale entries from the ips map so memory doesn't
// grow unbounded under a distributed scan with many unique attacker IPs.
func (l *loginLimiter) gc() {
	t := time.NewTicker(limiterGCRate)
	defer t.Stop()
	for range t.C {
		cutoff := time.Now().Add(-loginWindow)
		l.mu.Lock()
		for ip, ts := range l.ips {
			if len(ts) == 0 || ts[len(ts)-1].Before(cutoff) {
				delete(l.ips, ip)
			}
		}
		l.mu.Unlock()
	}
}

// allow returns true if the IP is within the rate limit, false if it has
// exceeded loginMaxFails in the last loginWindow.
func (l *loginLimiter) allow(r *http.Request) bool {
	ip := realIP(r)
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-loginWindow)
	ts := l.ips[ip]

	// Prune timestamps outside the current window.
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	ts = ts[i:]

	if len(ts) >= loginMaxFails {
		l.ips[ip] = ts
		return false
	}
	return true
}

// record adds a failed-attempt timestamp for the IP.
func (l *loginLimiter) record(r *http.Request) {
	ip := realIP(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ips[ip] = append(l.ips[ip], time.Now())
}

// realIP returns the client IP from RemoteAddr. X-Forwarded-For is not trusted:
// an attacker can spoof it to bypass the rate limiter. Reverse proxies should
// be placed in front and strip/rewrite RemoteAddr at the TCP layer instead.
func realIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}
