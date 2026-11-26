package server

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// The three upload modes are three different storage behaviours, so the form
// must persist "single" as posted — a handler that coerced it back to "all"
// (as it did for anything that was not "latest") would quietly mirror every
// snapshot of a client that asked for one object — and it must still fall back
// to "all" for a value the form never offers.
func TestOffsiteFormPersistsSingleGenerationMode(t *testing.T) {
	st, _, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	resp, err := c.PostForm(ts.URL+"/clients", url.Values{"name": {"laptop"}, "mode": {"targz"}})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	resp.Body.Close()

	save := func(mode string) {
		t.Helper()
		resp, err := c.PostForm(ts.URL+"/clients/laptop/offsite", url.Values{
			"offsite_remote":      {""},
			"offsite_dir":         {"laptop"},
			"offsite_cron":        {"0 2 * * *"},
			"offsite_upload_mode": {mode},
		})
		if err != nil {
			t.Fatalf("save offsite form (%q): %v", mode, err)
		}
		resp.Body.Close()
		// The handler answers 303 and this client follows it to the client page;
		// either way a refusal (400/500) has to fail the test rather than be
		// mistaken for a save.
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("save offsite form (%q) = %d, want a save (200 after the 303 redirect, or 303)", mode, resp.StatusCode)
		}
	}

	save(model.OffsiteUploadSingle)
	cl, err := st.GetClientBySlug(ctx, "laptop")
	if err != nil || cl == nil {
		t.Fatalf("load client: %v (client=%v)", err, cl)
	}
	if cl.OffsiteUploadMode != model.OffsiteUploadSingle {
		t.Fatalf("upload mode after saving single = %q, want %q", cl.OffsiteUploadMode, model.OffsiteUploadSingle)
	}

	// The page must say what the mode costs, next to the selector rather than in
	// a footnote, and mark it as the stored choice.
	page, err := c.Get(ts.URL + "/clients/laptop")
	if err != nil {
		t.Fatalf("get client page: %v", err)
	}
	body, _ := io.ReadAll(page.Body)
	page.Body.Close()
	html := string(body)
	for _, want := range []string{
		`value="single" checked`,
		"no point-in-time restore",
		"a bad backup replaces a good one",
		"latest.tar.gz",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the offsite settings do not say %q:\n%s", want, offsiteSettings(html))
		}
	}

	// A value the form does not offer is still stored as the default, not
	// verbatim: the column is read by the uploader, so it may only hold a mode
	// this build implements.
	save("single-generation")
	if cl, _ = st.GetClientBySlug(ctx, "laptop"); cl.OffsiteUploadMode != model.OffsiteUploadAll {
		t.Fatalf("an unknown upload mode was stored as %q, want %q", cl.OffsiteUploadMode, model.OffsiteUploadAll)
	}
}

// offsiteSettings narrows a rendered page to the offsite settings form, so a
// failure message shows the block that has to change rather than the whole page.
func offsiteSettings(html string) string {
	i := strings.Index(html, "Offsite destination")
	if i < 0 {
		return "(the page has no offsite settings block at all)"
	}
	end := strings.Index(html[i:], "</form>")
	if end < 0 {
		return html[i:]
	}
	return html[i : i+end+len("</form>")]
}
