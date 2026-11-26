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

// newPrefixStack creates a logged-in stack with one targz client called
// "laptop", which is the client every case below names objects after.
func newPrefixStack(t *testing.T) (*prefixFixture, func()) {
	t.Helper()
	st, _, ts := hardeningStack(t)
	c := loggedInClient(t, st, ts)
	resp, err := c.PostForm(ts.URL+"/clients", url.Values{"name": {"laptop"}, "mode": {"targz"}})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	resp.Body.Close()
	return &prefixFixture{st: st, c: c, url: ts.URL, ctx: context.Background()}, func() {}
}

type prefixFixture struct {
	st interface {
		GetClientBySlug(context.Context, string) (*model.Client, error)
	}
	c   *http.Client
	url string
	ctx context.Context
}

// save posts the offsite form with the given prefix controls and returns the
// page the browser lands on.
func (f *prefixFixture) save(t *testing.T, text string, none bool) (status int, page string) {
	t.Helper()
	form := url.Values{
		"offsite_remote":      {""},
		"offsite_upload_mode": {model.OffsiteUploadAll},
		"name_prefix":         {text},
	}
	if none {
		form.Set("name_prefix_none", "1")
	}
	resp, err := f.c.PostForm(f.url+"/clients/laptop/offsite", form)
	if err != nil {
		t.Fatalf("save offsite form: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func (f *prefixFixture) client(t *testing.T) *model.Client {
	t.Helper()
	c, err := f.st.GetClientBySlug(f.ctx, "laptop")
	if err != nil || c == nil {
		t.Fatalf("load client: %v (client=%v)", err, c)
	}
	return c
}

// The three states have to survive a round trip through the form, because that
// is the only place an operator can express "none of these": a text field alone
// cannot tell "blank, use the default" from "blank, write no prefix".
func TestOffsiteFormStoresTheBackupNamePrefix(t *testing.T) {
	f, done := newPrefixStack(t)
	defer done()

	// A client that has chosen nothing.
	if got := f.client(t); got.NamePrefixSet {
		t.Fatalf("a new client already has a chosen prefix %q", got.NamePrefix)
	}

	if status, page := f.save(t, "nas", false); status != http.StatusOK && status != http.StatusSeeOther {
		t.Fatalf("save prefix = %d, want a save:\n%s", status, page)
	}
	got := f.client(t)
	if !got.NamePrefixSet || got.NamePrefix != "nas" {
		t.Fatalf("prefix = %q (set=%v), want nas (set)", got.NamePrefix, got.NamePrefixSet)
	}

	// "No prefix" is the checkbox, and it must beat whatever is left in the box.
	if status, page := f.save(t, "nas", true); status != http.StatusOK && status != http.StatusSeeOther {
		t.Fatalf("save no-prefix = %d, want a save:\n%s", status, page)
	}
	got = f.client(t)
	if !got.NamePrefixSet || got.NamePrefix != "" {
		t.Fatalf("prefix = %q (set=%v), want the chosen, empty \"no prefix\"", got.NamePrefix, got.NamePrefixSet)
	}

	// A blank box with the checkbox clear means "nobody chose one", which is a
	// different setting from the one above.
	if status, page := f.save(t, "  ", false); status != http.StatusOK && status != http.StatusSeeOther {
		t.Fatalf("save blank = %d, want a save:\n%s", status, page)
	}
	if got = f.client(t); got.NamePrefixSet {
		t.Fatalf("a blank box with no box ticked stored a choice %q", got.NamePrefix)
	}
}

// A refused prefix must name the offender on the page. This is the form half of
// the rule; model.InvalidNamePrefix is the other, and both must refuse.
func TestOffsiteFormRefusesAnUnsafeNamePrefix(t *testing.T) {
	f, done := newPrefixStack(t)
	defer done()

	for _, bad := range []string{"bad/prefix", "..", "../escape", ".hidden", "two words", "UPPER"} {
		t.Run(bad, func(t *testing.T) {
			status, page := f.save(t, bad, false)
			if status != http.StatusOK {
				t.Fatalf("a refused prefix answered %d, want the client page with the reason (200 after the 303)", status)
			}
			if !strings.Contains(page, "backup name prefix refused") {
				t.Errorf("the page does not say the prefix was refused:\n%s", truncate([]byte(page), 2000))
			}
			if !strings.Contains(page, bad) {
				t.Errorf("the refusal does not name the offending value %q:\n%s", bad, truncate([]byte(page), 2000))
			}
			// Nothing was stored: the client still has no chosen prefix.
			if got := f.client(t); got.NamePrefixSet {
				t.Errorf("a refused prefix was stored anyway: %q", got.NamePrefix)
			}
		})
	}

	// The control: a valid prefix goes through, so the refusals above are not
	// "the form refuses everything".
	if status, page := f.save(t, "nas-01", false); status != http.StatusOK {
		t.Fatalf("a valid prefix was refused (%d):\n%s", status, truncate([]byte(page), 2000))
	}
	if got := f.client(t); !got.NamePrefixSet || got.NamePrefix != "nas-01" {
		t.Fatalf("prefix = %q (set=%v), want nas-01", got.NamePrefix, got.NamePrefixSet)
	}
}

// The setting has to be visible where it is set: the field, what blank means, and
// an example of the object name it produces — built by the same function the
// uploader uses, so the page cannot promise a name nothing writes.
func TestClientPageShowsTheNamePrefixFieldAndExample(t *testing.T) {
	f, done := newPrefixStack(t)
	defer done()

	resp, err := f.c.Get(f.url + "/clients/laptop")
	if err != nil {
		t.Fatalf("get client page: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	html := string(body)

	for _, want := range []string{
		`for="name_prefix"`, // the field's label points at it
		`id="name_prefix"`,
		`name="name_prefix"`,
		"Backup name prefix",
		// blank = <slug>
		`blank = <code>laptop</code>`,
		// the override that means "the old names" is offered, not implied
		`name="name_prefix_none"`,
		"No prefix",
		// a rendered example of the resulting object name
		`id="offsite-prefix-example"`,
		// The example is the real name for this client's mode: a tar.gz client's
		// object is the snapshot id itself, prefixed with its slug.
		">laptop-20260101T000000Z<",
		// and the unprefixed name the example is built from, so the live update
		// in the browser moves the prefix without re-deriving the rule
		`data-legacy="20260101T000000Z"`,
		`data-fallback="laptop"`,
		`data-dir="laptop"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("the settings tab does not contain %q:\n%s", want, offsiteSettings(html))
		}
	}
}
