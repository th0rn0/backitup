package server

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// A database that still carries a remote of a backend this build dropped must
// open, show the row, and say what to do about it — not hide the row, not crash
// on the missing field list, and not offer an edit form that cannot work.
func TestUnsupportedRemoteShowsBannerAndIsNotEditable(t *testing.T) {
	st, ts := rcloneStack(t)
	c := loggedInClient(t, st, ts)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "legacy-s3", Backend: "s3", Unsupported: true,
		Config:    map[string]string{"bucket": "s3-bucket", "access_key_id": "AKIA"},
		CreatedAt: time.Now().Add(-24 * time.Hour),
	}); err != nil {
		t.Fatalf("create unsupported remote: %v", err)
	}

	page := getPage(t, c, ts.URL+"/settings/remotes")
	for _, want := range []string{
		"legacy-s3",
		"Unsupported remote type — recreate as B2",
		"<code>s3</code>",
		"the lifecycle skips it",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("the remotes page does not say %q about an unsupported remote:\n%s", want, page)
		}
	}
	// Nothing offers to use it: no test button, no edit link.
	for _, unwanted := range []string{
		"/settings/remotes/legacy-s3/test",
		"/settings/remotes/legacy-s3/edit",
	} {
		if strings.Contains(page, unwanted) {
			t.Errorf("the page still offers %s for an unsupported remote", unwanted)
		}
	}

	// The edit page opens (the row is visible and explainable) but renders no
	// form, because this build has no field list for that backend.
	edit := getPage(t, c, ts.URL+"/settings/remotes/legacy-s3/edit")
	if !strings.Contains(edit, "Unsupported remote type — recreate as B2") {
		t.Fatalf("the edit page does not explain why it cannot edit this remote:\n%s", edit)
	}
	if strings.Contains(edit, `action="/settings/remotes/legacy-s3/edit"`) {
		t.Error("the edit page still renders a form for an unsupported remote")
	}

	// And a hand-rolled POST is refused with the type named, not 404 or 500.
	nc := noRedirectClientWithSession(t, st, ts)
	loc := postNoRedirect(t, nc, ts.URL+"/settings/remotes/legacy-s3/edit",
		url.Values{"account": {"whatever"}})
	if !strings.Contains(loc, "err=") || !strings.Contains(loc, "unsupported remote type") {
		t.Fatalf("editing an unsupported remote = %q; want a refusal naming the unsupported type", loc)
	}
	// Refused means unchanged.
	again, err := st.GetRemoteByName(t.Context(), "legacy-s3")
	if err != nil || again == nil {
		t.Fatalf("load remote after the refused edit: %v (remote=%v)", err, again)
	}
	if again.Config["account"] != "" {
		t.Fatalf("the refused edit still changed the config: %+v", again.Config)
	}
}
