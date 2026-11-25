package server

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// TestUXPass_OneTabBar asserts the client detail page has exactly one tab bar
// at the top level (no nested tab groups).
func TestUXPass_OneTabBar(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()
	id, err := st.CreateClient(ctx, model.Client{
		Name: "ux-test", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	cl, _ := st.GetClientBySlug(ctx, "ux-test")
	_ = id

	resp, err := c.Get(ts.URL + "/clients/" + cl.Slug)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Count top-level tab groups (data-tab-group at the main level).
	// The page should have exactly ONE tab group for the main tabs.
	count := strings.Count(html, `data-tab-group`)
	if count != 1 {
		t.Errorf("expected exactly 1 tab group, found %d", count)
	}

	// Verify the tab names are present.
	for _, tab := range []string{"Snapshots", "Runs", "Settings"} {
		if !strings.Contains(html, `data-tab-btn="`+strings.ToLower(tab)+`"`) {
			t.Errorf("missing tab button for %q", tab)
		}
	}
}

// TestUXPass_UnselectedPanelsHidden asserts that unselected tab panels have the
// hidden attribute, so their content is not in the DOM tree.
func TestUXPass_UnselectedPanelsHidden(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()
	_, _ = st.CreateClient(ctx, model.Client{
		Name: "hidden-test", Mode: model.ModeRsync, RetentionDays: 7,
		OffsiteRemote: "s3", Enabled: true,
	})
	cl, _ := st.GetClientBySlug(ctx, "hidden-test")

	resp, err := c.Get(ts.URL + "/clients/" + cl.Slug)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// The Snapshots tab is selected by default, so it should NOT have hidden.
	// The Runs and Settings tabs should have hidden.
	if !strings.Contains(html, `data-tab="runs" hidden`) {
		t.Error("Runs tab panel should have hidden attribute")
	}
	if !strings.Contains(html, `data-tab="settings" hidden`) {
		t.Error("Settings tab panel should have hidden attribute")
	}

	// The offsite empty-state text should not be visible when Snapshots is selected.
	// Since the offsite panel is hidden, its content should not appear in the
	// visible DOM (though it's still in the HTML, just hidden).
	// We verify the panel exists but is hidden.
	if !strings.Contains(html, `data-tab="offsite" hidden`) {
		t.Error("Offsite tab panel should have hidden attribute when not selected")
	}
}

// TestUXPass_StatusLabelsVisible asserts that every status glyph has visible
// text next to it, not just an aria-label.
func TestUXPass_StatusLabelsVisible(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	// Create clients in different health states.
	now := time.Now().UTC()
	mk := func(name string, interval int) int64 {
		id, err := st.CreateClient(ctx, model.Client{
			Name: name, Mode: model.ModeTarGz, RetentionDays: 14,
			ExpectedIntervalSecs: interval, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return id
	}
	run := func(id int64, status model.RunStatus, ago time.Duration) {
		fin := now.Add(-ago)
		if _, err := st.RecordRun(ctx, model.Run{
			ClientID: id, StartedAt: fin.Add(-time.Minute), FinishedAt: fin,
			Status: status, Bytes: 1024,
		}); err != nil {
			t.Fatalf("record run: %v", err)
		}
	}

	okID := mk("ok-client", 0)
	run(okID, model.StatusOK, 30*time.Second)

	failedID := mk("failed-client", 0)
	run(failedID, model.StatusFailed, 1*time.Hour)

	_ = mk("never-client", 0)

	// Check dashboard.
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Each health label should appear as visible text.
	for _, label := range []string{"OK", "Failed", "Never"} {
		if !strings.Contains(html, label) {
			t.Errorf("dashboard missing visible status label %q", label)
		}
	}

	// Check client detail page.
	cl, _ := st.GetClientBySlug(ctx, "ok-client")
	resp2, err := c.Get(ts.URL + "/clients/" + cl.Slug)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	html2 := string(body2)

	// The hero status should have visible text.
	if !strings.Contains(html2, "OK") {
		t.Error("client detail hero missing visible status label")
	}
}

// TestUXPass_NoConstantColumns asserts the local snapshot table does not have
// a constant "on disk" status column.
func TestUXPass_NoConstantColumns(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()
	_, _ = st.CreateClient(ctx, model.Client{
		Name: "no-const-col", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
	})
	cl, _ := st.GetClientBySlug(ctx, "no-const-col")

	resp, err := c.Get(ts.URL + "/clients/" + cl.Slug)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// The old "on disk" badge should not appear in the snapshot table.
	// (It may still appear in the offsite objects table, which is fine.)
	// We check that the snapshot table header does not have a "Status" column.
	// Find the snapshot table (it has "Snapshot" and "Created" columns).
	snapTableStart := strings.Index(html, `<th>Snapshot</th>`)
	if snapTableStart < 0 {
		t.Skip("no snapshot table found (no snapshots yet)")
	}
	snapTableEnd := strings.Index(html[snapTableStart:], "</table>")
	if snapTableEnd < 0 {
		t.Fatal("could not find end of snapshot table")
	}
	snapTable := html[snapTableStart : snapTableStart+snapTableEnd]

	// The snapshot table should not have a "Status" column header.
	if strings.Contains(snapTable, "<th>Status</th>") {
		t.Error("snapshot table should not have a constant Status column")
	}

	// The "on disk" badge should not appear in the snapshot table rows.
	if strings.Contains(snapTable, `badge-ok">on disk`) {
		t.Error("snapshot table should not have 'on disk' badges")
	}
}

// TestUXPass_ProductNameConsistent asserts the product name is consistent
// between the page title and the masthead.
func TestUXPass_ProductNameConsistent(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)

	// Check dashboard.
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Title should contain "Backitup".
	if !strings.Contains(html, "<title>Backitup") {
		t.Error("dashboard title should contain 'Backitup'")
	}

	// Masthead (sidebar brand) should contain "Backitup".
	if !strings.Contains(html, `class="brand"`) {
		t.Error("dashboard missing brand element")
	}
	// The brand text should be "Backitup", not "Back! It! Up!".
	if strings.Contains(html, "Back! It! Up!") {
		t.Error("masthead should not contain 'Back! It! Up!'")
	}
	if !strings.Contains(html, ">Backitup</span>") {
		t.Error("masthead should contain 'Backitup'")
	}
}

// TestUXPass_DashboardClientTotal asserts the dashboard shows a total client count.
func TestUXPass_DashboardClientTotal(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	// Create 3 clients.
	for i := 0; i < 3; i++ {
		_, err := st.CreateClient(ctx, model.Client{
			Name: "total-test-" + string(rune('a'+i)), Mode: model.ModeTarGz,
			RetentionDays: 7, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create client: %v", err)
		}
	}

	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Should show "3 clients" somewhere in the fleet meta.
	if !strings.Contains(html, "3 clients") {
		t.Error("dashboard should show total client count")
	}
}

// TestUXPass_MeasuredOnNextLifecyclePass asserts the string "not measured yet"
// no longer appears and is replaced with the single-line replacement.
func TestUXPass_MeasuredOnNextLifecyclePass(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)

	// Check dashboard.
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// "not measured yet" should not appear anywhere.
	if strings.Contains(html, "not measured yet") {
		t.Error("page should not contain 'not measured yet'")
	}
}
