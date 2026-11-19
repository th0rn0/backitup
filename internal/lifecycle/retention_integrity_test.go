package lifecycle

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"

	_ "modernc.org/sqlite"

	// register the rsync ServerMode so mode.Server(ModeRsync) resolves
	_ "github.com/th0rn0/backitup/internal/mode/rsync"
)

// copyArchive copies an archive byte-for-byte under a new snapshot id with a
// chosen mtime. Two runs over an unchanged tree produce exactly this: identical
// content, a new snapshot name, a later timestamp.
func copyArchive(t *testing.T, src, dst string, mtime time.Time) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
	if err := os.Chtimes(dst, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", dst, err)
	}
}

// Item 1 (dedupe). An unchanged tree uploaded three times yields one real
// offsite object and two deduplicated snapshots. Pre-fix the two duplicates are
// never recorded, so they never count as "offsited" and the hot pruner protects
// them forever: the hot store grows by one snapshot per run no matter what
// retention_days says.
func TestDeduplicatedUploadIsRecordedSoHotPruneProceeds(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 1, OffsiteRetentionDays: 90, Enabled: true,
	})

	// Three snapshots of an unchanged tree: the newest inside the hot horizon,
	// the other two already past it.
	newest := filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz")
	mkArchiveContent(t, newest, now.Add(-1*time.Hour), "unchanged-tree")
	copyArchive(t, newest, filepath.Join(dir, "backup-20260617T000000Z-000.tar.gz"), now.Add(-30*time.Hour))
	copyArchive(t, newest, filepath.Join(dir, "backup-20260509T000000Z-000.tar.gz"), now.Add(-40*time.Hour))

	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	off := deps.Offsite.(*fakeOffsite)
	if len(off.up) != 1 {
		t.Fatalf("uploads = %d, want 1 (identical content is deduplicated): %v", len(off.up), off.up)
	}
	objs, err := st.ListOffsiteObjects(context.Background(), id)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	if len(objs) != 3 {
		t.Fatalf("offsite records = %d (%+v), want 3: a deduplicated snapshot must be recorded too, "+
			"otherwise it never becomes \"offsited\" and the hot pruner protects it forever", len(objs), objs)
	}

	// A snapshot with unique content that was never offsited must still be
	// protected by the offsite-first rule (created after the upload pass).
	unoffsited := filepath.Join(dir, "backup-20260509T000001Z-000.tar.gz")
	mkArchiveContent(t, unoffsited, now.Add(-40*time.Hour), "unique-content")

	if err := RunOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	for _, pruned := range []string{"backup-20260509T000000Z-000.tar.gz", "backup-20260617T000000Z-000.tar.gz"} {
		if _, statErr := os.Stat(filepath.Join(dir, pruned)); statErr == nil {
			t.Errorf("%s must be pruned: its content is offsite (deduplicated) and it is past the 1-day hot horizon", pruned)
		}
	}
	for _, kept := range []string{"backup-20260618T000000Z-000.tar.gz", "backup-20260509T000001Z-000.tar.gz"} {
		if _, statErr := os.Stat(filepath.Join(dir, kept)); statErr != nil {
			t.Errorf("%s must survive (newest snapshot / no offsite record): %v", kept, statErr)
		}
	}
}

// Item 2 (prune must report). `latest` still points at an older snapshot (a
// newer run failed before flipping it), which is exactly the case rsync refuses
// to delete. Pre-fix the failure is logged and swallowed, so RunOnce reports a
// clean pass while the hot store keeps everything the pruner could not remove.
func TestPruneHotSurfacesDeleteFailures(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, RetentionDays: 14, Enabled: true,
	})
	snaps := filepath.Join(dir, "snapshots")
	for _, id := range []string{"20260501T000000Z", "20260520T000000Z", "20260618T110000Z"} {
		if err := os.MkdirAll(filepath.Join(snaps, id), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(snaps, id, "f.txt"), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("20260520T000000Z", filepath.Join(snaps, "latest")); err != nil {
		t.Fatal(err)
	}

	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	err := RunOnce(context.Background(), deps)
	if err == nil {
		t.Fatal("RunOnce reported a clean pass although a prune deletion failed")
	}
	if !strings.Contains(err.Error(), "20260520T000000Z") {
		t.Errorf("the error must name the snapshot that could not be pruned, got: %v", err)
	}
	// One failure must not abort the rest of the prune.
	if _, statErr := os.Stat(filepath.Join(snaps, "20260501T000000Z")); statErr == nil {
		t.Error("the deletable old snapshot should still have been pruned")
	}
	for _, kept := range []string{"20260520T000000Z", "20260618T110000Z"} {
		if _, statErr := os.Stat(filepath.Join(snaps, kept)); statErr != nil {
			t.Errorf("%s must survive: %v", kept, statErr)
		}
	}
}

// Item 3 (fail closed). Offsite pruning deletes anything older than the horizon.
// A row whose uploaded_at cannot be parsed used to become year 1 — older than
// any horizon — so a recent remote object with a corrupt timestamp was deleted.
func TestPruneOffsiteKeepsObjectWithMalformedTimestamp(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, _ := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 7, Enabled: true,
	})
	ctx := context.Background()

	// newest: protected by the never-prune-newest rule.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "fresh", Remote: "local", UploadedAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// genuinely past the 7-day horizon: must be pruned.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "stale", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// 30 minutes old, but unparseable. Its text sorts after every valid
	// timestamp, so it is NOT the protected newest row — pre-fix it is parsed as
	// year 1 and deleted as "ancient".
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO offsite_objects (client_id, snapshot_id, remote, bytes, uploaded_at) VALUES (?,?,?,0,?)`,
		id, "recent-bad", "local", "2026-06-18 11:30:00Z"); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}

	off := &fakeOffsite{}
	if err := RunOnce(ctx, Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	want := objectPath(model.Slug("c"), model.ModeTarGz, "stale")
	if len(off.del) != 1 || off.del[0] != want {
		t.Fatalf("pruned %v, want only %q: a row with an unreadable timestamp must never look ancient", off.del, want)
	}
	var rows int
	if err := raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM offsite_objects`).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Fatalf("offsite rows = %d, want 2 (the unreadable row must be kept, not silently dropped)", rows)
	}
}

// Item 3, the other half (round 2): LatestOffsite feeds the upload *schedule*,
// not a deletion. Returning its parse error there reaches offsiteDue, which
// RunOffsiteOnce logs as "schedule check: …" and skips — so a single undateable
// row would silently stop that client's offsite copies from ever being made
// again while the pass still reported clean. The safe direction is to skip the
// row and stay due.
func TestScheduledOffsiteUploadSurvivesCorruptTimestampRow(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		OffsiteCron: "*/5 * * * *", RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	ctx := context.Background()

	// A readable upload two hours ago: the next cron tick is in the past, so the
	// client is due.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "backup-20260618T000001Z-000.tar.gz", Remote: "local", UploadedAt: now.Add(-2 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// A newer row whose timestamp cannot be read. Its text sorts after every
	// RFC 3339 value, so ORDER BY uploaded_at DESC meets it first.
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO offsite_objects (client_id, snapshot_id, remote, bytes, uploaded_at) VALUES (?,?,?,0,?)`,
		id, "backup-20260601T000000Z-000.tar.gz", "local", "99/06/2026 11:30"); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}
	// One snapshot on disk that has never been offsited: the work the schedule
	// exists to trigger.
	mkArchiveContent(t, filepath.Join(dir, "backup-20260618T000002Z-000.tar.gz"), now.Add(-30*time.Minute), "fresh-content")

	off := &fakeOffsite{}
	deps := Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	if len(off.up) != 1 {
		t.Fatalf("uploads = %d (%v), want 1: an unreadable uploaded_at must not stop the client's scheduled "+
			"offsite uploads — that is the silent retention stall this pass exists to remove", len(off.up), off.up)
	}
	for obj := range off.up {
		if !strings.HasSuffix(obj, "/backup-20260618T000002Z-000.tar.gz") {
			t.Errorf("uploaded %q, want the never-offsited snapshot backup-20260618T000002Z-000.tar.gz", obj)
		}
	}
}

// stagingProbe is a cold-storage stub that records, at the moment of each
// upload, how many archives are staged — that is what the caller is about to
// hand it — and where they live.
type stagingProbe struct {
	uploads      int
	leftAtUpload []int
	dirs         []string
}

func (p *stagingProbe) Upload(_ context.Context, local, _, _ string) (int64, error) {
	fi, err := os.Stat(local)
	if err != nil {
		return 0, err
	}
	entries, _ := os.ReadDir(filepath.Dir(local))
	p.uploads++
	p.leftAtUpload = append(p.leftAtUpload, len(entries))
	p.dirs = append(p.dirs, filepath.Dir(local))
	return fi.Size(), nil
}

func (p *stagingProbe) Delete(context.Context, string, string) error { return nil }

func (p *stagingProbe) Lsf(context.Context, string, string) ([]string, error) { return nil, nil }

func mkRsyncSnapshots(t *testing.T, clientDir string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		d := filepath.Join(clientDir, "snapshots", id)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "f.txt"), []byte(id), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Item 4 (staging). rsync archives are staged for upload; pre-fix they were
// written to the system temp dir and every one of them lived until the pass
// ended, so the peak scratch was N × snapshot size instead of one snapshot.
func TestOffsiteStagingIsOnTheBackupVolumeAndRemovedInline(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T080000Z", "20260618T100000Z", "20260618T110000Z")

	probe := &stagingProbe{}
	deps := Deps{Store: st, Offsite: probe, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	if probe.uploads != 3 {
		t.Fatalf("uploads = %d, want 3", probe.uploads)
	}
	wantStage := filepath.Join(base, "staging")
	for i, left := range probe.leftAtUpload {
		if probe.dirs[i] != wantStage {
			t.Errorf("upload %d staged in %q, want the backup volume at %q", i+1, probe.dirs[i], wantStage)
		}
		if left != 1 {
			t.Errorf("upload %d saw %d staged archives, want exactly 1 (removed inline, not at the end of the pass)", i+1, left)
		}
	}
	if entries, err := os.ReadDir(wantStage); err == nil && len(entries) != 0 {
		t.Errorf("staging dir %s still holds %d archives after the pass", wantStage, len(entries))
	}
}

// Item 4, second half: the staging location is configurable.
func TestOffsiteStagingDirIsConfigurable(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("BACKITUP_STAGING_DIR", custom)

	st := openStore(t)
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	_, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeRsync, OffsiteRemote: "local",
		RetentionDays: 14, OffsiteRetentionDays: 90, Enabled: true,
	})
	mkRsyncSnapshots(t, dir, "20260618T110000Z")

	probe := &stagingProbe{}
	deps := Deps{Store: st, Offsite: probe, BackupBaseDir: base, Now: func() time.Time { return now }}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}
	if probe.uploads != 1 {
		t.Fatalf("uploads = %d, want 1", probe.uploads)
	}
	if probe.dirs[0] != custom {
		t.Errorf("staged in %q, want the configured BACKITUP_STAGING_DIR %q", probe.dirs[0], custom)
	}
	if entries, err := os.ReadDir(custom); err != nil || len(entries) != 0 {
		t.Errorf("configured staging dir should be empty after the pass (entries=%d, err=%v)", len(entries), err)
	}
}
