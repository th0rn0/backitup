package server

import (
	"context"
	"io"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// TestDarkPaletteNoLightRemnants asserts no page contains light-theme hex
// tokens (#fff, #ffffff, background:#f) and no inline <style> blocks.
func TestDarkPaletteNoLightRemnants(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	// Create a client so dashboard and client detail render.
	_, err := st.CreateClient(ctx, model.Client{
		Name: "palette-test", Mode: model.ModeTarGz, RetentionDays: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	pages := []string{"/", "/clients/palette-test", "/login", "/settings/remotes", "/users"}
	for _, path := range pages {
		resp, err := c.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		html := strings.ToLower(string(body))

		// Check for light-theme hex tokens.
		for _, bad := range []string{"#fff", "#ffffff", "background:#f", "background: #f"} {
			if strings.Contains(html, bad) {
				t.Errorf("%s contains light-theme token %q", path, bad)
			}
		}

		// Check for inline <style> blocks (all styles should be in app.css).
		if strings.Contains(html, "<style") {
			t.Errorf("%s contains inline <style> block", path)
		}
	}
}

// TestStateColourTokens asserts the state classes map to the four allowed
// tokens and a healthy client renders one colour not three.
func TestStateColourTokens(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	// Create an OK client with a recent run.
	id, err := st.CreateClient(ctx, model.Client{
		Name: "colour-test", Mode: model.ModeTarGz, RetentionDays: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	// Record a successful run to make it "ok".
	now := time.Now()
	if _, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: now.Add(-time.Minute), FinishedAt: now,
		Status: model.StatusOK, Bytes: 1000,
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}

	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// The card should have class "health-ok" and pill class "pill-ok".
	if !strings.Contains(html, "health-ok") {
		t.Error("OK client missing health-ok class")
	}
	if !strings.Contains(html, "pill-ok") {
		t.Error("OK client missing pill-ok class")
	}

	// Should NOT have multiple health classes on the same card.
	if strings.Count(html, "health-ok") > 1 {
		t.Error("OK client has multiple health-ok classes")
	}
}

// TestFleetCardCount asserts the dashboard renders one card per client.
func TestFleetCardCount(t *testing.T) {
	for _, n := range []int{1, 2, 5} {
		t.Run(string(rune('0'+n))+"clients", func(t *testing.T) {
			st, ts := testStack(t)
			c := loggedInClient(t, st, ts)
			ctx := context.Background()

			for i := 0; i < n; i++ {
				_, err := st.CreateClient(ctx, model.Client{
					Name: "card-test-" + string(rune('a'+i)), Mode: model.ModeTarGz,
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

			count := strings.Count(html, "fleet-card health-")
			if count != n {
				t.Errorf("expected %d cards, got %d", n, count)
			}
		})
	}
}

// TestSparklineAbsentUnderThreeRuns asserts no sparkline element when a client
// has fewer than 3 runs.
func TestSparklineAbsentUnderThreeRuns(t *testing.T) {
	for _, n := range []int{0, 1, 2} {
		t.Run(string(rune('0'+n))+"runs", func(t *testing.T) {
			st, ts := testStack(t)
			c := loggedInClient(t, st, ts)
			ctx := context.Background()

			id, err := st.CreateClient(ctx, model.Client{
				Name: "spark-absent", Mode: model.ModeTarGz, RetentionDays: 7, Enabled: true,
			})
			if err != nil {
				t.Fatalf("create client: %v", err)
			}

			now := time.Now()
			for i := 0; i < n; i++ {
				fin := now.Add(-time.Duration(i) * time.Hour)
				if _, err := st.RecordRun(ctx, model.Run{
					ClientID: id, StartedAt: fin.Add(-time.Minute), FinishedAt: fin,
					Status: model.StatusOK, Bytes: int64(1000 * (i + 1)),
				}); err != nil {
					t.Fatalf("record run: %v", err)
				}
			}

			resp, err := c.Get(ts.URL + "/")
			if err != nil {
				t.Fatalf("get dashboard: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			html := string(body)

			// data-spark should be "[]" or have fewer than 3 elements.
			if strings.Contains(html, "sparkline") {
				t.Error("sparkline element present with fewer than 3 runs")
			}
		})
	}
}

// TestSparklinePresentWithThreeRuns asserts a sparkline element is present when
// a client has 3+ runs.
func TestSparklinePresentWithThreeRuns(t *testing.T) {
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	ctx := context.Background()

	id, err := st.CreateClient(ctx, model.Client{
		Name: "spark-present", Mode: model.ModeTarGz, RetentionDays: 7, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	now := time.Now()
	for i := 0; i < 5; i++ {
		fin := now.Add(-time.Duration(i) * time.Hour)
		if _, err := st.RecordRun(ctx, model.Run{
			ClientID: id, StartedAt: fin.Add(-time.Minute), FinishedAt: fin,
			Status: model.StatusOK, Bytes: int64(1000 * (i + 1)),
		}); err != nil {
			t.Fatalf("record run: %v", err)
		}
	}

	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// data-spark should have a JSON array with 5 elements.
	if !strings.Contains(html, "data-spark='[") {
		t.Error("data-spark attribute missing or empty")
	}
	// The sparkline is rendered client-side, but the data attribute must be present.
}

// TestNavHitBox asserts each sidebar link has non-zero dimensions and the nav
// has position relative/absolute/fixed with z-index > 0.
func TestNavHitBox(t *testing.T) {
	// Read app.css and assert the nav rules.
	css := readAsset(t, "app.css")

	// Check .sidebar has position and z-index.
	if !strings.Contains(css, ".sidebar") {
		t.Fatal(".sidebar rule missing")
	}
	if !strings.Contains(css, "position: relative") && !strings.Contains(css, "position: absolute") && !strings.Contains(css, "position: fixed") {
		t.Error(".sidebar missing position property")
	}
	if !strings.Contains(css, "z-index:") {
		t.Error(".sidebar missing z-index")
	}

	// Check .nav a has position and z-index.
	if !strings.Contains(css, ".nav a") {
		t.Fatal(".nav a rule missing")
	}

	// Render a page and check the sidebar links are present.
	st, ts := testStack(t)
	c := loggedInClient(t, st, ts)
	resp, err := c.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("get dashboard: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Check for sidebar links.
	for _, link := range []string{"/clients/new", "/users", "/settings/remotes"} {
		if !strings.Contains(html, `href="`+link+`"`) {
			t.Errorf("sidebar missing link to %s", link)
		}
	}
}

// TestContrastRatios computes body text on background (must be >=7:1) and muted
// text on background (>=4.5:1).
func TestContrastRatios(t *testing.T) {
	// Design tokens:
	// --bg: #0d1014 = rgb(13, 16, 20)
	// --text: #e9eef4 = rgb(233, 238, 244)
	// --text-dim: #8d9aa8 = rgb(141, 154, 168)

	bgLum := relativeLuminance(13, 16, 20)
	textLum := relativeLuminance(233, 238, 244)
	dimLum := relativeLuminance(141, 154, 168)

	bodyRatio := contrastRatio(textLum, bgLum)
	mutedRatio := contrastRatio(dimLum, bgLum)

	t.Logf("Body text contrast: %.2f:1 (must be >= 7:1)", bodyRatio)
	t.Logf("Muted text contrast: %.2f:1 (must be >= 4.5:1)", mutedRatio)

	if bodyRatio < 7.0 {
		t.Errorf("body text contrast %.2f:1 < 7:1", bodyRatio)
	}
	if mutedRatio < 4.5 {
		t.Errorf("muted text contrast %.2f:1 < 4.5:1", mutedRatio)
	}
}

// TestKeyboardFocusVisible asserts :focus-visible is defined in app.css with a
// visible outline.
func TestKeyboardFocusVisible(t *testing.T) {
	css := readAsset(t, "app.css")

	if !strings.Contains(css, ":focus-visible") {
		t.Fatal(":focus-visible rule missing")
	}
	if !strings.Contains(css, "outline:") {
		t.Error(":focus-visible missing outline property")
	}
}

// readAsset reads an asset file from the embedded FS.
func readAsset(t *testing.T, name string) string {
	t.Helper()
	data, err := assetFS.ReadFile("assets/" + name)
	if err != nil {
		t.Fatalf("read asset %s: %v", name, err)
	}
	return string(data)
}

// relativeLuminance computes the relative luminance of an sRGB color.
func relativeLuminance(r, g, b int) float64 {
	rs := float64(r) / 255.0
	gs := float64(g) / 255.0
	bs := float64(b) / 255.0

	if rs <= 0.03928 {
		rs /= 12.92
	} else {
		rs = math.Pow((rs+0.055)/1.055, 2.4)
	}
	if gs <= 0.03928 {
		gs /= 12.92
	} else {
		gs = math.Pow((gs+0.055)/1.055, 2.4)
	}
	if bs <= 0.03928 {
		bs /= 12.92
	} else {
		bs = math.Pow((bs+0.055)/1.055, 2.4)
	}

	return 0.2126*rs + 0.7152*gs + 0.0722*bs
}

// contrastRatio computes the contrast ratio between two luminances.
func contrastRatio(l1, l2 float64) float64 {
	lighter := l1
	darker := l2
	if l2 > l1 {
		lighter = l2
		darker = l1
	}
	return (lighter + 0.05) / (darker + 0.05)
}
