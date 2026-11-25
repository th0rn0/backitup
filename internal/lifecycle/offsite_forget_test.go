package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// Behaviour after an operator stops tracking an object that a later run
// deduplicated against (the client page's "Stop tracking").
//
// The dedupe row is an inference — "my content is in that object" — and once the
// record of the object is gone the app cannot list it, download it or verify it.
// So the claim must go too, rather than outliving the row it points at: the
// forgotten snapshot stops counting as offsited (its hot copy is therefore kept,
// never pruned on the strength of a chain nobody can check), and the next upload
// pass gives the deduplicated run an object of its own.
func TestForgottenOwnerLeavesNoDedupeClaimBehind(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	const (
		owner     = "backup-20260501T000000Z-000.tar.gz" // only the offsite record exists
		dependent = "backup-20260515T000000Z-000.tar.gz" // hot, content lives in the owner's object
	)
	mkArchiveContent(t, filepath.Join(dir, dependent), now.Add(-34*24*time.Hour), "shared-tree")
	sum, err := fileChecksum(filepath.Join(dir, dependent))
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	for _, o := range []model.OffsiteObject{
		{ClientID: id, SnapshotID: owner, Remote: "local", Checksum: sum, UploadedAt: now.Add(-40 * 24 * time.Hour)},
		{ClientID: id, SnapshotID: dependent, Remote: "local", Checksum: sum, DedupOf: owner, UploadedAt: now.Add(-34 * 24 * time.Hour)},
	} {
		if err := st.RecordOffsiteObject(ctx, o); err != nil {
			t.Fatal(err)
		}
	}

	// The operator stops tracking the object from the client page.
	dedupRows, err := st.ForgetOffsiteObject(ctx, id, owner, "local")
	if err != nil {
		t.Fatalf("forget: %v", err)
	}
	if dedupRows != 1 {
		t.Fatalf("forget reported %d dedupe rows, want 1: the chain outlived the record", dedupRows)
	}
	objs, err := st.ListOffsiteObjects(ctx, id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 0 {
		t.Fatalf("records survived the forget of their only owner: %+v", objs)
	}
	if s := offsitedSetFrom(objs); s[owner] || s[dependent] {
		t.Fatalf("a snapshot still claims an offsite copy nobody can list, download or verify: %v", s)
	}

	// The dependent's hot copy is still there and is now the only copy, so the
	// pass must not prune it, and the upload pass must give it an object of its
	// own rather than deduplicating into a hole.
	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, dependent)); err != nil {
		t.Fatalf("the hot copy was pruned while it was the only copy left: %v", err)
	}
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	dep := offsiteRow(t, st, id, dependent)
	if dep.DedupOf != "" || dep.Bytes <= 0 {
		t.Fatalf("the deduplicated run still points at the forgotten object instead of uploading its own: %+v", dep)
	}
	if !remoteHolds(off, dependent) {
		t.Fatalf("the run has a record but no object on the remote: %v", off.up)
	}
}
