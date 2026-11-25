package server

import (
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/model"
)

// group_readable is the one per-client setting that trades fidelity for access,
// so it has to reach the client (that is how a run learns to pass
// --chmod=Dg+rx,Fg+r), has to be visible on the page, and has to say on the page
// what it costs. These tests cover that path end to end.

func TestConfigCarriesGroupReadable(t *testing.T) {
	st, ts := testStack(t)
	token := "group-readable-token"
	hash, err := auth.HashPassword(token)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := st.CreateClient(t.Context(), model.Client{
		Name: "vm-host", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA vm-host", TokenHash: hash, TokenPrefix: token[:8],
		GroupReadable: true,
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	resp := doAuthed(t, ts, "GET", "/api/v1/config", token, "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(body), `"group_readable":true`) {
		t.Errorf("/api/v1/config does not carry the setting: %s", body)
	}

	// A client that never had it on reads back off — an upgraded database keeps
	// producing the snapshots it always did.
	if _, err := st.CreateClient(t.Context(), model.Client{
		Name: "other", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA other", TokenHash: hash, TokenPrefix: "group2",
	}); err != nil {
		t.Fatalf("create second client: %v", err)
	}
	other, err := st.GetClientBySlug(t.Context(), "other")
	if err != nil || other == nil {
		t.Fatalf("load second client: %v", err)
	}
	if other.GroupReadable {
		t.Errorf("group_readable defaults to on: %+v", other)
	}
}

// The page states the trade, saves the setting, and clears it again when the box
// is unticked (a browser sends nothing for an unticked box, so "no key" has to
// mean off).
func TestClientGroupReadableFormSavesAndShows(t *testing.T) {
	st, _, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"vm-host"}, "mode": {"rsync"}})

	page := getPage(t, c, ts.URL+"/clients/vm-host")
	for _, want := range []string{
		"Group-readable snapshots",
		`name="group_readable" value="1">`,
		"no longer match the source",
		"earlier snapshots",
		"--chmod=Dg+rx,Fg+r",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("client detail page is missing %q", want)
		}
	}

	postForm(t, c, ts.URL+"/clients/vm-host/group-readable", url.Values{"group_readable": {"1"}})
	client, err := st.GetClientBySlug(t.Context(), "vm-host")
	if err != nil || client == nil {
		t.Fatalf("load client: %v", err)
	}
	if !client.GroupReadable {
		t.Fatal("the form did not save the setting")
	}
	if page := getPage(t, c, ts.URL+"/clients/vm-host"); !strings.Contains(page, `name="group_readable" value="1" checked`) {
		t.Error("the saved setting does not render as checked")
	}

	postForm(t, c, ts.URL+"/clients/vm-host/group-readable", url.Values{})
	client, err = st.GetClientBySlug(t.Context(), "vm-host")
	if err != nil || client == nil {
		t.Fatalf("reload client: %v", err)
	}
	if client.GroupReadable {
		t.Fatal("unticking the box left the setting on")
	}
}

// A tar.gz client has no snapshot directory to widen: its receiver makes one
// archive whose modes are the source's. Asking for the setting is refused with a
// message rather than stored and silently ignored on every run.
func TestGroupReadableRefusedForTarGzClient(t *testing.T) {
	st, _, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	nc := noRedirectClientWithSession(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"archive"}, "mode": {"targz"}})

	loc := postNoRedirect(t, nc, ts.URL+"/clients/archive/group-readable", url.Values{"group_readable": {"1"}})
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "rsync") {
		t.Errorf("refusal flash = %q, want an err= explaining that the setting is rsync-only", loc)
	}
	client, err := st.GetClientBySlug(t.Context(), "archive")
	if err != nil || client == nil {
		t.Fatalf("load client: %v", err)
	}
	if client.GroupReadable {
		t.Error("the refused setting was stored anyway")
	}
}
