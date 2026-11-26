package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// offsiteKey is the fake remote's key for one object: "<dir>/<name>", which is
// what the uploader addresses when the remote has no bucket row.
func offsiteKey(dir, name string) string { return dir + "/" + name }

// A new upload lands at "<client-dir>/<prefix>-<snapshot id>": that is the whole
// point of the setting — a file found in the bucket names its client without the
// database.
func TestUploadNamesTheObjectAfterTheClient(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const snap = "backup-20261126T100000Z-000.tar.gz"

	tests := []struct {
		name   string
		client model.Client
		fleet  string
		want   string
	}{
		{
			name:   "the slug when nothing is chosen",
			client: model.Client{Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "local", RetentionDays: 14, Enabled: true},
			want:   offsiteKey("laptop", "laptop-"+snap),
		},
		{
			name: "the client's own prefix wins",
			client: model.Client{Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "local", RetentionDays: 14, Enabled: true,
				NamePrefix: "nas", NamePrefixSet: true},
			want: offsiteKey("laptop", "nas-"+snap),
		},
		{
			name:   "the fleet default when the client has not chosen",
			client: model.Client{Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "local", RetentionDays: 14, Enabled: true},
			fleet:  "fleet",
			want:   offsiteKey("laptop", "fleet-"+snap),
		},
		{
			// The regression the setting must not break: an operator who wants the
			// old names asks for no prefix and gets exactly what backitup wrote
			// before it existed — even with a fleet default configured.
			name: "no prefix produces today's name",
			client: model.Client{Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "local", RetentionDays: 14, Enabled: true,
				NamePrefixSet: true},
			fleet: "fleet",
			want:  offsiteKey("laptop", snap),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// A store per case: the slug is unique across a database, and every
			// case here creates a client called "laptop".
			st := openStore(t)
			_, base, dir := newClient(t, st, tc.client)
			deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now },
				NamePrefixDefault: tc.fleet}
			off := deps.Offsite.(*fakeOffsite)
			mkArchiveContent(t, filepath.Join(dir, snap), now.Add(-time.Hour), "payload")

			if err := RunOffsiteOnce(context.Background(), deps); err != nil {
				t.Fatalf("offsite pass: %v", err)
			}
			if len(off.up) != 1 {
				t.Fatalf("uploads = %v, want exactly one", singleObjectKeys(off.up))
			}
			if _, ok := off.up[tc.want]; !ok {
				t.Fatalf("the object was not written at %q; the remote holds %v", tc.want, singleObjectKeys(off.up))
			}
			// The prefix decorates the NAME only: the directory component must
			// still be the client's, whatever the prefix says.
			for key := range off.up {
				if !strings.HasPrefix(key, "laptop/") {
					t.Fatalf("object %q left the client's directory", key)
				}
			}
		})
	}
}

// The compatibility problem, from the side that would read as data loss: every
// object in the bucket was written without a prefix. The first verification pass
// after the upgrade must find them, or every row is flagged remote_missing — a
// false alarm that also stops the hot copy counting as offsited, so the pruner
// would delete the only copy.
func TestVerificationAcceptsTheLegacyUnprefixedObject(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const snap = "backup-20260101T100000Z-000.tar.gz"
	const legacyName = snap // what backitup wrote before prefixes existed

	id, base, dir := newClient(t, st, model.Client{
		Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "local", RetentionDays: 14, Enabled: true,
	})
	archive := filepath.Join(dir, snap)
	mkArchiveContent(t, archive, now.Add(-24*time.Hour), "payload")

	off := &fakeOffsite{}
	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	ctx := context.Background()

	// The row says the snapshot is offsite; the bucket has the OLD name only.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: snap, Remote: "local", Bytes: 10, UploadedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record offsite: %v", err)
	}
	if _, err := off.Upload(ctx, archive, "", offsiteKey("laptop", legacyName)); err != nil {
		t.Fatalf("plant the legacy object: %v", err)
	}

	logs := captureLog(t)
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("lifecycle pass: %v", err)
	}

	rows, err := st.ListOffsiteObjects(ctx, id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("offsite rows = %+v, %v", rows, err)
	}
	if rows[0].RemoteMissing {
		t.Fatalf("an object that IS in the bucket under its legacy name was reported missing: %+v", rows[0])
	}
	if rows[0].RemoteVerifiedAt.IsZero() {
		t.Fatal("the row was not verified at all (a skipped check is not a pass)")
	}
	if !strings.Contains(logs.String(), "legacy unprefixed name") {
		t.Errorf("the pass does not say that the object is still under its old name:\n%s", logs.String())
	}

	// And the guard must still bite: a snapshot under neither name is missing.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "backup-20260102T100000Z-000.tar.gz", Remote: "local", Bytes: 10,
		UploadedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("record second offsite: %v", err)
	}
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("lifecycle pass: %v", err)
	}
	for _, row := range mustRows(t, st, id) {
		switch row.SnapshotID {
		case snap:
			if row.RemoteMissing {
				t.Errorf("the legacy object was reported missing again: %+v", row)
			}
		case "backup-20260102T100000Z-000.tar.gz":
			if !row.RemoteMissing {
				t.Errorf("an object that is in the bucket under neither name was not reported missing: %+v", row)
			}
		}
	}
}

func mustRows(t *testing.T, st interface {
	ListOffsiteObjects(context.Context, int64) ([]model.OffsiteObject, error)
}, id int64) []model.OffsiteObject {
	t.Helper()
	rows, err := st.ListOffsiteObjects(context.Background(), id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	return rows
}

// The dedupe checksum is the SHA-256 of the archive, not of its name: two clients
// holding identical content must agree on it however differently they are named.
// If a prefix could change it, every client renaming itself would look like new
// data and re-upload the fleet.
func TestDedupeChecksumIsIndependentOfTheNamePrefix(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	const snap = "backup-20261126T100000Z-000.tar.gz"
	const content = "identical-content"

	// One base directory for both, because a pass walks the base it is given.
	base := t.TempDir()
	aID, _, aDir := newClientIn(t, st, base, model.Client{
		Name: "alpha", Mode: model.ModeTarGz, OffsiteRemote: "local", RetentionDays: 14, Enabled: true,
		NamePrefix: "team-a", NamePrefixSet: true,
	})
	bID, _, bDir := newClientIn(t, st, base, model.Client{
		Name: "bravo", Mode: model.ModeTarGz, OffsiteRemote: "local", RetentionDays: 14, Enabled: true,
		NamePrefix: "team-b", NamePrefixSet: true,
	})
	mkArchiveContent(t, filepath.Join(aDir, snap), now.Add(-time.Hour), content)
	mkArchiveContent(t, filepath.Join(bDir, snap), now.Add(-time.Hour), content)

	off := &fakeOffsite{}
	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	ctx := context.Background()
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("offsite pass: %v", err)
	}

	a := mustRows(t, st, aID)
	b := mustRows(t, st, bID)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("rows: alpha=%+v bravo=%+v", a, b)
	}
	if a[0].Checksum == "" || b[0].Checksum == "" {
		t.Fatalf("a checksum is empty: alpha=%q bravo=%q", a[0].Checksum, b[0].Checksum)
	}
	if a[0].Checksum != b[0].Checksum {
		t.Fatalf("identical archives under different prefixes got different checksums: %q vs %q",
			a[0].Checksum, b[0].Checksum)
	}
	if _, ok := off.up[offsiteKey("alpha", "team-a-"+snap)]; !ok {
		t.Errorf("alpha's object is not under its prefixed name: %v", singleObjectKeys(off.up))
	}
	if _, ok := off.up[offsiteKey("bravo", "team-b-"+snap)]; !ok {
		t.Errorf("bravo's object is not under its prefixed name: %v", singleObjectKeys(off.up))
	}
}
