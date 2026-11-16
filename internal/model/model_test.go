package model

import (
	"regexp"
	"strings"
	"testing"
)

func TestModeValid(t *testing.T) {
	valid := []Mode{ModeTarGz, ModeRsync}
	for _, m := range valid {
		if !m.Valid() {
			t.Errorf("%q should be valid", m)
		}
	}
	invalid := []Mode{"", "zip", "rsyncc", "TARGZ"}
	for _, m := range invalid {
		if m.Valid() {
			t.Errorf("%q should be invalid", m)
		}
	}
}

// A slug becomes a directory name and part of an sshd forced command, so it is
// capped: the authkeys allowlist refuses anything longer than MaxSlugLen, and a
// refused slug means the client silently loses its SSH key.
func TestSlugIsCappedAndAlwaysAllowlisted(t *testing.T) {
	shape := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	for _, tc := range []struct {
		name string
		want string
	}{
		{"", ""},
		{"Laptop Docs", "laptop-docs"},
		{"  leading and trailing  ", "leading-and-trailing"},
		{strings.Repeat("a", MaxSlugLen), strings.Repeat("a", MaxSlugLen)},
		{strings.Repeat("a", MaxSlugLen+1), strings.Repeat("a", MaxSlugLen)},
		// The character at the cut is a hyphen: it must be dropped, not left
		// dangling, or the slug fails the allowlist.
		{strings.Repeat("a", MaxSlugLen-1) + "-tail", strings.Repeat("a", MaxSlugLen-1)},
		// Non-ASCII is not a directory component we accept; it slugifies away.
		{"ünïcode", "n-code"},
	} {
		got := Slug(tc.name)
		if got != tc.want {
			t.Errorf("Slug(%.20q…) = %q (len %d), want %q (len %d)", tc.name, got, len(got), tc.want, len(tc.want))
		}
		if len(got) > MaxSlugLen {
			t.Errorf("Slug(%.20q…) is %d characters, over the %d cap", tc.name, len(got), MaxSlugLen)
		}
		if got != "" && !shape.MatchString(got) {
			t.Errorf("Slug(%.20q…) = %q does not satisfy the authkeys allowlist", tc.name, got)
		}
	}
}
