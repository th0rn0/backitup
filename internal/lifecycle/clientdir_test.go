package lifecycle

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// Every lifecycle pass (prune, verify, offsite upload) starts by resolving the
// client's directory. They all go through Deps.clientDir now, so one bad row
// fails that client instead of sending three different passes at a path that is
// not the client's — most importantly at the base directory itself for an empty
// slug, which is every other client's parent.
func TestDepsClientDir(t *testing.T) {
	base := filepath.Join("srv", "backups")
	d := Deps{BackupBaseDir: base}

	got, err := d.clientDir(model.Client{ID: 3, Name: "three", Slug: "three"})
	if err != nil {
		t.Fatalf("valid slug: %v", err)
	}
	if want := filepath.Join(base, "three"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}

	for _, slug := range []string{"", "..", "a/b", `a",no-pty`, strings.Repeat("a", model.MaxSlugLen+1)} {
		got, err := d.clientDir(model.Client{ID: 4, Name: "four", Slug: slug})
		if err == nil {
			t.Errorf("slug %q: expected an error, got dir %q", slug, got)
			continue
		}
		if got != "" {
			t.Errorf("slug %q: error returned alongside dir %q, want empty", slug, got)
		}
		if got == base || got == filepath.Clean(base) {
			t.Errorf("slug %q: resolved to the shared base directory", slug)
		}
	}
}
