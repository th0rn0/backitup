package model

import (
	"strings"
	"testing"
)

// A prefix is a NAME, never a path. The refusals below are the ones that would
// otherwise change what the object name addresses: a separator adds a directory
// component, ".." climbs out of the client's directory, a leading dot produces a
// hidden/traversal-ish name, and a space is something no shell, rclone flag or
// storage provider reads back the same way twice.
func TestInvalidNamePrefix(t *testing.T) {
	tests := []struct {
		prefix  string
		wantErr bool
	}{
		{"", false}, // no prefix: today's names
		{"laptop", false},
		{"nas-01", false},
		{"a", false},
		{strings.Repeat("a", 63), false},

		{"bad/prefix", true},
		{"..", true},
		{"../escape", true},
		{".hidden", true},
		{"two words", true},
		{" ends-with-space ", true},
		{"UPPER", true},
		{"under_score", true},
		{"dot.ted", true},
		{"-leading-hyphen", true},
		{strings.Repeat("a", 64), true},
		// A trailing hyphen is allowed by the slug rule (model.Slug trims it,
		// but the authkeys allowlist accepts it), and it is harmless in a name —
		// "<prefix>-<id>" then reads "x--<id>", which is still one component.
		{"trailing-hyphen-", false},
	}
	for _, tc := range tests {
		reason := InvalidNamePrefix(tc.prefix)
		if tc.wantErr && reason == "" {
			t.Errorf("InvalidNamePrefix(%q) accepted it; want a refusal", tc.prefix)
		}
		if !tc.wantErr && reason != "" {
			t.Errorf("InvalidNamePrefix(%q) = %q; want accepted", tc.prefix, reason)
		}
		if tc.wantErr && reason != "" {
			// The refusal has to name the offender: the operator typed it and
			// the page shows this string.
			if !strings.Contains(reason, tc.prefix) {
				t.Errorf("InvalidNamePrefix(%q) = %q; the message must name the offending value", tc.prefix, reason)
			}
		}
	}
}

// Three states, and the difference between the last two is a setting an
// operator can make: unset follows the fleet default and then the slug, while an
// empty-but-chosen prefix means "the old unprefixed names".
func TestNamePrefixResolution(t *testing.T) {
	unset := Client{Slug: "laptop"}
	if got := NamePrefix(unset, ""); got != "laptop" {
		t.Errorf("NamePrefix(unset, no fleet default) = %q, want the slug %q", got, "laptop")
	}
	if got := NamePrefix(unset, "fleet"); got != "fleet" {
		t.Errorf("NamePrefix(unset, fleet) = %q, want %q", got, "fleet")
	}

	chosen := unset
	chosen.NamePrefix, chosen.NamePrefixSet = "nas", true
	if got := NamePrefix(chosen, "fleet"); got != "nas" {
		t.Errorf("NamePrefix(chosen, fleet) = %q; the client's own value must win", got)
	}

	none := unset
	none.NamePrefixSet = true
	if got := NamePrefix(none, "fleet"); got != "" {
		t.Errorf("NamePrefix(explicitly none, fleet) = %q; an operator who asked for no prefix must get none", got)
	}
}
