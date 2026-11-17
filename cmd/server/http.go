package main

import (
	"net/http"
	"time"
)

// HTTP server timeouts (sweep S4). The control plane previously served with
// http.ListenAndServe / ListenAndServeTLS, i.e. the zero-value http.Server and
// therefore no deadlines at all: a client that opened a socket and never
// finished its request headers was kept open indefinitely (measured: still open
// after 15 s).
//
// Chosen values and why:
//
//   - ReadHeaderTimeout = 10s — bounds the time allowed to send the request
//     line + headers. This is the slowloris knob: 10s is generous for any real
//     client on a slow link (headers are a few hundred bytes) and cheap to hold
//     per connection, while a half-open header stream is dropped at 10s.
//   - ReadTimeout = 60s — bounds the whole request read including the body.
//     Every request body on this surface is a small HTML form or JSON document
//     (no upload path: snapshots arrive over SSH, offsite traffic is outbound),
//     so 60s is already an order of magnitude more than any legitimate client
//     needs. It also stops a slow *body* from pinning a connection the way
//     slowloris pins a header read.
//   - IdleTimeout = 120s — keep-alive reuse for the web UI (a browser tab left
//     open keeps a warm connection) without letting abandoned sockets pile up.
//   - WriteTimeout = 0, i.e. NO write deadline — deliberate, and the reason this
//     server is constructed by hand. The snapshot and offsite download handlers
//     stream whole archives through the response (internal/server/snapshots.go:
//     http.ServeFile for tar.gz snapshots, tarGzDirectory / rclone cat for
//     rsync snapshots and offsite objects) under a 30-minute request context.
//     Any finite WriteTimeout shorter than the slowest legitimate download would
//     cut the stream mid-transfer and hand the user a truncated archive that
//     looks like a successful download. Download duration is instead bounded by
//     the handler-side context (30 min) and by the client going away, while the
//     request side stays bounded by ReadHeaderTimeout/ReadTimeout above.
//     Verified by TestStreamingDownloadNotTruncated in http_test.go.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	idleTimeout       = 120 * time.Second
	writeTimeout      = 0 // unbounded: snapshot/offsite downloads stream for minutes
)

// newHTTPServer builds the control-plane listener with explicit timeouts. Both
// the plaintext and the TLS listener are served by this one constructor so the
// two paths cannot drift apart, and so the timeout set is reachable from tests.
//
// addr goes into Server.Addr because *http.Server.ListenAndServe serves
// s.Addr, not an argument: an empty Addr silently falls back to ":http" (port
// 80) and ignores BACKITUP_ADDR.
func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
		WriteTimeout:      writeTimeout,
	}
}
