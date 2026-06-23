package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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

func mkArchive(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	src := t.TempDir()
	_ = os.WriteFile(filepath.Join(src, "f.txt"), []byte("payload"), 0o644)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := archiveutil.TarGz(context.Background(), f, src, nil, false); err != nil {
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
	clientDir := filepath.Join(base, strconv.FormatInt(id, 10))
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

// Happy path: all snapshots offsited; hot prune drops the old+offsited one but
// keeps newest and within-horizon; offsite keeps everything (long horizon).
func TestRunOnceOffsiteAndPrune(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkArchive(t, filepath.Join(dir, "old.tar.gz"), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "mid.tar.gz"), now.Add(-5*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "new.tar.gz"), now.Add(-1*time.Hour))

	off := &fakeOffsite{}
	if err := RunOnce(context.Background(), Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// All three offsited.
	if len(off.up) != 3 {
		t.Fatalf("offsite uploads = %d, want 3: %v", len(off.up), off.up)
	}
	objs, _ := st.ListOffsiteObjects(context.Background(), id)
	if len(objs) != 3 {
		t.Fatalf("offsite records = %d, want 3", len(objs))
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

// Offsite-first: when upload fails, nothing is recorded offsite AND the old
// snapshot is NOT pruned from hot (never delete an un-offsited copy).
func TestRunOnceOffsiteFirstProtectsHot(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkArchive(t, filepath.Join(dir, "old.tar.gz"), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "new.tar.gz"), now.Add(-1*time.Hour))

	off := &fakeOffsite{fail: true}
	if err := RunOnce(context.Background(), Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}); err == nil {
		t.Fatal("expected error when offsite upload fails")
	}
	if objs, _ := st.ListOffsiteObjects(context.Background(), id); len(objs) != 0 {
		t.Fatalf("nothing should be recorded offsite, got %d", len(objs))
	}
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

	if len(off.del) != 1 || off.del[0] != objectPath(id, model.ModeTarGz, "stale") {
		t.Fatalf("expected stale offsite object deleted, got %v", off.del)
	}
	objs, _ := st.ListOffsiteObjects(context.Background(), id)
	if len(objs) != 1 || objs[0].SnapshotID != "fresh" {
		t.Fatalf("offsite retention should leave only fresh: %+v", objs)
	}
}
