package server

import (
	"fmt"
	"path/filepath"

	"github.com/th0rn0/backitup/internal/authkeys"
	"github.com/th0rn0/backitup/internal/model"
)

// clientDir returns the storage directory for c: backupBaseDir/<slug>.
//
// A client's directory comes from its PERSISTED slug and nothing else — never
// from its name, never from the request. Every path that is about one client
// goes through here so the check lives in one place instead of in whichever
// call site remembered it: a slug that is empty, or that is not a plain
// [a-z0-9-] component, is refused with an error.
//
// The error path matters more than it looks. filepath.Join(baseDir, "") is
// baseDir itself, which is every client's parent directory, so "no slug" must
// not degrade to a path — it must fail. Callers differ in what they do with the
// failure (HTTP 500, skip the client, treat it as "no previous snapshot"), but
// none of them may end up with a directory that is not this client's.
func (s *Server) clientDir(c model.Client) (string, error) {
	return clientDirIn(s.backupBaseDir, c)
}

// clientDirIn is the package-level form, so tests can exercise the rule without
// constructing a Server.
func clientDirIn(baseDir string, c model.Client) (string, error) {
	if reason := authkeys.InvalidSlug(c.Slug); reason != "" {
		return "", fmt.Errorf("client %d (%q) has an unusable slug: %s", c.ID, c.Name, reason)
	}
	return filepath.Join(baseDir, c.Slug), nil
}
