package server

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// The values below are the secrets an operator types into the crypt form. They
// must never leave the request: not in a response body, not in a log line, not
// in rclone.conf in plaintext.
const (
	cryptPassword  = "crypto-Pa55word-must-never-leak"
	cryptSalt      = "crypto-Salt-must-never-leak"
	cryptPassword2 = "crypto-NewPa55word-must-never-leak"
)

// cryptStack is rcloneStack with the ingest wiring the client pages need, and
// it hands back the Server and the rclone.conf path so a test can drive the
// startup write and inspect the file the app produces.
func cryptStack(t *testing.T) (*store.Store, *httptest.Server, *Server, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	srv := New(st, false)
	srv.dbTimeout = testDBTimeout // assert behaviour, not the machine's speed
	tmp := t.TempDir()
	srv.authKeysPath = filepath.Join(tmp, "authkeys", "authorized_keys")
	srv.backupBaseDir = filepath.Join(tmp, "backups")
	srv.publicHost = "backup.test:2222"
	srv.clientImage = "th0rn0/backitup-client:test"
	confPath := filepath.Join(tmp, "rclone.conf")
	srv.ConfigureRclone(confPath)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return st, ts, srv, confPath
}

// installFakeRclone puts a stub `rclone` at the front of PATH. It appends one
// line per invocation to the returned log path and answers `obscure` with a hex
// digest of the password — deterministic, and containing no plaintext, so a
// test can tell "obscured" from "leaked" without guessing rclone's own format.
func installFakeRclone(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "rclone-calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + calls + "\n" +
		"case \" $* \" in\n" +
		"  *\" obscure \"*)\n" +
		"    printf 'OBSCURED-'\n" +
		"    od -An -tx1 | tr -d ' \\n'\n" +
		"    printf '\\n'\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	bin := filepath.Join(dir, "rclone")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("install fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// postForm posts a form and returns the body of the page the redirect lands on.
func postForm(t *testing.T, c *http.Client, u string, form url.Values) string {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatalf("post %s: %v", u, err)
	}
	return readBody(t, resp)
}

// postNoRedirect posts a form without following the redirect, so the Location
// (where the handler puts its error message) is observable.
func postNoRedirect(t *testing.T, c *http.Client, u string, form url.Values) string {
	t.Helper()
	resp, err := c.PostForm(u, form)
	if err != nil {
		t.Fatalf("post %s: %v", u, err)
	}
	defer resp.Body.Close()
	loc, err := url.QueryUnescape(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("post %s: unescape Location %q: %v", u, resp.Header.Get("Location"), err)
	}
	// The message lives in the query string; the body is empty either way.
	return loc
}

// getPage fetches a page and returns its body.
func getPage(t *testing.T, c *http.Client, u string) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("get %s: %v", u, err)
	}
	return readBody(t, resp)
}

func mustReadConf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rclone.conf: %v", err)
	}
	return string(b)
}

func wantIn(t *testing.T, where, haystack string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(haystack, w) {
			t.Fatalf("%s: missing %q in:\n%s", where, w, haystack)
		}
	}
}

func wantNotIn(t *testing.T, where, haystack string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(haystack, w) {
			t.Fatalf("%s: %q must not appear in:\n%s", where, w, haystack)
		}
	}
}

// createPlaintextRemote adds an ordinary (b2) remote through the API.
func createPlaintextRemote(t *testing.T, c *http.Client, base, name string) {
	t.Helper()
	body := postForm(t, c, base+"/settings/remotes", url.Values{
		"name": {name}, "backend": {"b2"}, "account": {name + "-account"}, "key": {name + "-key"},
	})
	wantIn(t, "create plaintext remote "+name, body, "created")
}

// createCryptRemote adds a crypt remote wrapping parent through the API and
// returns the page body the flash landed on.
func createCryptRemote(t *testing.T, c *http.Client, base, name, parent, filenameEncryption string) string {
	t.Helper()
	return postForm(t, c, base+"/settings/remotes", url.Values{
		"name":                {name},
		"backend":             {"crypt"},
		"remote":              {parent},
		"filename_encryption": {filenameEncryption},
		"password":            {cryptPassword},
		"password2":           {cryptSalt},
	})
}

// ── the hand-written section must survive every write ────────────────────────

// TestHandWrittenRcloneSectionSurvivesEveryWrite is the regression test for the
// reported bug: backitup regenerated rclone.conf from the remotes table, so a
// [crypt] section an operator wrote by hand disappeared the next time anybody
// created, edited or deleted a remote — or simply restarted the server.
//
// The section is read back after every one of those writes.
func TestHandWrittenRcloneSectionSurvivesEveryWrite(t *testing.T) {
	installFakeRclone(t) // the crypt remote below needs `rclone obscure`
	st, ts, srv, confPath := cryptStack(t)
	c := loggedInClient(t, st, ts)

	handWritten := "# Operator-owned: backitup must never rewrite or delete this.\n" +
		"[crypt]\n" +
		"type = crypt\n" +
		"remote = provider:backed\n" +
		"filename_encryption = standard\n" +
		"password = hand-written-password\n" +
		"password2 = hand-written-salt\n"
	want := strings.TrimRight(handWritten, "\n")
	if err := os.WriteFile(confPath, []byte(handWritten), 0o600); err != nil {
		t.Fatalf("seed rclone.conf: %v", err)
	}

	check := func(step string) string {
		t.Helper()
		got := mustReadConf(t, confPath)
		if !strings.Contains(got, want) {
			t.Fatalf("%s: the hand-written [crypt] section was lost or altered.\nwant this substring:\n%s\ngot file:\n%s",
				step, want, got)
		}
		return got
	}

	if err := srv.RegenerateRcloneConfig(t.Context()); err != nil {
		t.Fatalf("startup regenerate: %v", err)
	}
	check("startup write")

	createPlaintextRemote(t, c, ts.URL, "provider")
	got := check("after POST /settings/remotes (plaintext remote)")
	wantIn(t, "after create", got, "[provider]", "type = b2")

	if body := createCryptRemote(t, c, ts.URL, "vault", "provider", "standard"); !strings.Contains(body, "created") {
		t.Fatalf("create crypt remote: flash missing %q:\n%s", "created", body)
	}
	got = check("after POST /settings/remotes (crypt remote)")
	wantIn(t, "after crypt create", got, "[vault]", "type = crypt", "remote = provider:")
	if n := strings.Count(got, rcloneManagedEnd); n != 1 {
		t.Fatalf("after crypt create: %d managed blocks in rclone.conf, want 1:\n%s", n, got)
	}

	postForm(t, c, ts.URL+"/settings/remotes/provider/edit", url.Values{"account": {"changed-account"}})
	got = check("after POST /settings/remotes/provider/edit")
	wantIn(t, "after edit", got, "account = changed-account", "[vault]")

	postForm(t, c, ts.URL+"/settings/remotes/vault/delete", nil)
	got = check("after POST /settings/remotes/vault/delete")
	wantNotIn(t, "after crypt delete", got, "[vault]")

	postForm(t, c, ts.URL+"/settings/remotes/provider/delete", nil)
	got = check("after POST /settings/remotes/provider/delete")
	// With every managed remote gone, only the operator's content is left — and
	// no empty marker block is left behind.
	if strings.TrimRight(got, "\n") != want {
		t.Fatalf("after deleting every remote rclone.conf should be the operator's content alone:\ngot:\n%q\nwant:\n%q", got, want)
	}

	if err := srv.RegenerateRcloneConfig(t.Context()); err != nil {
		t.Fatalf("second startup regenerate: %v", err)
	}
	if got = check("second startup write"); strings.TrimRight(got, "\n") != want {
		t.Fatalf("second startup write changed rclone.conf:\ngot:\n%q\nwant:\n%q", got, want)
	}
}

// ── a crypt remote is creatable through the API ──────────────────────────────

func TestCreateCryptRemoteThroughAPI(t *testing.T) {
	calls := installFakeRclone(t)
	st, ts, _, confPath := cryptStack(t)
	c := loggedInClient(t, st, ts)

	createPlaintextRemote(t, c, ts.URL, "provider")
	body := createCryptRemote(t, c, ts.URL, "vault", "provider:", "standard")
	wantIn(t, "crypt create flash", body, "created")

	// The stored config names the plaintext remote without rclone's colon: the
	// colon is config syntax, not data.
	remote, err := st.GetRemoteByName(t.Context(), "vault")
	if err != nil || remote == nil {
		t.Fatalf("crypt remote not stored: %v (remote=%v)", err, remote)
	}
	if remote.Backend != model.BackendCrypt {
		t.Fatalf("stored backend = %q, want %q", remote.Backend, model.BackendCrypt)
	}
	if remote.Config["remote"] != "provider" {
		t.Fatalf("stored remote = %q, want %q", remote.Config["remote"], "provider")
	}
	if !remote.IsCrypt() || remote.CryptParent() != "provider" {
		t.Fatalf("CryptParent() = %q (IsCrypt %v), want \"provider\"", remote.CryptParent(), remote.IsCrypt())
	}

	conf := mustReadConf(t, confPath)
	wantIn(t, "generated rclone.conf", conf,
		"[vault]",
		"type = crypt",
		"remote = provider:", // references the plaintext remote rclone talks to
		"filename_encryption = standard",
		"[provider]",
	)
	wantNotIn(t, "generated rclone.conf", conf, cryptPassword, cryptSalt)

	// The obscured form is what rclone needs; it is present, and it is not the
	// password.
	for _, key := range []string{"password", "password2"} {
		stored := remote.Config[key]
		if stored == "" || !strings.HasPrefix(stored, "OBSCURED-") {
			t.Fatalf("%s stored as %q; want an `rclone obscure` value", key, stored)
		}
		if !strings.Contains(conf, key+" = "+stored) {
			t.Fatalf("rclone.conf does not carry %s = <obscured>:\n%s", key, conf)
		}
	}

	// `rclone obscure` is fed on stdin, never argv, so the password cannot be
	// read out of /proc/<pid>/cmdline or ps.
	logged, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("read rclone call log: %v", err)
	}
	wantIn(t, "rclone invocation log", string(logged), "obscure -")
	wantNotIn(t, "rclone invocation log", string(logged), cryptPassword, cryptSalt)

	// The file holds the key material: it must not be world-readable.
	info, err := os.Stat(confPath)
	if err != nil {
		t.Fatalf("stat rclone.conf: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("rclone.conf permissions = %#o, want 0600 — it holds obscured passwords", perm)
	}
}

// TestCryptRemoteRefusesBadConfig covers the checks the form alone cannot make.
func TestCryptRemoteRefusesBadConfig(t *testing.T) {
	cases := []struct {
		name    string
		form    url.Values
		wantMsg string
	}{
		{
			name: "unknown plaintext remote",
			form: url.Values{"name": {"vault"}, "backend": {"crypt"}, "remote": {"nope"},
				"password": {"p"}, "password2": {"s"}},
			wantMsg: "does not exist yet",
		},
		{
			name: "missing plaintext remote",
			form: url.Values{"name": {"vault"}, "backend": {"crypt"}, "remote": {""},
				"password": {"p"}, "password2": {"s"}},
			wantMsg: "required",
		},
		{
			name: "wraps itself",
			form: url.Values{"name": {"vault"}, "backend": {"crypt"}, "remote": {"vault"},
				"password": {"p"}, "password2": {"s"}},
			wantMsg: "cannot encrypt itself",
		},
		{
			name: "plaintext remote name with a space",
			form: url.Values{"name": {"vault"}, "backend": {"crypt"}, "remote": {"bad name"},
				"password": {"p"}, "password2": {"s"}},
			wantMsg: "may only contain",
		},
		{
			name: "filename encryption not one of the options",
			form: url.Values{"name": {"vault"}, "backend": {"crypt"}, "remote": {"provider"},
				"filename_encryption": {"rot13"}, "password": {"p"}, "password2": {"s"}},
			wantMsg: "not one of the allowed choices",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installFakeRclone(t)
			st, ts, _, confPath := cryptStack(t)
			c := loggedInClient(t, st, ts)
			nc := noRedirectClientWithSession(t, st, ts)
			createPlaintextRemote(t, c, ts.URL, "provider")

			loc := postNoRedirect(t, nc, ts.URL+"/settings/remotes", tc.form)
			if !strings.Contains(loc, "err=") || !strings.Contains(loc, tc.wantMsg) {
				t.Fatalf("redirect = %q; want an error carrying %q", loc, tc.wantMsg)
			}
			remote, err := st.GetRemoteByName(t.Context(), "vault")
			if err != nil {
				t.Fatalf("look up remote: %v", err)
			}
			if remote != nil {
				t.Fatalf("a rejected crypt remote was still stored: %+v", remote)
			}
			wantNotIn(t, "rclone.conf after rejected create", mustReadConf(t, confPath), "[vault]")
		})
	}
}

// An encrypted remote may not wrap another encrypted remote: the field is
// labelled "Plaintext remote", and nesting makes the failure modes unreadable.
func TestCryptRemoteRefusesToWrapAnotherCrypt(t *testing.T) {
	installFakeRclone(t)
	st, ts, _, confPath := cryptStack(t)
	c := loggedInClient(t, st, ts)
	nc := noRedirectClientWithSession(t, st, ts)
	createPlaintextRemote(t, c, ts.URL, "provider")
	createCryptRemote(t, c, ts.URL, "vault", "provider", "standard")

	loc := postNoRedirect(t, nc, ts.URL+"/settings/remotes", url.Values{
		"name": {"inner"}, "backend": {"crypt"}, "remote": {"vault"},
		"password": {cryptPassword}, "password2": {cryptSalt},
	})
	if !strings.Contains(loc, "itself encrypted") {
		t.Fatalf("redirect = %q; want a refusal naming the encrypted parent", loc)
	}
	wantNotIn(t, "rclone.conf", mustReadConf(t, confPath), "[inner]")
}

// An operator's own `[provider]` section is a legitimate parent: it has no row
// in the database, and it must keep working.
func TestCryptRemoteMayWrapAHandWrittenRemote(t *testing.T) {
	installFakeRclone(t)
	st, ts, _, confPath := cryptStack(t)
	c := loggedInClient(t, st, ts)

	const operatorProvider = "[provider]\ntype = local\n"
	if err := os.WriteFile(confPath, []byte(operatorProvider), 0o600); err != nil {
		t.Fatalf("seed rclone.conf: %v", err)
	}

	body := createCryptRemote(t, c, ts.URL, "vault", "provider", "standard")
	wantIn(t, "crypt over a hand-written remote", body, "created")

	conf := mustReadConf(t, confPath)
	wantIn(t, "rclone.conf", conf, "[provider]", "type = local", "[vault]", "remote = provider:")
	if !strings.Contains(conf, operatorProvider) {
		t.Fatalf("the operator's [provider] section was altered:\n%s", conf)
	}
}

// Deleting the plaintext remote out from under an encrypted one would leave a
// config rclone cannot resolve, so it is refused.
func TestDeletePlaintextRemoteRefusedWhileEncrypted(t *testing.T) {
	installFakeRclone(t)
	st, ts, _, confPath := cryptStack(t)
	c := loggedInClient(t, st, ts)
	nc := noRedirectClientWithSession(t, st, ts)
	createPlaintextRemote(t, c, ts.URL, "provider")
	createCryptRemote(t, c, ts.URL, "vault", "provider", "standard")

	loc := postNoRedirect(t, nc, ts.URL+"/settings/remotes/provider/delete", nil)
	if !strings.Contains(loc, "delete that one first") {
		t.Fatalf("delete of an encrypted remote's parent = %q; want a refusal", loc)
	}
	if got, err := st.GetRemoteByName(t.Context(), "provider"); err != nil || got == nil {
		t.Fatalf("provider remote gone after a refused delete (err=%v)", err)
	}
	wantIn(t, "rclone.conf", mustReadConf(t, confPath), "[provider]", "[vault]")

	// Deleting the encrypted remote first is allowed, and then the parent.
	postForm(t, c, ts.URL+"/settings/remotes/vault/delete", nil)
	postForm(t, c, ts.URL+"/settings/remotes/provider/delete", nil)
	if got, err := st.GetRemoteByName(t.Context(), "provider"); err != nil || got != nil {
		t.Fatalf("provider remote survived its delete (err=%v, remote=%v)", err, got)
	}
}

// ── secrets stay write-only ──────────────────────────────────────────────────

// TestCryptSecretsNeverLeak asserts the acceptance criterion on the HTML the
// operator sees and on the log lines the server writes: the passwords appear in
// neither, in any form.
func TestCryptSecretsNeverLeak(t *testing.T) {
	installFakeRclone(t)

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	st, ts, _, confPath := cryptStack(t)
	c := loggedInClient(t, st, ts)

	createPlaintextRemote(t, c, ts.URL, "provider")

	var bodies []string
	bodies = append(bodies, createCryptRemote(t, c, ts.URL, "vault", "provider", "standard"))
	bodies = append(bodies, getPage(t, c, ts.URL+"/settings/remotes"))
	bodies = append(bodies, getPage(t, c, ts.URL+"/settings/remotes/vault/edit"))

	remote, err := st.GetRemoteByName(t.Context(), "vault")
	if err != nil || remote == nil {
		t.Fatalf("crypt remote not stored: %v", err)
	}
	obscuredPassword := remote.Config["password"]
	obscuredSalt := remote.Config["password2"]
	if obscuredPassword == "" || obscuredSalt == "" {
		t.Fatalf("crypt passwords were not stored: %+v", remote.Config)
	}

	// Editing rotates the password; the new one must not leak either.
	bodies = append(bodies, postForm(t, c, ts.URL+"/settings/remotes/vault/edit", url.Values{
		"password": {cryptPassword2},
	}))
	bodies = append(bodies, getPage(t, c, ts.URL+"/settings/remotes/vault/edit"))
	bodies = append(bodies, getPage(t, c, ts.URL+"/settings/remotes"))

	// The client pages render the offsite destination list, which names the
	// encrypted remote — still no secrets.
	bodies = append(bodies, getPage(t, c, ts.URL+"/clients/new"))

	rotated, err := st.GetRemoteByName(t.Context(), "vault")
	if err != nil || rotated == nil {
		t.Fatalf("crypt remote gone after edit: %v", err)
	}
	if rotated.Config["password"] == obscuredPassword {
		t.Fatal("editing the password did not change the stored value")
	}

	secrets := []string{
		cryptPassword, cryptSalt, cryptPassword2,
		obscuredPassword, obscuredSalt, rotated.Config["password"],
	}
	for i, body := range bodies {
		wantNotIn(t, "HTTP response body", body, secrets...)
		if i == 0 {
			wantIn(t, "response body", body, "vault")
		}
	}
	wantNotIn(t, "server log", logs.String(), secrets...)
	wantNotIn(t, "rclone.conf", mustReadConf(t, confPath), cryptPassword, cryptSalt, cryptPassword2)
}

// ── the offsite destination can be the encrypted remote ──────────────────────

// TestOffsiteDestinationMayBeCryptRemote drives the operator path that makes
// offsite encrypted: pick the crypt remote as the client's destination. The
// upload itself is lifecycle's job and is covered by
// TestOffsiteUploadRunsThroughCryptRemote there.
func TestOffsiteDestinationMayBeCryptRemote(t *testing.T) {
	installFakeRclone(t)
	st, ts, _, _ := cryptStack(t)
	c := loggedInClient(t, st, ts)
	nc := noRedirectClientWithSession(t, st, ts)
	createPlaintextRemote(t, c, ts.URL, "provider")
	createCryptRemote(t, c, ts.URL, "vault", "provider", "standard")

	postForm(t, c, ts.URL+"/clients", url.Values{
		"name": {"laptop"}, "mode": {"rsync"}, "offsite_remote": {"vault"},
		"offsite_dir": {"clients"}, "expected_interval_secs": {"86400"},
	})

	loc := postNoRedirect(t, nc, ts.URL+"/clients/laptop/offsite", url.Values{
		"offsite_remote": {"vault"}, "offsite_dir": {"clients"},
	})
	if strings.Contains(loc, "err=") {
		t.Fatalf("selecting the encrypted remote as the destination was refused: %q", loc)
	}

	client, err := st.GetClientBySlug(t.Context(), "laptop")
	if err != nil || client == nil {
		t.Fatalf("load client: %v (client=%v)", err, client)
	}
	if client.OffsiteRemote != "vault" {
		t.Fatalf("client offsite remote = %q, want the encrypted remote %q", client.OffsiteRemote, "vault")
	}

	// And the page tells the operator what that destination is.
	wantIn(t, "client detail page", getPage(t, c, ts.URL+"/clients/laptop"),
		"Encrypted (rclone crypt)")
}
