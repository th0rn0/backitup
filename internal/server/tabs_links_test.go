package server

import (
	"context"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// TestClientTabsWorkWithoutJavaScript pins the reason the tabs are links.
//
// They used to be <button> elements wired by offsite-dir.js, so in any browser
// that ran a stale, blocked or 404'd copy of that script the four tabs did
// nothing at all: the markup had the controls, nothing had bound them. A link
// with an href the server understands cannot fail that way, and it is also what
// makes a tab refreshable and shareable.
func TestClientTabsWorkWithoutJavaScript(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "tabs", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
		OffsiteRemote: "s3",
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}

	body := getPage(t, c, ts.URL+"/clients/tabs")
	for _, tab := range []string{"snapshots", "offsite", "runs", "settings"} {
		want := `href="?tab=` + tab + `"`
		if !strings.Contains(body, want) {
			t.Errorf("tab %q is not a link (%s missing): it needs JavaScript to work", tab, want)
		}
	}
	if strings.Contains(body, `<button class="tab-btn"`) {
		t.Error("a tab is still a <button>: it cannot work without the script")
	}

	// The server must render the requested panel, with the others hidden.
	for tab := range map[string]struct{}{"snapshots": {}, "offsite": {}, "runs": {}, "settings": {}} {
		page := getPage(t, c, ts.URL+"/clients/tabs?tab="+tab)
		if !strings.Contains(page, `data-tab="`+tab+`" role="tabpanel">`) {
			t.Errorf("?tab=%s: that panel is not rendered visible", tab)
		}
		for _, other := range []string{"snapshots", "offsite", "runs", "settings"} {
			if other == tab {
				continue
			}
			if !strings.Contains(page, `data-tab="`+other+`" role="tabpanel" hidden>`) {
				t.Errorf("?tab=%s: panel %q should be hidden on that page", tab, other)
			}
		}
		if !strings.Contains(page, `data-tab-btn="`+tab+`" role="tab" aria-selected="true"`) {
			t.Errorf("?tab=%s: that tab is not marked selected", tab)
		}
	}

	// An unknown tab falls back to the first panel rather than rendering a page
	// where every panel is hidden and nothing looks selected.
	page := getPage(t, c, ts.URL+"/clients/tabs?tab=nonsense")
	if !strings.Contains(page, `data-tab="snapshots" role="tabpanel">`) {
		t.Error("an unknown ?tab= did not fall back to the snapshots panel")
	}
}
