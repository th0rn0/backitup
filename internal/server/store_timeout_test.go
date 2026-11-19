package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// The tests in this file pin down what a handler does when the store does not
// answer within Server.dbTimeout.
//
// Why they exist: builds 163 and 165 went red on a `test` step while the
// identical commit's paired PR build was green, with
//
//	lane_a_test.go:203: rotate status = 500, want 200
//	lane_a_test.go:55:  create status = 400, want 200
//
// Both statuses were produced by handlers that had run out of their five-second
// store deadline and then reported it as something else — a failed rotation, or
// "Could not create client (is the name already taken?)". Nothing was logged, so
// the only way to find out was to measure test durations (7.37s and 9.61s
// against a normal 0.3-0.8s) and guess.
//
// They are deterministic by construction: the deadline is a field, so a test can
// hand the handler one that is already spent. Reproducing the runner's load
// instead would be a coin toss.

// storeTimeoutStack builds a server whose store deadline is d, already wired, so
// the field is written before any handler goroutine can read it.
func storeTimeoutStack(t *testing.T, d time.Duration) (*store.Store, *Server) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(st, false)
	tmp := t.TempDir()
	srv.authKeysPath = filepath.Join(tmp, "authkeys", "authorized_keys")
	srv.backupBaseDir = filepath.Join(tmp, "backups")
	srv.publicHost = "backup.test:2222"
	srv.clientImage = "th0rn0/backitup-client:test"
	srv.dbTimeout = d
	return st, srv
}

// testDBTimeout is the store deadline the shared fixtures give their server.
//
// The production default (5s) is a bound on a store that has stopped answering,
// and on an idle box a query takes about a millisecond. On a loaded CI runner a
// stalled disk can eat five seconds of wall clock, which made two builds red on
// a tree that was green (163, 165) — the assertion "create returns 200" was
// silently also an assertion "this runner never stalls for five seconds". A test
// that asserts handler behaviour should not be an assertion about the machine,
// so the fixtures use a bound long enough that only a genuine hang trips it.
//
// The timeout path itself is not skipped, it is tested on purpose: the tests
// below drive it with a deadline that has already expired, which is the same
// state the CI runner reached, reached deliberately.
const testDBTimeout = 30 * time.Second

// expiredDeadline is a deadline that is already in the past. It matters that it
// is negative rather than "very small": context.WithDeadline cancels
// synchronously when the deadline has passed, but a 1ns deadline depends on a
// timer firing before the store call, which is the same coin toss the CI
// failure was. With a spent deadline every store call sees DeadlineExceeded, on
// any machine, every time.
const expiredDeadline = -time.Second

// captureServerLog captures the process log for the duration of the test. No
// test in this package runs in parallel, so the global logger is safe to borrow.
func captureServerLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// sessionClient creates the admin account and a valid session cookie for it
// without calling POST /login: the login handler runs under the same store
// deadline the test is about to expire.
func sessionClient(t *testing.T, st *store.Store, srv *Server, ts *httptest.Server) *http.Client {
	t.Helper()
	setAdmin(t, st, "admin", "pw")
	u, err := st.GetUserByUsername(context.Background(), "admin")
	if err != nil || u == nil {
		t.Fatalf("load admin user: %v (%v)", u, err)
	}
	tok, err := srv.sessions.Create(u.ID, u.Username)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(ts.URL)
	jar.SetCookies(base, []*http.Cookie{{Name: sessionCookie, Value: tok, Path: "/"}})
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// assertStoreTimeoutResponse is the contract for every handler whose store work
// ran out of time: a 5xx the caller can retry, the same it would get from a
// server under load, and no misleading message about their input.
func assertStoreTimeoutResponse(t *testing.T, resp *http.Response, body string) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %q)", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		t.Error("no Retry-After header on a retryable failure")
	}
	if !strings.Contains(body, "did not respond in time") {
		t.Errorf("body does not say the store timed out: %q", body)
	}
}

// --- the two builds -------------------------------------------------------

// TestPostClientsStoreTimeoutIsReportedNotBlamed reproduces build 165:
// `create status = 400, want 200`. The 400 was clients.go's "Could not create
// client (is the name already taken?)" — a spent deadline read as a naming
// problem. It must be a 503 that names the timeout, and it must not have created
// anything.
func TestPostClientsStoreTimeoutIsReportedNotBlamed(t *testing.T) {
	st, srv := storeTimeoutStack(t, expiredDeadline)
	logged := captureServerLog(t)

	form := url.Values{"name": {"docs"}, "mode": {"rsync"}, "retention_days": {"7"}}
	req := httptest.NewRequest(http.MethodPost, "/clients", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.postClients(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("create status = %d, want 503; body %q", rec.Code, body)
	}
	assertStoreTimeoutResponse(t, rec.Result(), body)

	// The regression this test exists for: the old text pointed the operator at
	// their own client name.
	for _, misleading := range []string{"already taken", "Could not create client", "invalid form"} {
		if strings.Contains(body, misleading) {
			t.Errorf("a store timeout still blames the request (%q in %q)", misleading, body)
		}
	}

	// The 500/400 was also silent; without this line nothing in the step log
	// distinguishes a timeout from a real failure.
	for _, want := range []string{"create client \"docs\"", "store did not answer", "deadline of -1s"} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("log is missing %q; got %q", want, logged.String())
		}
	}

	// A 503 says the request did nothing. Prove it.
	clients, err := st.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("a timed-out create still wrote %d client(s): %+v", len(clients), clients)
	}
}

// TestRotateClientStoreTimeoutIsReported reproduces build 163:
// `rotate status = 500, want 200`. That 500 was clients.go's "rotate failed",
// logged nowhere, for a store that never answered.
func TestRotateClientStoreTimeoutIsReported(t *testing.T) {
	st, srv := storeTimeoutStack(t, expiredDeadline)
	logged := captureServerLog(t)

	id, err := st.CreateClient(context.Background(), model.Client{
		Name: "rotateme", Mode: model.ModeRsync, RetentionDays: 14,
		SSHPubKey: "ssh-ed25519 AAAA original", TokenHash: "originalhash", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/clients/rotateme/rotate",
		strings.NewReader(url.Values{"confirm": {"1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "rotateme")
	rec := httptest.NewRecorder()
	srv.postRotateClient(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rotate status = %d, want 503; body %q", rec.Code, body)
	}
	assertStoreTimeoutResponse(t, rec.Result(), body)
	if strings.Contains(body, "rotate failed") {
		t.Errorf("a store timeout still reports the rotation as failed: %q", body)
	}
	if !strings.Contains(logged.String(), "store did not answer") {
		t.Errorf("the 500 would have been silent again; log = %q", logged.String())
	}

	// Nothing rotated: the credentials are still the originals.
	got, err := st.GetClient(context.Background(), id)
	if err != nil || got == nil {
		t.Fatalf("get client: %v", err)
	}
	if got.SSHPubKey != "ssh-ed25519 AAAA original" || got.TokenHash != "originalhash" {
		t.Fatalf("a timed-out rotate changed the credentials anyway: %+v", got)
	}
}

// TestRotateClientPartialDeadline covers the shape CI actually saw: the handler
// had already done work (the SSH keypair, the token, its argon2 hash) before the
// store call, so the deadline expired *during* the request rather than before
// it. dbTimeout is 5ms and argon2id (64 MiB, one pass) cannot run in 5ms, so the
// write always finds the deadline spent; whichever store call trips first, the
// answer must be a 503.
func TestRotateClientPartialDeadline(t *testing.T) {
	st, srv := storeTimeoutStack(t, 5*time.Millisecond)
	logged := captureServerLog(t)

	_, err := st.CreateClient(context.Background(), model.Client{
		Name: "halfway", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/clients/halfway/rotate",
		strings.NewReader(url.Values{"confirm": {"1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetPathValue("name", "halfway")
	rec := httptest.NewRecorder()
	srv.postRotateClient(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("rotate status = %d, want 503; body %q", rec.Code, body)
	}
	assertStoreTimeoutResponse(t, rec.Result(), body)
	if !strings.Contains(logged.String(), "store did not answer") {
		t.Errorf("no log line for a partially spent deadline; log = %q", logged.String())
	}
}

// --- the same fault, seen from the other doors ----------------------------

// TestAdminPageStoreTimeoutIsNotALoginRedirect: requireAdmin could not verify the
// session, which used to bounce the admin to /login — a page that needs the same
// store, so the operator is sent to a door that cannot open.
func TestAdminPageStoreTimeoutIsNotALoginRedirect(t *testing.T) {
	st, srv := storeTimeoutStack(t, expiredDeadline)
	logged := captureServerLog(t)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	c := sessionClient(t, st, srv, ts)

	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d -> %q, want 503 while the store cannot answer",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	if !strings.Contains(buf.String(), "did not respond in time") {
		t.Errorf("body does not name the timeout: %q", buf.String())
	}
	if !strings.Contains(logged.String(), "requireAdmin") {
		t.Errorf("the session check failed silently; log = %q", logged.String())
	}
}

// TestLoginStoreTimeoutIsNotAWrongPassword: a store that cannot answer must not
// be reported as a 401, and — worse — must not be counted as a failed attempt.
// Ten failed attempts lock the operator out for a minute, so a store outage
// would lock them out of the server that has the fault.
func TestLoginStoreTimeoutIsNotAWrongPassword(t *testing.T) {
	_, srv := storeTimeoutStack(t, expiredDeadline)
	logged := captureServerLog(t)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.PostForm(ts.URL+"/login", url.Values{"username": {"admin"}, "password": {"pw"}})
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("login status = %d, want 503; body %q", resp.StatusCode, buf.String())
	}
	if !strings.Contains(buf.String(), "did not respond in time") {
		t.Errorf("body does not name the timeout: %q", buf.String())
	}
	if !strings.Contains(logged.String(), "login: load user") {
		t.Errorf("the failed user lookup is not in the log: %q", logged.String())
	}

	srv.limiter.mu.Lock()
	ips, pairs := len(srv.limiter.ips), len(srv.limiter.pairs)
	srv.limiter.mu.Unlock()
	if ips != 0 || pairs != 0 {
		t.Fatalf("a store timeout was recorded as a failed login attempt (%d address bucket(s), "+
			"%d address+user bucket(s) tracked); ten of those lock the operator out", ips, pairs)
	}
}

// TestClientAPITokenLookupTimeoutIsNotUnauthorized: 401 tells a backup client its
// token is bad, and a client that believes that re-enrols. A store that did not
// answer says nothing about the token.
func TestClientAPITokenLookupTimeoutIsNotUnauthorized(t *testing.T) {
	_, srv := storeTimeoutStack(t, expiredDeadline)
	logged := captureServerLog(t)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/config", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("config status = %d, want 503; body %q", resp.StatusCode, buf.String())
	}
	if !strings.Contains(logged.String(), "bearer token") {
		t.Errorf("the token lookup failure is not in the log: %q", logged.String())
	}
}

// --- the classification itself -------------------------------------------

func TestClassifyStoreFailure(t *testing.T) {
	live, cancel := context.WithCancel(context.Background())
	defer cancel()
	spentCtx, spentCancel := context.WithTimeout(context.Background(), expiredDeadline)
	defer spentCancel()
	cancelled, cancelledCancel := context.WithCancel(context.Background())
	cancelledCancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want storeTimeoutKind
	}{
		{"no error", live, nil, storeOK},
		{"deadline in the error", live, context.DeadlineExceeded, storeDeadline},
		{"deadline wrapped by the store", live,
			fmt.Errorf("insert client: %w", context.DeadlineExceeded), storeDeadline},
		{"deadline in the context, error from the driver", spentCtx,
			errors.New("interrupted"), storeDeadline},
		{"request cancelled mid-flight", cancelled, context.Canceled, storeDeadline},
		{"sqlite writer timeout", live, errors.New("database is locked (5) (SQLITE_BUSY)"), storeLocked},
		{"sqlite table lock", live, errors.New("database table is locked"), storeLocked},
		{"closed store", live, errors.New("sql: database is closed"), storeOK},
		{"constraint violation", live, errors.New("UNIQUE constraint failed: clients.slug"), storeOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStoreFailure(tc.ctx, tc.err); got != tc.want {
				t.Fatalf("classifyStoreFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestStoreFailureLeavesGenuineErrorsToTheCaller: only availability failures are
// answered here; everything else must still reach the caller's own 500, or a
// real bug would be dressed up as "retry later".
func TestStoreFailureLeavesGenuineErrorsToTheCaller(t *testing.T) {
	_, srv := storeTimeoutStack(t, defaultDBTimeout)
	logged := captureServerLog(t)

	rec := httptest.NewRecorder()
	handled := srv.storeFailure(rec, context.Background(), "create client \"docs\"",
		errors.New("UNIQUE constraint failed: clients.slug"))

	if handled {
		t.Fatal("a constraint violation was reported as a store availability failure")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("storeFailure wrote a body for an error it did not handle: %q", rec.Body.String())
	}
	if !strings.Contains(logged.String(), "UNIQUE constraint failed") {
		t.Errorf("the underlying error was not logged: %q", logged.String())
	}
}

// TestDefaultDBTimeout guards the knob: the handlers must keep the five seconds
// they had, and a test-only value must never become the shipped one.
func TestDefaultDBTimeout(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if got := New(st, false).dbTimeout; got != 5*time.Second {
		t.Fatalf("default dbTimeout = %v, want 5s", got)
	}
}
