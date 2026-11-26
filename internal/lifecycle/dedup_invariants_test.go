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

// A dedup record names an object owned by another snapshot: it owns nothing on
// the remote, and no lifecycle pass may delete either row or that object. The
// pruning half of this invariant went with offsite pruning itself (offsite
// retention is the B2 bucket's rule; TestRunOnceDeletesNoOffsiteObjectWhateverItsAge
// asserts no deletion at all, dedup rows included), but the data-safety half did
// not: a dedup record is offsite evidence — it can be the newest offsite copy that
// a delete leans on — only while the object it points at is intact.
//
// So the two cases differ in what the marker becomes. With the owner's object
// intact the dependent (17th) IS the newest offsite copy, which makes the owner
// (16th) strictly older and therefore prunable; the dependent itself stays,
// because the freshness rule keeps anything at or newer than the marker. With the
// owner reported remote_missing the dependent's claim is not offsite evidence
// either (PR #42), there is no newest offsite copy at all, and nothing is pruned.
//
// The two cases run as separate passes with separate remotes on purpose: the pass
// reconciles each client's records against what its remote reports before it
// prunes, so "intact" has to mean an object the remote really holds and "missing"
// a remote that really does not have it. One fake shared by both would have to
// report the same filename as present and absent at once.
func TestDedupRecordOnlyProtectsWhileOwnerObjectExists(t *testing.T) {
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
		{"intact", false, want{pruned: []string{"backup-20260616T000000Z-000.tar.gz"}, kept: []string{"backup-20260617T000000Z-000.tar.gz", "backup-20260618T000000Z-000.tar.gz"}}},
		{"owner-missing", true, want{pruned: nil, kept: []string{"backup-20260617T000000Z-000.tar.gz", "backup-20260616T000000Z-000.tar.gz", "backup-20260618T000000Z-000.tar.gz"}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			base := t.TempDir()
			id, _, dir := newClientIn(t, st, base, model.Client{
				Name: tc.name, Mode: model.ModeTarGz, OffsiteRemote: "local",
				RetentionDays: 1, Enabled: true,
			})
			mkArchiveContent(t, filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz"), now.Add(-1*time.Hour), tc.name+"-new")
			mkArchiveContent(t, filepath.Join(dir, "backup-20260616T000000Z-000.tar.gz"), now.Add(-40*time.Hour), tc.name+"-content")
			mkArchiveContent(t, filepath.Join(dir, "backup-20260617T000000Z-000.tar.gz"), now.Add(-30*time.Hour), tc.name+"-content")

			if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
				ClientID: id, SnapshotID: "backup-20260616T000000Z-000.tar.gz", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
				ClientID: id, SnapshotID: "backup-20260617T000000Z-000.tar.gz", Remote: "local", DedupOf: "backup-20260616T000000Z-000.tar.gz",
				UploadedAt: now.Add(-30 * 24 * time.Hour),
			}); err != nil {
				t.Fatal(err)
			}
			if tc.missing {
				if err := st.UpdateOffsiteRemoteStatus(ctx, offsiteRowID(t, st, id, "backup-20260616T000000Z-000.tar.gz"), true, now); err != nil {
					t.Fatal(err)
				}
			}

			// The remote holds the owner's object exactly when the claim is true.
			off := &fakeOffsite{}
			if !tc.missing {
				off.up = map[string]int64{
					model.ObjectPath("", model.Slug(tc.name), defaultPrefix(model.Slug(tc.name))+"backup-20260616T000000Z-000.tar.gz"): 1,
				}
			}

			deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
			if err := RunOnce(ctx, deps); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
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
		})
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
