package lifecycle

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// A run whose tree was unchanged does not upload: it is recorded as a dedupe of
// the run that owns the object, with 0 bytes moved. That record is an inference —
// "my content lives in that object" — and once B2's bucket lifecycle rule expires
// the object it is false. Nothing in the database notices on its own, so the row
// keeps claiming an offsite copy.
//
// Since the hot-prune guard became a freshness rule, that claim is dangerous in
// exactly one shape: it can be the NEWEST offsite copy, and then every older
// snapshot is deleted because of it. The pass below is therefore the whole point —
// the pass that prunes looks at the remote first (pruneLeansOnDedupe forces the
// check here), the expired owner is *seen*, the dependent run is flagged as
// needing a re-upload instead of keeping a false claim, and because the claim no
// longer moves the newest-offsite marker nothing is pruned on the strength of it.
// The next upload pass puts the content back.
func TestDedupedRunIsFlaggedAndReuploadedWhenItsOwnerObjectExpires(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	const (
		// The upload pass walks newest-first, so of two identical snapshots the
		// newer one is uploaded and becomes the owner of the object. The dependent
		// is a LATER run over the same tree (created between the two upload
		// passes) so that it — not the owner — is the newest snapshot with an
		// offsite record: the position from which a stale claim would justify a
		// delete. `older` is the prune candidate that claim would have killed.
		older     = "backup-20260509T000000Z-000.tar.gz" // 40d: own object, prune candidate
		owner     = "backup-20260515T000000Z-000.tar.gz" // 34d: uploaded, owns the shared object
		dependent = "backup-20260516T000000Z-000.tar.gz" // 33d: same content, deduped
		newest    = "backup-20260618T000000Z-000.tar.gz" // 1h: created after the upload passes
	)
	mkArchiveContent(t, filepath.Join(dir, older), now.Add(-40*24*time.Hour), "older-tree")
	mkArchiveContent(t, filepath.Join(dir, owner), now.Add(-34*24*time.Hour), "shared-tree")

	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)

	// ── upload the owner's tree and the older snapshot ─────────────────────
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	if len(off.up) != 2 {
		t.Fatalf("offsite uploads = %d, want 2 (the owner and the older snapshot): %v", len(off.up), off.up)
	}

	// ── a later run of the unchanged tree dedupes against the owner ────────
	mkArchiveContent(t, filepath.Join(dir, dependent), now.Add(-33*24*time.Hour), "shared-tree")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("RunOffsiteOnce (unchanged tree): %v", err)
	}
	if len(off.up) != 2 {
		t.Fatalf("an unchanged tree must not upload again: uploads = %d, want still 2: %v", len(off.up), off.up)
	}
	dep := offsiteRow(t, st, id, dependent)
	if dep.DedupOf != owner || dep.Bytes != 0 {
		t.Fatalf("an unchanged tree must be recorded as a dedupe of %s with 0 bytes: %+v", owner, dep)
	}

	// ── a still later run, not offsited yet; the newest is protected anyway ─
	mkArchiveContent(t, filepath.Join(dir, newest), now.Add(-time.Hour), "other-tree")

	// ── the bucket lifecycle rule expires the owner's object ───────────────
	if !deleteObject(off, owner) {
		t.Fatalf("the owner's object was never uploaded, so it cannot be expired: %v", off.up)
	}

	// ── a lifecycle pass: flag, say why, and prune nothing on the claim ────
	logs := captureLog(t)
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	want := "offsite object " + defaultPrefix("c") + owner + " referenced by run " + defaultPrefix("c") + dependent +
		" is gone (expired by the bucket lifecycle rule): re-uploading"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("the pass must name the expired object and the run that deduped against it;\nwant: %s\ngot:\n%s", want, logs.String())
	}
	dep = offsiteRow(t, st, id, dependent)
	if !dep.RemoteMissing {
		t.Fatalf("the dependent run still claims an offsite copy that does not exist: %+v", dep)
	}
	// With the claim rejected, the newest confirmed offsite copy is the 40-day-old
	// one, so nothing is strictly older than it and nothing may be pruned. Had the
	// stale claim been honoured, `older` would have died here with no copy left.
	for _, name := range []string{older, owner, dependent, newest} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s was pruned on the strength of an expired dedupe claim: %v\n%s", name, err, logs.String())
		}
	}

	// ── the next upload pass puts the content back ─────────────────────────
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("RunOffsiteOnce (re-upload): %v", err)
	}
	objs, err := st.ListOffsiteObjects(ctx, id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if !offsitedSetFrom(objs)[dependent] {
		t.Fatalf("the deduped run still has no backed offsite copy after the upload pass: %+v", objs)
	}
	if dep = offsiteRow(t, st, id, dependent); dep.RemoteMissing {
		t.Fatalf("a re-uploaded run must not stay flagged as missing: %+v", dep)
	}
	// Both hot copies exist now, so the pass uploads one of them (the newer, the
	// dependent) and dedupes the other against it: what matters is that the shared
	// tree is on the remote again, under one of the two runs' names.
	if !remoteHolds(off, owner) && !remoteHolds(off, dependent) {
		t.Fatalf("the shared tree is on the remote under neither run's object after the re-upload pass: %v", off.up)
	}

	// One line per client per pass, and only on the transition: the claim is
	// gone from the database now, so a later pass has nothing to announce.
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("RunOnce (after the re-upload): %v", err)
	}
	if n := strings.Count(logs.String(), "is gone (expired by the bucket lifecycle rule)"); n != 1 {
		t.Fatalf("the expired-object line was printed %d times, want once per client per pass:\n%s", n, logs.String())
	}
}

// The only-copy rule wins: when the owner cannot be re-uploaded (its hot copy is
// already gone) the dependent run uploads an object of its own rather than
// deduplicating into a hole, and until it does its hot copy must not be pruned.
func TestDedupedRunUploadsItsOwnObjectWhenTheOwnerCannotBeReuploaded(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	const (
		owner     = "backup-20260501T000000Z-000.tar.gz" // only the offsite record survives
		dependent = "backup-20260515T000000Z-000.tar.gz" // 34d, hot
		newest    = "backup-20260618T000000Z-000.tar.gz" // 1h
	)
	mkArchiveContent(t, filepath.Join(dir, dependent), now.Add(-34*24*time.Hour), "shared-tree")
	mkArchiveContent(t, filepath.Join(dir, newest), now.Add(-time.Hour), "other-tree")
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

	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)

	logs := captureLog(t)
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if !strings.Contains(logs.String(), "re-uploading") {
		t.Fatalf("the pass must say what happened to the expired object:\n%s", logs.String())
	}
	if _, err := os.Stat(filepath.Join(dir, dependent)); err != nil {
		t.Fatalf("the hot copy was pruned while it was the only copy left: %v", err)
	}

	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	dep := offsiteRow(t, st, id, dependent)
	if dep.DedupOf != "" || dep.Bytes <= 0 {
		t.Fatalf("with no owner object to point at, the run must upload its own: %+v", dep)
	}
	if !remoteHolds(off, dependent) {
		t.Fatalf("the dependent run's own object was not uploaded: %v", off.up)
	}
}

// The healthy case, which must stay healthy: an owner that is still there means
// the dedupe stands, nothing is re-uploaded, and no pass invents a problem to
// report. This is the guard against "fix the false claim by uploading everything
// every pass".
func TestHealthyDedupeReuploadsNothingAcrossPasses(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	const (
		// Same shape as the expiry test: the newer of two identical snapshots is
		// the one that gets uploaded and owns the object.
		owner     = "backup-20260515T000000Z-000.tar.gz"
		dependent = "backup-20260509T000000Z-000.tar.gz"
		newest    = "backup-20260618T000000Z-000.tar.gz"
	)
	mkArchiveContent(t, filepath.Join(dir, owner), now.Add(-34*24*time.Hour), "shared-tree")
	mkArchiveContent(t, filepath.Join(dir, dependent), now.Add(-40*24*time.Hour), "shared-tree")
	mkArchiveContent(t, filepath.Join(dir, newest), now.Add(-time.Hour), "other-tree")

	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)

	logs := captureLog(t)
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	if n := len(off.up); n != 2 {
		t.Fatalf("offsite uploads = %d, want 2 (owner + newest; the third run dedupes): %v", n, off.up)
	}
	for i := 0; i < 3; i++ {
		if err := RunOnce(ctx, deps); err != nil {
			t.Fatalf("RunOnce %d: %v", i, err)
		}
		if err := RunOffsiteOnce(ctx, deps); err != nil {
			t.Fatalf("RunOffsiteOnce %d: %v", i, err)
		}
	}
	if n := len(off.up); n != 2 {
		t.Fatalf("a healthy fleet must not re-upload every pass: uploads = %d, want still 2: %v", n, off.up)
	}
	if dep := offsiteRow(t, st, id, dependent); dep.DedupOf != owner || dep.RemoteMissing {
		t.Fatalf("the dedupe record must survive an intact owner: %+v", dep)
	}
	if strings.Contains(logs.String(), "is gone (expired by the bucket lifecycle rule)") {
		t.Fatalf("nothing expired here; no pass may claim otherwise:\n%s", logs.String())
	}
}

// captureLog collects everything the package logs from this point on, restoring
// the previous writer when the test ends.
func captureLog(t *testing.T) *syncBuffer {
	t.Helper()
	logs := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })
	return logs
}

// offsiteRow is one stored record by snapshot id.
func offsiteRow(t *testing.T, st *store.Store, clientID int64, snapshotID string) model.OffsiteObject {
	t.Helper()
	objs, err := st.ListOffsiteObjects(context.Background(), clientID)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	for _, o := range objs {
		if o.SnapshotID == snapshotID {
			return o
		}
	}
	t.Fatalf("no offsite record for %s among %+v", snapshotID, objs)
	return model.OffsiteObject{}
}

// deleteObject drops one object from the fake remote, which is how a bucket
// lifecycle rule expiring it looks from here: the record survives, the object does
// not. It reports whether anything was removed.
func deleteObject(off *fakeOffsite, snapshotID string) bool {
	for obj := range off.up {
		if objectIsFor(obj, snapshotID) {
			delete(off.up, obj)
			return true
		}
	}
	return false
}

// remoteHolds reports whether the fake remote still has that object.
func remoteHolds(off *fakeOffsite, snapshotID string) bool {
	for obj := range off.up {
		if objectIsFor(obj, snapshotID) {
			return true
		}
	}
	return false
}

// objectIsFor reports whether an object name on the fake remote holds that
// snapshot. Objects are named "<dir>/<prefix>-<snapshot id>", or
// "<dir>/<snapshot id>" for a client with no prefix (and for every object a
// bucket written before name prefixes holds), so both spellings match.
func objectIsFor(obj, snapshotID string) bool {
	return strings.HasSuffix(obj, "/"+snapshotID) || strings.HasSuffix(obj, "-"+snapshotID) || obj == snapshotID
}
