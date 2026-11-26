package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// TestNoInlineStylesInAnyPage asserts no page ships a style attribute or an
// inline <style> element.
//
// The server sends `style-src 'self'`, and a CSP hash cannot whitelist a style
// attribute without 'unsafe-hashes', so an inline style is not "a bit sloppy":
// it is silently dropped by every browser and the page ships with the rule
// missing. That is what the browser console reported, and it is why the visual
// adjustments are classes now.
func TestNoInlineStylesInAnyPage(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()
	for _, cl := range []model.Client{
		{Name: "plain", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true},
		{Name: "with-remote", Mode: model.ModeRsync, RetentionDays: 7, Enabled: true, OffsiteRemote: "s3"},
	} {
		if _, err := st.CreateClient(ctx, cl); err != nil {
			t.Fatalf("create client %s: %v", cl.Name, err)
		}
	}

	pages := []string{"/", "/clients/plain", "/clients/with-remote", "/clients/new", "/users", "/settings/remotes", "/login"}
	for _, path := range pages {
		body := getPage(t, c, ts.URL+path)
		if n := strings.Count(body, `style="`); n > 0 {
			t.Errorf("%s: %d inline style attribute(s); the CSP drops them, move them to a class", path, n)
		}
		if strings.Contains(body, "<style") {
			t.Errorf("%s: inline <style> element; the CSP blocks it", path)
		}
	}
}

// TestStaticAssetsRevalidate asserts /static/ responses carry a validator and
// tell the browser to revalidate.
//
// Without both, a browser may keep running the previous build's JavaScript
// after a deploy, against templates that expect handlers the old script never
// bound. The page then looks like the buttons are dead.
func TestStaticAssetsRevalidate(t *testing.T) {
	_, ts := testStack(t)
	for _, asset := range []string{"/static/app.css", "/static/offsite-dir.js"} {
		resp, err := http.Get(ts.URL + asset)
		if err != nil {
			t.Fatalf("get %s: %v", asset, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", asset, resp.StatusCode)
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Errorf("%s: no ETag, so the browser has nothing to revalidate against", asset)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Errorf("%s: Cache-Control = %q, want no-cache so a deploy is picked up on the next load", asset, cc)
		}

		// The validator must actually work: a matching If-None-Match is a 304.
		req, _ := http.NewRequest(http.MethodGet, ts.URL+asset, nil)
		req.Header.Set("If-None-Match", etag)
		resp2, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("conditional get %s: %v", asset, err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusNotModified {
			t.Errorf("%s: conditional GET returned %d, want 304 (the asset is %d bytes)", asset, resp2.StatusCode, len(body))
		}
	}

	// A tampered validator must not produce a 304.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/static/app.css", nil)
	req.Header.Set("If-None-Match", `"stale-from-a-previous-build"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stale validator get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("stale ETag returned %d, want 200", resp.StatusCode)
	}
}
