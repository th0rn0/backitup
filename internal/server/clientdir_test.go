package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// A client's directory comes from its persisted slug. clientDir is the one
// place that turns a slug into a path, so it is the one place that has to refuse
// a slug that is not a single plain component. The failure mode this guards is
// specific: returning the base directory for an empty slug puts every client in
// the same read-write directory, which is the confinement bug the slug column
// exists to fix.
func TestClientDirRejectsUnusableSlug(t *testing.T) {
	base := filepath.Join(strings.Repeat("x", 3), "backups")

	valid := []string{"a", "0", "alpha", "alpha-beta-2", strings.Repeat("a", model.MaxSlugLen)}
	for _, slug := range valid {
		c := model.Client{ID: 7, Name: "Client Seven", Slug: slug}
		got, err := clientDirIn(base, c)
		if err != nil {
			t.Errorf("slug %q: unexpected error: %v", slug, err)
			continue
		}
		if want := filepath.Join(base, slug); got != want {
			t.Errorf("slug %q: dir = %q, want %q", slug, got, want)
		}
	}

	invalid := []struct {
		name string
		slug string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dot-dot", ".."},
		{"slash", "a/b"},
		{"parent escape", "../other"},
		{"absolute", "/etc"},
		{"uppercase", "Alpha"},
		{"leading hyphen", "-alpha"},
		{"trailing newline", "alpha\nbravo"},
		{"forced-command injection", `a",no-pty,command="sh`},
		{"space", "alpha beta"},
		{"too long", strings.Repeat("a", model.MaxSlugLen+1)},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			c := model.Client{ID: 7, Name: "Client Seven", Slug: tc.slug}
			got, err := clientDirIn(base, c)
			if err == nil {
				t.Fatalf("slug %q: expected an error, got dir %q", tc.slug, got)
			}
			// The dangerous answer is the base directory itself, shared by
			// everyone. An error must never be paired with a usable path.
			if got != "" {
				t.Errorf("slug %q: error returned alongside dir %q, want empty", tc.slug, got)
			}
			if !strings.Contains(err.Error(), "unusable slug") {
				t.Errorf("slug %q: error %q does not say what is wrong", tc.slug, err)
			}
		})
	}
}

// s.clientDir is the form the handlers use; it must resolve against the
// server's own base directory and inherit the same refusal.
func TestServerClientDirUsesBaseDir(t *testing.T) {
	s := &Server{backupBaseDir: filepath.Join("srv", "backups")}

	got, err := s.clientDir(model.Client{ID: 1, Name: "one", Slug: "one"})
	if err != nil {
		t.Fatalf("valid slug: %v", err)
	}
	if want := filepath.Join("srv", "backups", "one"); got != want {
		t.Errorf("dir = %q, want %q", got, want)
	}

	got, err = s.clientDir(model.Client{ID: 2, Name: "two", Slug: ""})
	if err == nil {
		t.Fatalf("empty slug: expected an error, got %q", got)
	}
	if got != "" {
		t.Errorf("empty slug returned dir %q, want empty", got)
	}
}
