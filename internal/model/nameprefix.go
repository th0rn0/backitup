package model

import (
	"fmt"
	"regexp"
)

// NamePrefixDefaultEnv is the environment variable that supplies the fleet-wide
// default for the prefix every client's offsite object names carry. Empty means
// "use each client's slug", which is what a single-client install wants anyway.
//
// It is a DEFAULT, not an override: a client whose own name_prefix is set keeps
// it. The value is validated at startup with InvalidNamePrefix, because a prefix
// containing a separator or a leading dot would turn an object name into a path.
const NamePrefixDefaultEnv = "BACKITUP_NAME_PREFIX_DEFAULT"

// namePrefixPattern is a slug, and deliberately nothing more: an object name is
// "<prefix>-<snapshot id>", so a prefix containing "/" would add a directory
// component, ".." would climb out of the client's directory, and a leading dot
// would produce a hidden or traversal-ish name. Lowercase ASCII, digits and
// hyphens are what a slug is, and they are also what cannot be misread by a
// shell, rclone or the storage provider.
var namePrefixPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// InvalidNamePrefix returns a non-empty reason if prefix cannot be the leading
// part of an object's file name. The empty prefix is valid and means "no
// prefix" — the names backitup wrote before this setting existed.
//
// The rule is the slug rule (model.Slug / authkeys.InvalidSlug): a prefix is a
// NAME, never a path, so it must not be able to change the directory component
// of the object it decorates.
func InvalidNamePrefix(prefix string) string {
	if prefix == "" {
		return ""
	}
	if !namePrefixPattern.MatchString(prefix) {
		return fmt.Sprintf("unsafe backup name prefix %q: want ^[a-z0-9][a-z0-9-]{0,62}$ "+
			"(lowercase letters, digits and hyphens — no '/', no '..', no leading dot, no spaces)", prefix)
	}
	return ""
}

// NamePrefix resolves the prefix one client's offsite object names carry.
//
// Three states, and the difference between the last two is the whole point:
//
//   - the client has a prefix of its own (NamePrefixSet) — use it, including
//     when it is "" (the operator asked for the old, unprefixed names);
//   - no prefix chosen — the fleet default (BACKITUP_NAME_PREFIX_DEFAULT);
//   - no fleet default either — the client's slug, which is what makes a file
//     found in the bucket identifiable without the database.
//
// Unlike Slug, this value is never joined onto a path: it is a name component,
// and the directory stays offsiteDir(c) whatever it says.
func NamePrefix(c Client, fleetDefault string) string {
	if c.NamePrefixSet {
		return c.NamePrefix
	}
	if fleetDefault != "" {
		return fleetDefault
	}
	return c.Slug
}
