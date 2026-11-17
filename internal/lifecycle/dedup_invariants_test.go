package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// newClientIn is newClient with the backup base directory supplied by the
// caller, so several clients can share one base (one lifecycle pass).
func newClientIn(t *testing.T, st *store.Store, base string, c model.Client) (int64, string, string) {
	t.Helper()
	id, err := st.CreateClient(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, model.Slug(c.Name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return id, base, dir
}

// A dedup record must not be treated as a remote object of its own: it names an
// object owned by another snapshot. Pre-fix (had a duplicate been recorded as an
// ordinary row) the newest-by-upload order would protect the phantom and delete
// the real object of that content — deleting the only copy while both rows still
// claimed it was safe.
func TestDedupRowIsNotPrunedAsItsOwnObject(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, _ := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 7, Enabled: true,
	})
	ctx := context.Background()
	// The real object is past the offsite horizon; the dedup row pointing at it
	// was recorded later, so it sorts newest.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "owner", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "dup", Remote: "local", DedupOf: "owner", UploadedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	off := &fakeOffsite{}
	if err := RunOnce(ctx, Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(off.del) != 0 {
		t.Fatalf("deleted %v: the owner is the newest *real* object and must stay; a dedup row owns nothing to delete", off.del)
	}
	objs, err := st.ListOffsiteObjects(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("offsite records = %d, want both kept: %+v", len(objs), objs)
	}
}

// A dedup record only protects its snapshot while the object it points at is
// intact. Once the verification pass reports the owner's object missing, the
// content is nowhere — so the hot copy of the duplicate must be kept, and the
// real object of another client with an intact record must still be pruned.
func TestDedupRecordOnlyProtectsWhileOwnerObjectExists(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	type want struct {
		pruned []string
		kept   []string
	}
	cases := []struct {
		name     string
		missing  bool
		wantKill want
	}{
		{"intact", false, want{pruned: []string{"dup.tar.gz", "owner.tar.gz"}, kept: []string{"new.tar.gz"}}},
		{"owner-missing", true, want{pruned: nil, kept: []string{"dup.tar.gz", "owner.tar.gz", "new.tar.gz"}}},
	}

	base := t.TempDir()
	off := &fakeOffsite{}
	for _, tc := range cases {
		id, _, dir := newClientIn(t, st, base, model.Client{
			Name: tc.name, Mode: model.ModeTarGz, OffsiteRemote: "local",
			RetentionDays: 1, OffsiteRetentionDays: 90, Enabled: true,
		})
		mkArchiveContent(t, filepath.Join(dir, "new.tar.gz"), now.Add(-1*time.Hour), tc.name+"-new")
		mkArchiveContent(t, filepath.Join(dir, "owner.tar.gz"), now.Add(-40*time.Hour), tc.name+"-content")
		mkArchiveContent(t, filepath.Join(dir, "dup.tar.gz"), now.Add(-30*time.Hour), tc.name+"-content")

		if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: id, SnapshotID: "owner.tar.gz", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: id, SnapshotID: "dup.tar.gz", Remote: "local", DedupOf: "owner.tar.gz",
			UploadedAt: now.Add(-30 * 24 * time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
		if tc.missing {
			if err := st.UpdateOffsiteRemoteStatus(ctx, offsiteRowID(t, st, id, "owner.tar.gz"), true, now); err != nil {
				t.Fatal(err)
			}
		}
	}

	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for _, tc := range cases {
		dir := filepath.Join(base, model.Slug(tc.name))
		for _, name := range tc.wantKill.pruned {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				t.Errorf("%s: %s should have been pruned", tc.name, name)
			}
		}
		for _, name := range tc.wantKill.kept {
			if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
				t.Errorf("%s: %s must survive: %v", tc.name, name, err)
			}
		}
	}
}

// offsiteRowID returns the row id of one recorded object.
func offsiteRowID(t *testing.T, st *store.Store, clientID int64, snapshotID string) int64 {
	t.Helper()
	objs, err := st.ListOffsiteObjects(context.Background(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.SnapshotID == snapshotID {
			return o.ID
		}
	}
	t.Fatalf("no offsite row for snapshot %s", snapshotID)
	return 0
}
