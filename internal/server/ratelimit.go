package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	loginWindow   = time.Minute
	loginMaxFails = 10 // failed attempts per IP per minute
)

// loginLimiter tracks failed login attempts with a per-IP sliding window.
// Only failed attempts are counted; successful logins reset nothing (not needed:
// the counter drops naturally when the window expires).
type loginLimiter struct {
	mu  sync.Mutex
	ips map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{ips: make(map[string][]time.Time)}
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

// realIP extracts the client IP from the request. Checks X-Real-IP and
// X-Forwarded-For for reverse-proxy deployments; falls back to RemoteAddr.
func realIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return strings.TrimSpace(ip)
	}
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.Index(fwd, ","); i != -1 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}
