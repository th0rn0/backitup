package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/archiveutil"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"

	// register the tar.gz ServerMode so mode.Server(ModeTarGz) resolves
	_ "github.com/th0rn0/backitup/internal/mode/targz"
)

type fakeOffsite struct {
	up   map[string]int64
	del  []string
	fail bool
}

func (f *fakeOffsite) Upload(_ context.Context, local, _, obj string) (int64, error) {
	if f.fail {
		return 0, errors.New("upload boom")
	}
	fi, err := os.Stat(local)
	if err != nil {
		return 0, err
	}
	if f.up == nil {
		f.up = map[string]int64{}
	}
	f.up[obj] = fi.Size()
	return fi.Size(), nil
}

func (f *fakeOffsite) Delete(_ context.Context, _, obj string) error {
	f.del = append(f.del, obj)
	return nil
}

func (f *fakeOffsite) Lsf(_ context.Context, _, _ string) ([]string, error) {
	// Return the basenames of everything that was uploaded.
	var files []string
	for obj := range f.up {
		parts := strings.SplitN(obj, "/", 2)
		files = append(files, parts[len(parts)-1])
	}
	return files, nil
}

func mkArchive(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	mkArchiveContent(t, path, mtime, "payload")
}

// mkArchiveContent writes a tar.gz archive of a one-file tree with the given
// content and sets the archive's own mtime (which is what a snapshot's age is
// read from).
//
// The source file's mtime is pinned before archiving because tar records it in
// the header: an archive built from a file created "now" is only byte-identical
// to another archive of the same content if both were built inside the same
// second. Two tests depend on identical content meaning identical bytes — the
// dedupe path in TestRunOnceOffsiteAndPrune and the expiry tests — and without
// the pin they were a race against the clock.
func mkArchiveContent(t *testing.T, path string, mtime time.Time, content string) {
	t.Helper()
	src := t.TempDir()
	srcFile := filepath.Join(src, "f.txt")
	if err := os.WriteFile(srcFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(srcFile, pinned, pinned); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := archiveutil.TarGz(context.Background(), f, src, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	f.Close()
	_ = os.Chtimes(path, mtime, mtime)
}

func newClient(t *testing.T, st *store.Store, c model.Client) (int64, string, string) {
	t.Helper()
	base := t.TempDir()
	id, err := st.CreateClient(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	clientDir := filepath.Join(base, model.Slug(c.Name))
	_ = os.MkdirAll(clientDir, 0o755)
	return id, base, clientDir
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Happy path: offsite worker uploads all snapshots, records a dedup row for an
// unchanged tree, and the lifecycle worker then prunes hot (drops old+offsited,
// keeps newest and within-horizon) without touching a single offsite object.
//
// The prune half of this test is the hot half only: offsite retention is the B2
// bucket's lifecycle rule (see offsite_retention_delegated_test.go), so the
// offsite worker uploads and records, and the lifecycle pass deletes locally.
func TestRunOnceOffsiteAndPrune(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	mkArchiveContent(t, filepath.Join(dir, "backup-20260509T000000Z-000.tar.gz"), now.Add(-40*24*time.Hour), "payload-old")
	mkArchiveContent(t, filepath.Join(dir, "backup-20260613T000000Z-000.tar.gz"), now.Add(-5*24*time.Hour), "payload-mid")
	mkArchiveContent(t, filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz"), now.Add(-1*time.Hour), "payload-new")

	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)

	// Offsite worker uploads first.
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	// All three offsited.
	if len(off.up) != 3 {
		t.Fatalf("offsite uploads = %d, want 3: %v", len(off.up), off.up)
	}
	objs, _ := st.ListOffsiteObjects(context.Background(), id)
	if len(objs) != 3 {
		t.Fatalf("offsite records = %d, want 3", len(objs))
	}

	// An unchanged tree must not upload again: the next pass records the content
	// as deduplicated against the object that already holds it (0 bytes, no new
	// object), which is what keeps an unchanged client from growing cold storage.
	mkArchiveContent(t, filepath.Join(dir, "backup-20260618T120000Z-000.tar.gz"), now.Add(-1*time.Hour), "payload-new")
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce (unchanged tree): %v", err)
	}
	if len(off.up) != 3 {
		t.Fatalf("an unchanged tree must not upload again: uploads = %d, want still 3: %v", len(off.up), off.up)
	}
	objs, _ = st.ListOffsiteObjects(context.Background(), id)
	if len(objs) != 4 {
		t.Fatalf("offsite records = %d, want 4 (the new snapshot recorded as dedup)", len(objs))
	}
	var dedup model.OffsiteObject
	for _, o := range objs {
		if o.SnapshotID == "backup-20260618T120000Z-000.tar.gz" {
			dedup = o
		}
	}
	if dedup.DedupOf == "" || dedup.Bytes != 0 || dedup.Checksum == "" {
		t.Fatalf("an unchanged tree must be recorded as dedup with 0 bytes: %+v", dedup)
	}

	// Lifecycle worker then prunes hot only.
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(off.del) != 0 {
		t.Fatalf("the lifecycle pass deleted offsite objects %v; it must not delete any", off.del)
	}
	// Hot: old (>14d, offsited) pruned; mid + new kept.
	left, _ := os.ReadDir(dir)
	names := map[string]bool{}
	for _, e := range left {
		names[e.Name()] = true
	}
	if names["backup-20260509T000000Z-000.tar.gz"] {
		t.Error("old archive should have been pruned from hot")
	}
	if !names["backup-20260613T000000Z-000.tar.gz"] || !names["backup-20260618T000000Z-000.tar.gz"] {
		t.Errorf("mid/new should survive: %v", names)
	}
	// Nothing left the offsite side of the bookkeeping either.
	if after, _ := st.ListOffsiteObjects(context.Background(), id); len(after) != 4 {
		t.Fatalf("offsite records after the lifecycle pass = %d, want 4: %+v", len(after), after)
	}
}

// Offsite-first is now a freshness rule: a hot snapshot may be deleted when a
// STRICTLY NEWER snapshot is confirmed offsite. This pins the wiring end to end,
// in the shape the rule was written for — a client on offsite_upload_mode=latest,
// where only the newest snapshot ever has an offsite record of its own.
//
// First pass: every upload fails, so nothing is offsited, so retention may not
// delete a single copy and the pass says why. Second pass: the one upload that
// latest mode makes succeeds, and now both older snapshots are older than the
// newest offsite copy — so they go. Under the per-snapshot guard neither ever
// would have: neither has an offsite record of itself, so retention blocked
// forever on a client that only ever offsites the latest.
func TestRunOnceOffsiteFirstRequiresANewerOffsiteCopy(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	const (
		old    = "backup-20260509T000000Z-000.tar.gz" // 40d
		mid    = "backup-20260519T000000Z-000.tar.gz" // 30d
		newest = "backup-20260618T110000Z-000.tar.gz" // 1h
	)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		OffsiteUploadMode: "latest", RetentionDays: 14, Enabled: true,
	})
	mkArchive(t, filepath.Join(dir, old), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, mid), now.Add(-30*24*time.Hour))
	mkArchive(t, filepath.Join(dir, newest), now.Add(-1*time.Hour))

	deps := Deps{Store: st, Offsite: &fakeOffsite{fail: true}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)

	// ── uploads failing: no offsite copy exists, so no delete is permitted ──
	if err := RunOffsiteOnce(context.Background(), deps); err == nil {
		t.Fatal("expected error when offsite upload fails")
	}
	if objs, _ := st.ListOffsiteObjects(context.Background(), 1); len(objs) != 0 {
		t.Fatalf("nothing should be recorded offsite, got %d", len(objs))
	}
	logs := runOnceCapturingLog(t, deps)
	for _, name := range []string{old, mid, newest} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s must not be pruned while nothing is offsited: %v\n%s", name, err, logs)
		}
	}
	if want := "keeping 2 snapshots: nothing is offsited for this client"; !strings.Contains(logs, want) {
		t.Fatalf("the pass must say why it pruned nothing;\nwant a line containing: %s\ngot:\n%s", want, logs)
	}

	// ── the newest snapshot reaches cold storage; latest mode offsites only it ──
	off.fail = false
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce (healthy): %v", err)
	}
	if len(off.up) != 1 {
		t.Fatalf("offsite_upload_mode=latest uploaded %d snapshots, want 1: %v", len(off.up), off.up)
	}
	logs = runOnceCapturingLog(t, deps)
	for _, name := range []string{old, mid} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s must be pruned now that a strictly newer snapshot is offsite\n%s", name, logs)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, newest)); err != nil {
		t.Fatalf("the offsited newest snapshot must survive: %v\n%s", err, logs)
	}
	if want := "older than the newest offsite copy " + newest; !strings.Contains(logs, want) {
		t.Errorf("the log must name the copy that justified the delete;\nwant a line containing: %s\ngot:\n%s", want, logs)
	}
}

// No offsite configured: hot prune still applies by age (the hot horizon).
func TestRunOnceNoOffsitePrunesByAge(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "", RetentionDays: 14, Enabled: true,
	})
	mkArchive(t, filepath.Join(dir, "backup-20260509T000000Z-000.tar.gz"), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz"), now.Add(-1*time.Hour))

	off := &fakeOffsite{}
	_ = RunOnce(context.Background(), Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }})
	if len(off.up) != 0 {
		t.Fatal("no offsite remote -> no uploads")
	}
	if _, err := os.Stat(filepath.Join(dir, "backup-20260509T000000Z-000.tar.gz")); err == nil {
		t.Fatal("old archive should be pruned by hot retention even without offsite")
	}
	if _, err := os.Stat(filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz")); err != nil {
		t.Fatal("newest must be protected")
	}
}

// Stale .part files (older than PartOrphanAge) are removed; a recently-touched
// .part file (simulating an in-progress upload) is left alone.
func TestPrunePartOrphans(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, RetentionDays: 14, Enabled: true,
	})

	write := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}

	stale := write("backup-20260617T120000Z-000.tar.gz.part")
	active := write("backup-20260618T115900Z-000.tar.gz.part")
	keep := write("backup-20260618T110000Z-000.tar.gz") // real archive — must not be touched

	// Stale: mtime 3h before now (> DefaultPartOrphanAge of 2h).
	_ = os.Chtimes(stale, now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	// Active: mtime 1 minute before now (< threshold).
	_ = os.Chtimes(active, now.Add(-1*time.Minute), now.Add(-1*time.Minute))
	_ = os.Chtimes(keep, now.Add(-1*time.Hour), now.Add(-1*time.Hour))

	deps := Deps{Store: st, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if _, err := os.Stat(stale); err == nil {
		t.Error("stale .part file should have been removed")
	}
	if _, err := os.Stat(active); err != nil {
		t.Error("active .part file should NOT have been removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("completed archive must not be touched")
	}
}
