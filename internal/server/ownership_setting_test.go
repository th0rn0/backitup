package server

import (
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/model"
)

// The ownership setting is the one per-client setting whose effect is NOT an
// rsync flag the client sends: it selects the sshd forced command. So the tests
// that matter are (a) the client learns it is on, (b) saving it rewrites
// authorized_keys in the same request, and (c) a mode that cannot use it is
// refused rather than stored.

func TestConfigCarriesRecordOwnership(t *testing.T) {
	st, ts := testStack(t)
	token := "record-ownership-token"
	hash, err := auth.HashPassword(token)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := st.CreateClient(t.Context(), model.Client{
		Name: "vm-host", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA vm-host", TokenHash: hash, TokenPrefix: token[:8],
		RecordOwnership: true,
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	resp := doAuthed(t, ts, "GET", "/api/v1/config", token, "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(body), `"record_ownership":true`) {
		t.Errorf("/api/v1/config does not carry the setting: %s", body)
	}

	// An upgraded client reads back off: the client's log must not start talking
	// about xattrs it does not have.
	if _, err := st.CreateClient(t.Context(), model.Client{
		Name: "other", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA other", TokenHash: hash, TokenPrefix: "own00002",
	}); err != nil {
		t.Fatalf("create second client: %v", err)
	}
	other, err := st.GetClientBySlug(t.Context(), "other")
	if err != nil || other == nil {
		t.Fatalf("load second client: %v", err)
	}
	if other.RecordOwnership {
		t.Errorf("record_ownership defaults to on: %+v", other)
	}
}

// Saving the setting has to rewrite authorized_keys in the same request. If it
// does not, the page says one thing and the next run does another — the failure
// this setting's whole design invites, because the store write is not the effect.
func TestRecordOwnershipFormRewritesAuthorizedKeys(t *testing.T) {
	st, srv, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"vm-host"}, "mode": {"rsync"}})

	read := func() string {
		b, err := os.ReadFile(srv.authKeysPath)
		if err != nil {
			t.Fatalf("read authorized_keys: %v", err)
		}
		return string(b)
	}
	if keys := read(); !strings.Contains(keys, `command="rrsync `) {
		t.Fatalf("a new client is not forced through rrsync:\n%s", keys)
	}

	postForm(t, c, ts.URL+"/clients/vm-host/ownership", url.Values{"record_ownership": {"1"}})
	client, err := st.GetClientBySlug(t.Context(), "vm-host")
	if err != nil || client == nil {
		t.Fatalf("load client: %v", err)
	}
	if !client.RecordOwnership {
		t.Fatal("the form did not save the setting")
	}
	if keys := read(); !strings.Contains(keys, `command="backitup-rrsync `) {
		t.Fatalf("saving the setting did not rewrite the forced command:\n%s", keys)
	}
	if page := getPage(t, c, ts.URL+"/clients/vm-host"); !strings.Contains(page, `name="record_ownership" value="1" checked`) {
		t.Error("the saved setting does not render as checked")
	}

	// Unticking it (a browser sends nothing) puts the client back on the plain
	// command, which is what restoring the old snapshot shape requires.
	postForm(t, c, ts.URL+"/clients/vm-host/ownership", url.Values{})
	client, err = st.GetClientBySlug(t.Context(), "vm-host")
	if err != nil || client == nil {
		t.Fatalf("reload client: %v", err)
	}
	if client.RecordOwnership {
		t.Fatal("unticking the box left the setting on")
	}
	if keys := read(); !strings.Contains(keys, `command="rrsync `) || strings.Contains(keys, "backitup-rrsync") {
		t.Fatalf("turning the setting off did not restore the plain command:\n%s", keys)
	}
}

// A tar.gz client's receiver is not rsync: there is no server-side rsync to add
// the flag to, and its own tar already records the numeric owner. Storing the
// setting would promise something no run performs.
func TestRecordOwnershipRefusedForTarGzClient(t *testing.T) {
	st, srv, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	nc := noRedirectClientWithSession(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"archive"}, "mode": {"targz"}})

	loc := postNoRedirect(t, nc, ts.URL+"/clients/archive/ownership", url.Values{"record_ownership": {"1"}})
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "rsync") {
		t.Errorf("refusal flash = %q, want an err= explaining that the setting is rsync-only", loc)
	}
	client, err := st.GetClientBySlug(t.Context(), "archive")
	if err != nil || client == nil {
		t.Fatalf("load client: %v", err)
	}
	if client.RecordOwnership {
		t.Error("the refused setting was stored anyway")
	}
	keys, err := os.ReadFile(srv.authKeysPath)
	if err != nil {
		t.Fatalf("read authorized_keys: %v", err)
	}
	if strings.Contains(string(keys), "backitup-rrsync") {
		t.Errorf("a refused setting changed the forced command anyway:\n%s", keys)
	}
}

// The page has to state what the snapshot stores and how to get the ownership
// back, because a restore that does not know about the xattr silently loses it.
func TestClientPageDescribesOwnership(t *testing.T) {
	st, _, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"vm-host"}, "mode": {"rsync"}})

	page := getPage(t, c, ts.URL+"/clients/vm-host")
	for _, want := range []string{
		"Ownership in snapshots",
		`name="record_ownership" value="1"`,
		"user.rsync.%stat",
		"rsync --fake-super",
		"extended attributes",
		"tar --xattrs",
		"--fake-super",
		// The restore command the page states, with this server's ingest address.
		"backup@backup.test:2222:snapshots/",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("client detail page is missing %q", want)
		}
	}
	// The old claim — that ownership is not preserved, full stop — has to be
	// gone, or the page contradicts the setting below it.
	if strings.Contains(page, "Ownership (uid/gid) is <strong>not</strong> preserved in the hot tier") {
		t.Error("the page still says ownership is never preserved")
	}
	if !strings.Contains(page, "a plain <code>cp</code> or <code>tar</code> out of a snapshot loses it") {
		t.Error("the page does not warn that a copy out of a snapshot drops the recorded owner")
	}
}

// Files in the hot tier stay owned by the backup account: the page must say so,
// or an operator reads "records ownership" as "changes ownership".
func TestClientPageSaysTheHotTierStaysOwnedByBackup(t *testing.T) {
	st, _, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"vm-host"}, "mode": {"rsync"}})
	page := getPage(t, c, ts.URL+"/clients/vm-host")
	if !strings.Contains(page, "stay owned by the backup account") {
		t.Error("the page does not say the hot tier keeps the backup account as owner")
	}
	if !strings.Contains(page, filepath.ToSlash("qcow2")) {
		t.Error("the page does not say what this setting cannot fix (a source the client cannot read)")
	}
}
