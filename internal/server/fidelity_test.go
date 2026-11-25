package server

import (
	"io"
	"net/url"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/model"
)

// The operator path for the fidelity settings: the create form's boxes are
// stored, the client's own /api/v1/config carries them (that is how the run
// learns about them), and the settings tab shows and saves them.
func TestCreateClientStoresFidelitySettings(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)

	postForm(t, c, ts.URL+"/clients", url.Values{
		"name": {"vm-host"}, "mode": {"rsync"},
		// Sparse is the create form's default, so it arrives checked when the
		// operator does not touch it; the other three are opt-in.
		"preserve_sparse": {"1"}, "preserve_acls": {"1"},
	})

	client, err := st.GetClientBySlug(t.Context(), "vm-host")
	if err != nil || client == nil {
		t.Fatalf("load the created client: %v (client=%v)", err, client)
	}
	if !client.PreserveSparse || !client.PreserveACLs {
		t.Fatalf("the create form's checked boxes were not stored: %+v", client)
	}
	if client.PreserveHardlinks || client.PreserveXattrs {
		t.Fatalf("an unchecked box was stored as on: %+v", client)
	}

	// A second client with nothing checked keeps every setting off — the
	// regression shape for clients created from a form that predates them.
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"plain-box"}, "mode": {"rsync"}})
	plain, err := st.GetClientBySlug(t.Context(), "plain-box")
	if err != nil || plain == nil {
		t.Fatalf("load plain-box: %v", err)
	}
	if plain.PreserveSparse || plain.PreserveHardlinks || plain.PreserveACLs || plain.PreserveXattrs {
		t.Fatalf("a client created with no boxes ticked read back with a setting on: %+v", plain)
	}
}

func TestConfigCarriesFidelitySettings(t *testing.T) {
	st, ts := testStack(t)
	token := "fidelity-token"
	hash, err := auth.HashPassword(token)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := st.CreateClient(t.Context(), model.Client{
		Name: "vm-host", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA vm-host", TokenHash: hash, TokenPrefix: "fidelity",
		PreserveSparse: true, PreserveHardlinks: true,
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	resp := doAuthed(t, ts, "GET", "/api/v1/config", token, "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, want := range []string{`"preserve_sparse":true`, `"preserve_hardlinks":true`, `"preserve_acls":false`, `"preserve_xattrs":false`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/api/v1/config is missing %s: %s", want, body)
		}
	}
}

// The settings tab saves them, shows them back, and clears them when every box
// is unticked (a browser sends nothing for an unticked box, so "no keys" has to
// mean "all off" rather than "leave as it was").
func TestClientFidelityFormSavesAndShows(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	postForm(t, c, ts.URL+"/clients", url.Values{"name": {"vm-host"}, "mode": {"rsync"}})

	postForm(t, c, ts.URL+"/clients/vm-host/fidelity", url.Values{
		"preserve_sparse": {"1"}, "preserve_hardlinks": {"1"}, "preserve_acls": {"1"}, "preserve_xattrs": {"1"},
	})
	client, err := st.GetClientBySlug(t.Context(), "vm-host")
	if err != nil || client == nil {
		t.Fatalf("load client: %v", err)
	}
	if !client.PreserveSparse || !client.PreserveHardlinks || !client.PreserveACLs || !client.PreserveXattrs {
		t.Fatalf("the settings form did not save every box: %+v", client)
	}

	page := getPage(t, c, ts.URL+"/clients/vm-host")
	// The settings are on the page with the cost note that justifies each one.
	for _, want := range []string{
		"What the snapshot captures",
		`name="preserve_sparse" value="1" checked`,
		`name="preserve_hardlinks" value="1" checked`,
		`name="preserve_acls" value="1" checked`,
		`name="preserve_xattrs" value="1" checked`,
		"Ownership (uid/gid) is",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("client detail page is missing %q", want)
		}
	}

	// Unticking everything clears them all.
	postForm(t, c, ts.URL+"/clients/vm-host/fidelity", url.Values{})
	client, err = st.GetClientBySlug(t.Context(), "vm-host")
	if err != nil || client == nil {
		t.Fatalf("reload client: %v", err)
	}
	if client.PreserveSparse || client.PreserveHardlinks || client.PreserveACLs || client.PreserveXattrs {
		t.Fatalf("clearing every box left a setting on: %+v", client)
	}
	if page := getPage(t, c, ts.URL+"/clients/vm-host"); strings.Contains(page, `name="preserve_acls" value="1" checked`) {
		t.Error("the page still renders a cleared setting as checked")
	}
}

// The create form itself: the four boxes exist, each with its one-line cost
// note, and sparse is pre-ticked while the rest are not.
func TestNewClientFormOffersFidelitySettings(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	page := getPage(t, c, ts.URL+"/clients/new")

	for _, want := range []string{
		`name="preserve_sparse" value="1" checked`,
		`name="preserve_hardlinks" value="1">`,
		`name="preserve_acls" value="1">`,
		`name="preserve_xattrs" value="1">`,
		"Preserve sparse files (<code>-S</code>)",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the create form is missing %q", want)
		}
	}
	if strings.Contains(page, `name="preserve_hardlinks" value="1" checked`) {
		t.Error("the create form defaults a hardlink/ACL/xattr setting to on")
	}
}
