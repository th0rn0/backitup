package alert

// Discord() is the function that tells an operator a backup failed, and it had
// no test at all. Its contract is narrow and silent, which is exactly why it
// needs asserting: it returns nothing, so every failure mode has to be read off
// the log — the thing a human sees when a webhook stops working.
//
// Nothing here reaches the network: the real cases go through httptest servers
// on 127.0.0.1, and the one that must NOT make a request swaps httpClient's
// transport for one that fails the test if it is ever called.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// captureLog redirects the standard logger for the duration of one test and
// returns the buffer it writes into.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// swapClient replaces the package's http client for one test. The timeout on the
// production client is what a request to a dead webhook hangs on, so a test that
// needs a canned transport must not silently drop it: the replacement keeps it.
func swapClient(t *testing.T, c *http.Client) {
	t.Helper()
	prev := httpClient
	httpClient = c
	t.Cleanup(func() { httpClient = prev })
}

// A webhook that answers 204 is the success path: the body must be the JSON
// Discord accepts, with the message intact, and the call must say nothing.
func TestDiscordPostsTheMessageAndIsSilentOnSuccess(t *testing.T) {
	var got struct {
		method string
		ctype  string
		body   map[string]string
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.ctype = r.Header.Get("Content-Type")
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if err := json.Unmarshal(raw, &got.body); err != nil {
			t.Errorf("body is not JSON (%q): %v", raw, err)
		}
		w.WriteHeader(http.StatusNoContent) // what Discord answers for an accepted post
	}))
	defer ts.Close()

	logs := captureLog(t)
	Discord(ts.URL, "backup FAILED on vm-host: rsync exit 23")

	if got.method != http.MethodPost {
		t.Errorf("method = %q, want POST", got.method)
	}
	if got.ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.ctype)
	}
	if want := "backup FAILED on vm-host: rsync exit 23"; got.body["content"] != want {
		t.Errorf("posted content = %q, want %q", got.body["content"], want)
	}
	if s := logs.String(); s != "" {
		t.Errorf("a 204 was logged as a failure: %q", s)
	}
}

// A non-2xx is the case an operator needs to know about: the alert did not
// arrive. Discord rejects a malformed or rotated webhook with 400/404, so the
// status has to reach the log — silently discarding it means a backup failure
// nobody is ever told about.
func TestDiscordLogsANon2xxStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))

		logs := captureLog(t)
		Discord(ts.URL, "backup FAILED")
		s := logs.String()
		ts.Close()

		if !strings.Contains(s, "alert: discord:") {
			t.Errorf("status %d: nothing was logged: %q", status, s)
		}
		if !strings.Contains(s, strconv.Itoa(status)) {
			t.Errorf("status %d: the status is not in the log line %q; without it the operator cannot tell a dead webhook from a refused one", status, s)
		}
	}
}

// A transport failure (DNS, refused connection, timeout) must be reported and
// must not panic: this runs on the lifecycle's goroutine after a backup already
// failed, where a panic would take down the server.
func TestDiscordLogsATransportFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := ts.URL
	ts.Close() // every connection to this address is now refused

	logs := captureLog(t)
	Discord(url, "backup FAILED") // must return, not panic or hang

	s := logs.String()
	if !strings.Contains(s, "alert: discord:") {
		t.Fatalf("a refused connection was not logged: %q", s)
	}
	if !strings.Contains(s, url) {
		t.Errorf("the log line does not name the webhook that failed (%q); the operator runs several", s)
	}
}

// An empty URL is the "no webhook configured" case and must be a true no-op: no
// request, no log line. Sending to "" would log an error on every failed backup
// for every operator who has not configured Discord.
func TestDiscordWithNoURLMakesNoRequest(t *testing.T) {
	called := false
	swapClient(t, &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
	})})

	logs := captureLog(t)
	Discord("", "backup FAILED")

	if called {
		t.Error("an empty webhook URL still made a request")
	}
	if s := logs.String(); s != "" {
		t.Errorf("an unconfigured webhook logged %q; that is one line per failed backup for every operator without Discord", s)
	}
}

// roundTripFunc lets a test answer without a server.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// FormatBytes is what the alert (and the dashboard) renders a size with. The
// boundaries are the bug: a 1 GiB snapshot shown as "1024.00 MB" is not wrong
// enough to notice in a review, and a KB branch that truncates hides a partial
// upload.
func TestFormatBytes(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 KB"},
		{1023, "0 KB"},                // below one KiB: still reported in KB
		{1024, "1 KB"},                // exactly one KiB
		{1<<20 - 1, "1023 KB"},        // just under a MiB stays in KB
		{1 << 20, "1.00 MB"},          // exactly one MiB
		{1<<20 + 512*1024, "1.50 MB"}, // the fraction is shown, not truncated
		{1 << 30, "1.00 GB"},          // exactly one GiB
		{3<<30 + 512<<20, "3.50 GB"},  // and above
	}
	for _, tc := range cases {
		if got := FormatBytes(tc.bytes); got != tc.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}
