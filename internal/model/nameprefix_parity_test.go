package model_test

import (
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/authkeys"
	"github.com/th0rn0/backitup/internal/model"
)

// The prefix is validated with the SLUG rule — the card asks for exactly that,
// and the reason is the same for both values: a slug becomes a directory and a
// prefix becomes part of a name, so neither may contain a separator, "..", a
// leading dot or a space. Two definitions of "safe component" that drift apart
// is how a prefix eventually becomes a path, so this test pins the agreement
// instead of trusting it.
//
// It lives in the external test package because internal/authkeys imports
// model; the values and the assertion are the point, and they are duplicated
// from the schema's own regexp on purpose.
func TestNamePrefixUsesTheSlugRule(t *testing.T) {
	values := []string{
		"laptop", "nas-01", "a", "0", "all-the-things-2026",
		strings.Repeat("a", 63), strings.Repeat("a", 64),
		"bad/prefix", "/leading", "trailing/", "..", "../escape", ".hidden",
		"two words", "UPPER", "under_score", "dot.ted", "-leading-hyphen",
		"trailing-hyphen-", "a\tb", "a\nb", "café",
	}
	for _, v := range values {
		slugReason := authkeys.InvalidSlug(v)
		prefixReason := model.InvalidNamePrefix(v)
		if (slugReason == "") != (prefixReason == "") {
			t.Errorf("%q: slug says %q, prefix says %q — the two rules must agree",
				v, slugReason, prefixReason)
		}
	}
	// The one place they deliberately differ: an empty prefix is a valid
	// setting ("no prefix"), while an empty slug is the dangerous case (every
	// client sharing one directory).
	if r := model.InvalidNamePrefix(""); r != "" {
		t.Errorf("empty prefix refused with %q; it means \"no prefix\"", r)
	}
	if r := authkeys.InvalidSlug(""); r == "" {
		t.Error("empty slug accepted; it is the one value that must always be refused")
	}
}
