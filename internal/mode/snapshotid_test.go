package mode

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// The id gate is shared by the bulk handler, the single handler and every mode's
// DeleteSnapshot, so it is tested directly as well as through each caller.
func TestValidateSnapshotID(t *testing.T) {
	good := map[model.Mode][]string{
		model.ModeRsync: {"20260101T000000Z", "19991231T235959Z"},
		model.ModeTarGz: {"backup-20260101T000000Z-000.tar.gz", "backup-20260101T000000Z-1234.tar.gz"},
	}
	for m, ids := range good {
		for _, id := range ids {
			if err := ValidateSnapshotID(m, id); err != nil {
				t.Errorf("ValidateSnapshotID(%s, %q) = %v, want nil", m, id, err)
			}
		}
	}

	bad := []string{
		"",
		".",
		"..",
		"../..",
		"../../etc/passwd",
		"/etc/passwd",
		"a/b",
		`a\b`,
		"..20260101T000000Z",
		"20260101T000000Z/",
		"latest",
		"2026010",
		"20260101T000000", // missing trailing Z
		"backup-20260101T000000Z.tar.gz",
		"backup-20260101T000000Z-000.tgz",
		"backup-20260101T000000Z-00.tar.gz",
	}
	for _, m := range []model.Mode{model.ModeRsync, model.ModeTarGz} {
		for _, id := range bad {
			if err := ValidateSnapshotID(m, id); !errors.Is(err, ErrInvalidSnapshotID) {
				t.Errorf("ValidateSnapshotID(%s, %q) = %v, want ErrInvalidSnapshotID", m, id, err)
			}
		}
	}

	// A mode's own shape is not another mode's shape: the same name must not be
	// accepted for both, or the check is decorative.
	if err := ValidateSnapshotID(model.ModeRsync, "backup-20260101T000000Z-000.tar.gz"); err == nil {
		t.Error("rsync accepted a targz name")
	}
	if err := ValidateSnapshotID(model.ModeTarGz, "20260101T000000Z"); err == nil {
		t.Error("targz accepted a rsync name")
	}
	// Unknown modes fail closed rather than skipping the shape check.
	if err := ValidateSnapshotID("nope", "20260101T000000Z"); err == nil {
		t.Error("unknown mode accepted an id")
	}
}

// SnapshotPath is the belt-and-braces half: even if a caller regresses and skips
// ValidateSnapshotID, resolving an id must never yield the base directory itself.
func TestSnapshotPathRefusesBase(t *testing.T) {
	base := filepath.Join("/srv", "backups", "nas", "snapshots")

	got, err := SnapshotPath(base, "20260101T000000Z")
	if err != nil || got != filepath.Join(base, "20260101T000000Z") {
		t.Fatalf("SnapshotPath = %q, %v", got, err)
	}

	for _, id := range []string{".", "..", "", "../..", "a/b", "a/../b"} {
		if p, err := SnapshotPath(base, id); !errors.Is(err, ErrInvalidSnapshotID) {
			t.Errorf("SnapshotPath(%q) = %q, %v; want ErrInvalidSnapshotID", id, p, err)
		}
	}
}
