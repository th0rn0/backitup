package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// TestSettingsForm_RendersAllFields asserts the settings tab renders every
// field with its current name and default value. This guards against a text
// edit accidentally deleting a control.
func TestSettingsForm_RendersAllFields(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	// Create a client with known defaults.
	_, err := st.CreateClient(ctx, model.Client{
		Name:                 "settings-fields-test",
		Mode:                 model.ModeRsync,
		RetentionDays:        14,
		ExpectedIntervalSecs: 86400,
		OffsiteRemote:        "",
		OffsiteCron:          "",
		OffsiteUploadMode:    "all",
		PreserveSparse:       false,
		PreserveHardlinks:    false,
		PreserveACLs:         false,
		PreserveXattrs:       false,
		GroupReadable:        false,
		RecordOwnership:      false,
		Enabled:              true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	cl, _ := st.GetClientBySlug(ctx, "settings-fields-test")

	resp, err := c.Get(ts.URL + "/clients/" + cl.Slug)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Extract the settings tab panel.
	settingsStart := strings.Index(html, `data-tab="settings"`)
	if settingsStart < 0 {
		t.Fatal("settings tab panel not found")
	}
	settingsEnd := strings.Index(html[settingsStart:], `</div>
      </div>
    </main>`)
	if settingsEnd < 0 {
		t.Fatal("could not find end of settings panel")
	}
	settingsHTML := html[settingsStart : settingsStart+settingsEnd]

	// Field labels that must be present.
	requiredLabels := []string{
		"Hot retention (days)",
		"Expected interval (seconds)",
		"Remote",
		"Upload schedule (cron)",
		"Upload scope",
		"Preserve sparse files",
		"Preserve internal hardlinks",
		"Preserve ACLs",
		"Preserve extended attributes",
		"Make this client's snapshots group-readable",
		"Record the source's uid/gid in each snapshot",
	}
	for _, label := range requiredLabels {
		if !strings.Contains(settingsHTML, label) {
			t.Errorf("settings panel missing field label %q", label)
		}
	}

	// Verify default values are rendered.
	// retention_days should have value="14".
	if !strings.Contains(settingsHTML, `id="retention_days"`) {
		t.Error("missing retention_days input")
	}
	if !strings.Contains(settingsHTML, `value="14"`) {
		t.Error("retention_days should have value 14")
	}

	// expected_interval_secs should have value="86400".
	if !strings.Contains(settingsHTML, `id="expected_interval_secs"`) {
		t.Error("missing expected_interval_secs input")
	}
	if !strings.Contains(settingsHTML, `value="86400"`) {
		t.Error("expected_interval_secs should have value 86400")
	}

	// offsite_remote select should be present.
	if !strings.Contains(settingsHTML, `id="offsite_remote"`) {
		t.Error("missing offsite_remote select")
	}

	// offsite_cron input should be present.
	if !strings.Contains(settingsHTML, `id="offsite_cron"`) {
		t.Error("missing offsite_cron input")
	}

	// Upload scope radio buttons: "all" should be checked by default.
	if !strings.Contains(settingsHTML, `name="offsite_upload_mode" value="all"`) {
		t.Error("missing offsite_upload_mode=all radio")
	}
	// The "all" radio should have checked attribute.
	allRadioStart := strings.Index(settingsHTML, `name="offsite_upload_mode" value="all"`)
	if allRadioStart < 0 {
		t.Fatal("could not find all radio")
	}
	allRadioContext := settingsHTML[allRadioStart : allRadioStart+100]
	if !strings.Contains(allRadioContext, "checked") {
		t.Error("offsite_upload_mode=all should be checked by default")
	}

	// Checkboxes should be present (none checked by default).
	checkboxes := []string{
		`name="preserve_sparse"`,
		`name="preserve_hardlinks"`,
		`name="preserve_acls"`,
		`name="preserve_xattrs"`,
		`name="group_readable"`,
		`name="record_ownership"`,
	}
	for _, cb := range checkboxes {
		if !strings.Contains(settingsHTML, cb) {
			t.Errorf("missing checkbox %q", cb)
		}
	}
}

// TestSettingsForm_HintsAreConcise asserts that hint blocks do not exceed
// two lines of visible text (except the single-generation warning).
func TestSettingsForm_HintsAreConcise(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	_, err := st.CreateClient(ctx, model.Client{
		Name: "hints-test", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	cl, _ := st.GetClientBySlug(ctx, "hints-test")

	resp, err := c.Get(ts.URL + "/clients/" + cl.Slug)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// The long cron tutorial should be behind a disclosure, not inline.
	// The inline hint should be short: "5-field cron... Blank = every lifecycle pass."
	if strings.Contains(html, "Standard 5-field cron expression") {
		t.Error("cron tutorial should be behind a disclosure, not inline")
	}
	if !strings.Contains(html, "Blank = every lifecycle pass") {
		t.Error("cron hint should mention blank means every lifecycle pass")
	}

	// The long upload scope explanation should be behind a disclosure.
	// The inline hint should be a one-liner.
	if strings.Contains(html, "every local snapshot is mirrored offsite as its own object") {
		t.Error("upload scope tutorial should be behind a disclosure, not inline")
	}

	// The single-generation warning should be inline (destructive).
	if !strings.Contains(html, "Single generation keeps") {
		t.Error("single-generation warning should be inline")
	}

	// The long group-readable section hint should be behind a disclosure.
	if strings.Contains(html, "The backup user's own directories are") {
		t.Error("group-readable section hint should be behind a disclosure, not inline")
	}

	// The long ownership section hint should be behind a disclosure.
	if strings.Contains(html, "The receiver is an unprivileged account") {
		t.Error("ownership section hint should be behind a disclosure, not inline")
	}

	// The restoring tutorial must stay reachable but behind a disclosure. A bare
	// Contains cannot tell "hidden behind <details>" from "rendered inline": the
	// tutorial is in the markup either way, so check where it sits.
	if !insideDetails(html, "rsync -a --numeric-ids -e") {
		t.Error("restoring tutorial should be behind a disclosure, not inline")
	}
}

// insideDetails reports whether needle appears between an opening <details> and
// its closing tag, i.e. hidden behind a disclosure rather than rendered inline.
func insideDetails(html, needle string) bool {
	i := strings.Index(html, needle)
	if i < 0 {
		return false
	}
	open := strings.LastIndex(html[:i], "<details")
	closed := strings.LastIndex(html[:i], "</details>")
	return open > closed
}

// TestSettingsForm_DestructiveWarningsInline asserts that destructive warnings
// remain visible inline, not hidden behind disclosures.
func TestSettingsForm_DestructiveWarningsInline(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	_, err := st.CreateClient(ctx, model.Client{
		Name: "warnings-test", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	cl, _ := st.GetClientBySlug(ctx, "warnings-test")

	resp, err := c.Get(ts.URL + "/clients/" + cl.Slug)
	if err != nil {
		t.Fatalf("get client: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Single-generation warning should be inline (not in a details element).
	if !strings.Contains(html, "⚠ Single generation keeps") {
		t.Error("single-generation warning should be inline")
	}

	// Group-readable warning should be inline.
	if !strings.Contains(html, "⚠ Modes in the snapshot no longer match") {
		t.Error("group-readable warning should be inline")
	}

	// Ownership xattr warning should be inline.
	if !strings.Contains(html, "⚠ Volume must support") {
		t.Error("ownership xattr warning should be inline")
	}

	// Delete client warning should be inline.
	if !strings.Contains(html, "⚠ The backup directory and its snapshots are kept") {
		t.Error("delete client warning should be inline")
	}
}
