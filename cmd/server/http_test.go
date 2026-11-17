package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// startHTTP serves hs on a loopback listener for the duration of the test and
// returns the address to dial.
func startHTTP(t *testing.T, hs *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = hs.Serve(ln) }()
	t.Cleanup(func() { _ = hs.Close() })
	return ln.Addr().String()
}

// TestHTTPServerHasExplicitTimeouts guards the timeout set: the control plane
// must never go back to the zero-value server (no deadlines at all).
func TestHTTPServerHasExplicitTimeouts(t *testing.T) {
	t.Parallel()
	hs := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	t.Logf("configured: Addr=%q ReadHeaderTimeout=%s ReadTimeout=%s IdleTimeout=%s WriteTimeout=%s",
		hs.Addr, hs.ReadHeaderTimeout, hs.ReadTimeout, hs.IdleTimeout, hs.WriteTimeout)

	// *http.Server.ListenAndServe serves s.Addr; an empty Addr silently binds
	// ":http" (port 80) and ignores BACKITUP_ADDR (caught by running the binary
	// on a non-default port).
	if hs.Addr != "127.0.0.1:0" {
		t.Errorf("Addr = %q; want the address passed in, so ListenAndServe honors BACKITUP_ADDR", hs.Addr)
	}

	if hs.ReadHeaderTimeout <= 0 || hs.ReadHeaderTimeout > 30*time.Second {
		t.Errorf("ReadHeaderTimeout = %s; want a small positive bound (slowloris guard)", hs.ReadHeaderTimeout)
	}
	if hs.ReadTimeout <= 0 || hs.ReadTimeout < hs.ReadHeaderTimeout {
		t.Errorf("ReadTimeout = %s; want positive and >= ReadHeaderTimeout (%s)", hs.ReadTimeout, hs.ReadHeaderTimeout)
	}
	if hs.IdleTimeout <= 0 {
		t.Errorf("IdleTimeout = %s; want positive so idle keep-alive sockets are reaped", hs.IdleTimeout)
	}
	// Deliberate: the snapshot/offsite download handlers stream archives for
	// minutes under a 30-minute context, so a write deadline would truncate
	// legitimate downloads. See http.go and TestStreamingDownloadNotTruncated.
	if hs.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %s; want 0 (unbounded) — a finite value truncates streaming downloads", hs.WriteTimeout)
	}
}

// TestSlowlorisIncompleteHeaderClosed proves the ReadHeaderTimeout is really
// applied to a socket: send a request line and headers but never the terminating
// blank line, and require the server to disconnect at ~ReadHeaderTimeout.
// Before this change (plain http.ListenAndServe) the connection was still open
// after 15 s, which is the defect S4 describes.
func TestSlowlorisIncompleteHeaderClosed(t *testing.T) {
	t.Parallel()
	addr := startHTTP(t, newHTTPServer("127.0.0.1:0", http.NewServeMux()))

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Request line + two headers, deliberately no final CRLF: the header block
	// never completes, which is exactly what slowloris does.
	const partial = "GET /login HTTP/1.1\r\nHost: slowloris-probe\r\nUser-Agent: probe\r\n"
	if _, err := io.WriteString(conn, partial); err != nil {
		t.Fatalf("write partial header: %v", err)
	}

	start := time.Now()
	_ = conn.SetReadDeadline(time.Now().Add(readHeaderTimeout + 30*time.Second))
	var received int
	var readErr error
	buf := make([]byte, 512)
	for {
		n, err := conn.Read(buf)
		received += n
		if err != nil {
			readErr = err
			break
		}
	}
	elapsed := time.Since(start)
	t.Logf("incomplete header block: connection closed after %s (ReadHeaderTimeout=%s, %d bytes from server before close, read err=%v)",
		elapsed.Round(10*time.Millisecond), readHeaderTimeout, received, readErr)

	if elapsed > readHeaderTimeout+5*time.Second {
		t.Fatalf("connection still open after %s with an incomplete header block; ReadHeaderTimeout=%s is not applied",
			elapsed.Round(time.Millisecond), readHeaderTimeout)
	}
	if elapsed < readHeaderTimeout-time.Second {
		t.Fatalf("connection closed after %s, well before ReadHeaderTimeout=%s — something other than the header timeout closed it",
			elapsed.Round(time.Millisecond), readHeaderTimeout)
	}
}

// writePatternFile creates a deterministic file of size bytes and returns its
// path, size and sha256 so a transfer can be checked end to end.
func writePatternFile(t *testing.T, size int) (path, sum string) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "snapshot.tar.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create payload: %v", err)
	}
	defer f.Close()
	h := sha256.New()
	chunk := make([]byte, 256<<10)
	for i := range chunk {
		chunk[i] = byte(i * 7)
	}
	for written := 0; written < size; written += len(chunk) {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("write payload: %v", err)
		}
		h.Write(chunk)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close payload: %v", err)
	}
	return path, hex.EncodeToString(h.Sum(nil))
}

// downloadSlowly GETs addr and reads the body like a slow client on a thin
// link: chunkSize bytes, then a pause. Returns the byte count, the body sha256
// and the first read error.
func downloadSlowly(t *testing.T, addr string, pause time.Duration) (int64, string, error) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Minute}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	h := sha256.New()
	buf := make([]byte, 256<<10)
	var total int64
	var readErr error
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
		}
		if err != nil {
			if err != io.EOF {
				readErr = err
			}
			break
		}
		time.Sleep(pause)
	}
	return total, hex.EncodeToString(h.Sum(nil)), readErr
}

// serveFileHandler serves path the same way the snapshot download handler does
// for tar.gz snapshots: http.ServeContent (what http.ServeFile uses) on a
// *os.File, i.e. a real streaming sendfile-style body.
func serveFileHandler(path string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, err := os.Open(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", `attachment; filename="snapshot.tar.gz"`)
		http.ServeContent(w, r, "snapshot.tar.gz", fi.ModTime(), f)
	})
}

// TestStreamingDownloadNotTruncated streams a large archive through the
// configured server with a deliberately slow reader. The transfer takes several
// seconds — longer than the finite WriteTimeout a naive fix would set — and must
// arrive complete. This is the reason WriteTimeout stays 0 in http.go.
func TestStreamingDownloadNotTruncated(t *testing.T) {
	t.Parallel()
	const size = 32 << 20 // 32 MiB
	path, wantSum := writePatternFile(t, size)

	addr := startHTTP(t, newHTTPServer("127.0.0.1:0", serveFileHandler(path)))
	start := time.Now()
	total, sum, readErr := downloadSlowly(t, addr, 70*time.Millisecond)
	elapsed := time.Since(start)
	t.Logf("streamed %d of %d bytes in %s (sha256 %s, read err=%v)",
		total, size, elapsed.Round(10*time.Millisecond), sum[:16], readErr)

	if readErr != nil {
		t.Fatalf("stream read failed after %d bytes: %v", total, readErr)
	}
	if total != size {
		t.Fatalf("received %d bytes, want %d — response was truncated", total, size)
	}
	if sum != wantSum {
		t.Fatalf("body sha256 = %s, want %s", sum, wantSum)
	}
	if elapsed < 5*time.Second {
		t.Fatalf("transfer finished in %s; the test must span more than a short WriteTimeout to prove anything",
			elapsed.Round(time.Millisecond))
	}
}

// TestFiniteWriteTimeoutTruncates is the sensitivity check for the test above:
// the same handler and reader with a finite WriteTimeout (1s) is cut mid-stream.
// It documents why writeTimeout stays 0 rather than a "safe-looking" number.
func TestFiniteWriteTimeoutTruncates(t *testing.T) {
	t.Parallel()
	const size = 32 << 20 // 32 MiB — larger than loopback socket buffers, so writes block on the slow reader
	path, wantSum := writePatternFile(t, size)

	// Deliberate deviation from newHTTPServer: the naive fix being guarded against.
	hs := &http.Server{Handler: serveFileHandler(path), WriteTimeout: time.Second}
	addr := startHTTP(t, hs)

	total, sum, readErr := downloadSlowly(t, addr, 70*time.Millisecond)
	t.Logf("finite WriteTimeout=1s: received %d of %d bytes (sha256 %s, read err=%v)",
		total, size, sum[:16], readErr)

	if total == size && sum == wantSum && readErr == nil {
		t.Fatalf("a finite WriteTimeout=1s did NOT truncate an %d-byte slow transfer — this sensitivity check is broken", size)
	}
}
