package mode

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/th0rn0/backitup/internal/model"
)

// ErrInvalidSnapshotID marks a snapshot id that is not a name on the hot store.
var ErrInvalidSnapshotID = errors.New("invalid snapshot id")

// snapshotIDPatterns is the exact filename shape each mode produces. An id is
// the *name* of one entry inside a client's directory, so the shape is tight on
// purpose: anything looser is a path fragment in a name's clothing.
//
//	rsync:  <ts>                      (internal/mode/rsync Backup)
//	targz:  backup-<ts>-<seq>.tar.gz  (docker/sshd/backitup-recv)
var snapshotIDPatterns = map[model.Mode]*regexp.Regexp{
	model.ModeRsync: regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`),
	model.ModeTarGz: regexp.MustCompile(`^backup-[0-9]{8}T[0-9]{6}Z-[0-9]{3,}\.tar\.gz$`),
}

// ValidateSnapshotID reports whether id may be used as a snapshot name for mode
// m. Every caller that turns an id into a filesystem path goes through here —
// the bulk and single delete handlers and each mode's DeleteSnapshot — so they
// cannot disagree about what a legal id is.
//
// This exists because "." is not "..": the old guard only rejected ids
// containing "..", and filepath.Join(stateDir, ".") cleans to stateDir itself,
// so `ids=.` reached os.RemoveAll on a whole snapshots/ directory. Rejecting
// ".", "..", any path separator, NUL, and anything that is not the mode's own
// filename shape makes "collapse to the parent directory" unreachable by
// construction rather than by remembering to check.
func ValidateSnapshotID(m model.Mode, id string) error {
	if id == "" || id == "." || id == ".." {
		return fmt.Errorf("%w: %q", ErrInvalidSnapshotID, id)
	}
	if strings.Contains(id, "..") ||
		strings.ContainsAny(id, `/\`) ||
		strings.ContainsRune(id, 0) {
		return fmt.Errorf("%w: %q is not a name", ErrInvalidSnapshotID, id)
	}
	re, ok := snapshotIDPatterns[m]
	if !ok {
		return fmt.Errorf("%w: unknown mode %q", ErrInvalidSnapshotID, m)
	}
	if !re.MatchString(id) {
		return fmt.Errorf("%w: %q is not a %s snapshot name", ErrInvalidSnapshotID, id, m)
	}
	return nil
}

// SnapshotPath resolves id to a path directly inside base, refusing anything
// that is not exactly one entry in it. ValidateSnapshotID is the primary gate;
// this is the belt-and-braces check for a caller that regresses, because the
// failure it prevents (removing base itself) is unrecoverable.
func SnapshotPath(base, id string) (string, error) {
	clean := filepath.Clean(base)
	// One clean component, no separators: this check does not know the mode's
	// filename shape (ValidateSnapshotID owns that), only that an id is a name.
	if id == "" || id != filepath.Base(id) || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("%w: %q does not name one entry in %s", ErrInvalidSnapshotID, id, base)
	}
	p := filepath.Join(clean, id)
	if p == clean || filepath.Dir(p) != clean {
		return "", fmt.Errorf("%w: %q does not name one entry in %s", ErrInvalidSnapshotID, id, base)
	}
	return p, nil
}
