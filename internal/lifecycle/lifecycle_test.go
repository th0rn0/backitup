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

func mkArchiveContent(t *testing.T, path string, mtime time.Time, content string) {
	t.Helper()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f.txt"), []byte(content), 0o644)
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

// Happy path: offsite worker uploads all snapshots; lifecycle worker then
// prunes hot (drops old+offsited, keeps newest and within-horizon).
func TestRunOnceOffsiteAndPrune(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkArchiveContent(t, filepath.Join(dir, "old.tar.gz"), now.Add(-40*24*time.Hour), "payload-old")
	mkArchiveContent(t, filepath.Join(dir, "mid.tar.gz"), now.Add(-5*24*time.Hour), "payload-mid")
	mkArchiveContent(t, filepath.Join(dir, "new.tar.gz"), now.Add(-1*time.Hour), "payload-new")

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

	// Lifecycle worker then prunes.
	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	// Hot: old (>14d, offsited) pruned; mid + new kept.
	left, _ := os.ReadDir(dir)
	names := map[string]bool{}
	for _, e := range left {
		names[e.Name()] = true
	}
	if names["old.tar.gz"] {
		t.Error("old archive should have been pruned from hot")
	}
	if !names["mid.tar.gz"] || !names["new.tar.gz"] {
		t.Errorf("mid/new should survive: %v", names)
	}
}

// Offsite-first: when the offsite worker's upload fails, nothing is recorded
// offsite, AND the lifecycle worker must NOT prune the old snapshot from hot.
func TestRunOnceOffsiteFirstProtectsHot(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkArchive(t, filepath.Join(dir, "old.tar.gz"), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "new.tar.gz"), now.Add(-1*time.Hour))

	deps := Deps{Store: st, Offsite: &fakeOffsite{fail: true}, BackupBaseDir: base, Now: func() time.Time { return now }}

	// Offsite worker fails.
	if err := RunOffsiteOnce(context.Background(), deps); err == nil {
		t.Fatal("expected error when offsite upload fails")
	}
	if objs, _ := st.ListOffsiteObjects(context.Background(), id); len(objs) != 0 {
		t.Fatalf("nothing should be recorded offsite, got %d", len(objs))
	}

	// Lifecycle worker runs — must not prune the un-offsited old snapshot.
	_ = RunOnce(context.Background(), deps)
	if _, err := os.Stat(filepath.Join(dir, "old.tar.gz")); err != nil {
		t.Fatal("old un-offsited archive must NOT be pruned (offsite-first)")
	}
}

// No offsite configured: hot prune still applies by age (the hot horizon).
func TestRunOnceNoOffsitePrunesByAge(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "", RetentionDays: 14, Enabled: true,
	})
	mkArchive(t, filepath.Join(dir, "old.tar.gz"), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "new.tar.gz"), now.Add(-1*time.Hour))

	off := &fakeOffsite{}
	_ = RunOnce(context.Background(), Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }})
	if len(off.up) != 0 {
		t.Fatal("no offsite remote -> no uploads")
	}
	if _, err := os.Stat(filepath.Join(dir, "old.tar.gz")); err == nil {
		t.Fatal("old archive should be pruned by hot retention even without offsite")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.tar.gz")); err != nil {
		t.Fatal("newest must be protected")
	}
}

// Independent offsite retention: an old offsite object is pruned remotely on the
// offsite horizon, keeping the newest.
func TestRunOnceOffsiteRetention(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, _ := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 7, Enabled: true,
	})
	// No hot snapshots; pre-seed two offsite objects (one stale, one fresh).
	_ = st.RecordOffsiteObject(context.Background(), model.OffsiteObject{ClientID: id, SnapshotID: "stale", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour)})
	_ = st.RecordOffsiteObject(context.Background(), model.OffsiteObject{ClientID: id, SnapshotID: "fresh", Remote: "local", UploadedAt: now.Add(-1 * time.Hour)})

	off := &fakeOffsite{}
	_ = RunOnce(context.Background(), Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }})

	if len(off.del) != 1 || off.del[0] != objectPath(model.Slug("c"), model.ModeTarGz, "stale") {
		t.Fatalf("expected stale offsite object deleted, got %v", off.del)
	}
	objs, _ := st.ListOffsiteObjects(context.Background(), id)
	if len(objs) != 1 || objs[0].SnapshotID != "fresh" {
		t.Fatalf("offsite retention should leave only fresh: %+v", objs)
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
