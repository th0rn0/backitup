package server

import (
	"context"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// TestClientPageCarriesStatsAndTabs pins the structure the dark redesign
// introduced on a client page: the aggregate strip and stat cards above the tab
// bar, and the tab set itself.
//
// The Offsite tab is conditional on the client actually having a remote. A tab
// that opens on nothing is worse than no tab: the operator clicks it, sees an
// empty panel, and cannot tell whether the client has no offsite copies or the
// page failed to load them.
func TestClientPageCarriesStatsAndTabs(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "structure", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	body := getPage(t, c, ts.URL+"/clients/structure")

	if !strings.Contains(body, `class="aggr-strip"`) {
		t.Error("client page has no aggregate strip")
	}
	if n := strings.Count(body, `class="client-stat"`); n < 3 {
		t.Errorf("client page renders %d stat cards, want at least 3 (on disk, snapshots, newest)", n)
	}
	for _, tab := range []string{"snapshots", "runs", "settings"} {
		if !strings.Contains(body, `data-tab-btn="`+tab+`"`) {
			t.Errorf("client page is missing the %s tab", tab)
		}
	}
	if strings.Contains(body, `data-tab-btn="offsite"`) {
		t.Error("client page offers an Offsite tab for a client with no remote — it can only open on an empty panel")
	}

	// With a remote the tab belongs on the page. Create a second client with a
	// remote rather than mutating the first: the tab's condition is the stored
	// field, and a client created with a remote is the ordinary path.
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "structure-offsite", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
		OffsiteRemote: "s3",
	}); err != nil {
		t.Fatalf("create client with a remote: %v", err)
	}
	body = getPage(t, c, ts.URL+"/clients/structure-offsite")
	if !strings.Contains(body, `data-tab-btn="offsite"`) {
		t.Error("client page hides the Offsite tab even though the client has a remote")
	}
}

// TestSidebarOnEveryPage asserts the sidebar renders with its links on every
// page, not just the dashboard. A nav that exists only where you already are is
// the shape of a dead nav report.
//
// This is a structure assertion: it cannot prove the links are clickable, which
// needs a browser. It exists to catch the case where a template stops including
// the sidebar at all — the earlier single-page version of this check would have
// missed that on four of the five pages.
func TestSidebarOnEveryPage(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "sidebar", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	pages := []string{"/", "/clients/sidebar", "/clients/new", "/users", "/settings/remotes"}
	for _, path := range pages {
		body := getPage(t, c, ts.URL+path)
		if !strings.Contains(body, `class="sidebar"`) {
			t.Errorf("%s: no sidebar", path)
			continue
		}
		for _, href := range []string{`href="/"`, `href="/clients/new"`, `href="/users"`, `href="/settings/remotes"`} {
			if !strings.Contains(body, href) {
				t.Errorf("%s: sidebar missing %s", path, href)
			}
		}
	}
}
