package lifecycle

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// singleModeClient is a tar.gz client on offsite_upload_mode=single: one cold
// object, at a fixed name, replaced by every successful upload.
func singleModeClient(t *testing.T, st *store.Store, name string, retentionDays int) (int64, string, string) {
	t.Helper()
	return newClient(t, st, model.Client{
		Name: name, Mode: model.ModeTarGz, OffsiteRemote: "local",
		OffsiteUploadMode: model.OffsiteUploadSingle,
		RetentionDays:     retentionDays, Enabled: true,
	})
}

// singleObjectKeys returns the object names the fake remote holds, sorted.
func singleObjectKeys(up map[string]int64) []string {
	keys := make([]string, 0, len(up))
	for k := range up {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Two successive uploads must leave exactly one object at the fixed name, one
// offsite_objects row, and the SECOND payload on the remote — i.e. the file the
// restored data comes from is the newest backup, not the first one anybody ever
// uploaded.
func TestSingleModeKeepsOneObjectOneRowAndTheNewestPayload(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	id, base, dir := singleModeClient(t, st, "laptop", 14)
	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)
	ctx := context.Background()

	live := model.ObjectPath("", "laptop", model.SingleObjectName)
	first := filepath.Join(dir, "backup-20261124T090000Z-000.tar.gz")
	mkArchiveContent(t, first, now.Add(-3*time.Hour), "payload-one")

	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	if got := singleObjectKeys(off.up); len(got) != 1 || got[0] != live {
		t.Fatalf("after one upload the remote holds %v, want exactly [%s]", got, live)
	}
	if !bytes.Equal(off.payload[live], mustRead(t, first)) {
		t.Fatal("the fixed object does not hold the first snapshot's bytes")
	}
	if rows := singleRows(t, st, id); len(rows) != 1 || rows[0].SnapshotID != filepath.Base(first) {
		t.Fatalf("offsite rows after the first upload = %+v, want one naming %s", rows, filepath.Base(first))
	}

	second := filepath.Join(dir, "backup-20261124T110000Z-000.tar.gz")
	mkArchiveContent(t, second, now.Add(-1*time.Hour), "payload-two")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	if got := singleObjectKeys(off.up); len(got) != 1 || got[0] != live {
		t.Fatalf("after two uploads the remote holds %v, want one object at the fixed name %s", got, live)
	}
	if !bytes.Equal(off.payload[live], mustRead(t, second)) {
		t.Fatal("the remote still holds the FIRST payload: the second upload did not replace it")
	}
	if len(off.del) != 0 {
		t.Fatalf("a successful replacement deleted %v; the overwrite is a move, not a delete", off.del)
	}
	if len(off.moves) != 2 {
		t.Fatalf("moves = %d, want 2 (one per upload): %v", len(off.moves), off.moves)
	}
	for _, m := range off.moves {
		if !strings.HasSuffix(m[0], model.SingleObjectPartName) || !strings.HasSuffix(m[1], model.SingleObjectName) {
			t.Fatalf("move %v is not staging -> live (%s -> %s)", m, model.SingleObjectPartName, model.SingleObjectName)
		}
	}

	rows := singleRows(t, st, id)
	if len(rows) != 1 {
		t.Fatalf("offsite rows after two uploads = %d, want exactly 1: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.SnapshotID != filepath.Base(second) {
		t.Fatalf("the one row names %s, want the newest snapshot %s", row.SnapshotID, filepath.Base(second))
	}
	want, err := fileChecksum(second)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}
	if row.Checksum != want {
		t.Fatalf("row checksum = %q, want the second archive's %q", row.Checksum, want)
	}
	if row.Bytes != int64(len(mustRead(t, second))) {
		t.Fatalf("row bytes = %d, want the second archive's %d", row.Bytes, len(mustRead(t, second)))
	}
	if row.RemoteMissing {
		t.Fatal("a row written by a successful upload must not claim the object is missing")
	}

	// An unchanged client needs no new generation: the fixed object already holds
	// the newest snapshot. A row left behind by a previous remote must not read as
	// a second generation either — that would make the pass re-upload the same
	// snapshot on every tick, forever.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "backup-20260101T000000Z-000.tar.gz", Remote: "old-remote",
		Bytes: 9, UploadedAt: now.Add(-30 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("record a row for a previous remote: %v", err)
	}
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("pass over an unchanged client: %v", err)
	}
	if len(off.moves) != 2 {
		t.Fatalf("an unchanged client was uploaded again: moves = %d, want 2: %v", len(off.moves), off.moves)
	}
	if !bytes.Equal(off.payload[live], mustRead(t, second)) {
		t.Fatal("a pass with nothing new to tier changed the payload")
	}
	if rows := singleRows(t, st, id); len(rows) != 2 {
		t.Fatalf("rows = %d, want the new generation plus the previous remote's: %+v", len(rows), rows)
	}
}

// The overwrite must not be able to destroy the only copy: a transfer that fails
// leaves the previous object and its row exactly as they were, and a failed move
// replaces nothing while not leaving the staging object behind either.
func TestSingleModeFailedUploadLeavesThePreviousCopyAndRow(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	id, base, dir := singleModeClient(t, st, "laptop", 14)
	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)
	ctx := context.Background()

	live := model.ObjectPath("", "laptop", model.SingleObjectName)
	first := filepath.Join(dir, "backup-20261124T090000Z-000.tar.gz")
	mkArchiveContent(t, first, now.Add(-3*time.Hour), "payload-one")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	second := filepath.Join(dir, "backup-20261124T110000Z-000.tar.gz")
	mkArchiveContent(t, second, now.Add(-1*time.Hour), "payload-two")

	// 1. The transfer itself fails.
	off.fail = true
	if err := RunOffsiteOnce(ctx, deps); err == nil {
		t.Fatal("a failing upload must be reported, not silently skipped")
	}
	assertOnlyPreviousGeneration(t, off, live, first)
	if rows := singleRows(t, st, id); len(rows) != 1 || rows[0].SnapshotID != filepath.Base(first) {
		t.Fatalf("a failed upload changed the row to %+v; it must still name %s", rows, filepath.Base(first))
	}

	// 2. The upload completes but the move that replaces the live object fails.
	off.fail = false
	off.moveErr = errors.New("moveto boom")
	if err := RunOffsiteOnce(ctx, deps); err == nil {
		t.Fatal("a failing move must be reported")
	}
	assertOnlyPreviousGeneration(t, off, live, first)
	if len(off.del) != 1 || !strings.HasSuffix(off.del[0], model.SingleObjectPartName) {
		t.Fatalf("after a failed move the staging object must be removed and the live one left alone; deleted: %v", off.del)
	}
	if rows := singleRows(t, st, id); len(rows) != 1 || rows[0].SnapshotID != filepath.Base(first) {
		t.Fatalf("a failed move changed the row to %+v; it must still name %s", rows, filepath.Base(first))
	}
}

func assertOnlyPreviousGeneration(t *testing.T, off *fakeOffsite, live, first string) {
	t.Helper()
	if got := singleObjectKeys(off.up); len(got) != 1 || got[0] != live {
		t.Fatalf("the remote holds %v; a failed replacement must leave exactly [%s]", got, live)
	}
	if !bytes.Equal(off.payload[live], mustRead(t, first)) {
		t.Fatal("the only cold copy no longer holds the previous generation")
	}
}

// Verification lists the client's directory and looks for the fixed name: when
// the object is removed out of band (the bucket's lifecycle rule, an operator),
// the row must be flagged remote_missing, exactly as a per-snapshot object would
// be. That flag is what stops the row being read as "a copy exists elsewhere".
func TestSingleModeVerificationReportsTheFixedObjectMissing(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	id, base, dir := singleModeClient(t, st, "laptop", 14)
	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)
	ctx := context.Background()

	live := model.ObjectPath("", "laptop", model.SingleObjectName)
	mkArchiveContent(t, filepath.Join(dir, "backup-20261124T090000Z-000.tar.gz"), now.Add(-1*time.Hour), "payload-one")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if rows := singleRows(t, st, id); len(rows) != 1 || rows[0].RemoteMissing {
		t.Fatalf("rows after a successful upload = %+v, want one not flagged missing", rows)
	}

	// Out of band: the object disappears from the bucket.
	if _, ok := off.up[live]; !ok {
		t.Fatalf("fixture: %s was never uploaded: %v", live, singleObjectKeys(off.up))
	}
	if err := off.Delete(ctx, "local", live); err != nil {
		t.Fatalf("delete out of band: %v", err)
	}

	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("lifecycle pass: %v", err)
	}
	rows := singleRows(t, st, id)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	if !rows[0].RemoteMissing {
		t.Fatalf("the fixed object is gone but the row is not flagged remote_missing: %+v", rows[0])
	}
	if rows[0].RemoteVerifiedAt.IsZero() {
		t.Error("the pass must record when it looked, not only what it found")
	}
}

// The prune guard for this mode: hot snapshots STRICTLY OLDER than the snapshot
// the fixed object holds may go (there is a copy elsewhere for the mode's one
// generation only), and everything newer is kept — even when it is past the age
// horizon, because nothing newer than the held copy proves it survives off the
// hot store.
func TestSingleModePruneDeletesOnlyWhatIsOlderThanTheHeldSnapshot(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	id, base, dir := singleModeClient(t, st, "laptop", 1)
	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	ctx := context.Background()

	// A tar.gz client's snapshot age comes from its name, so the names carry it.
	const oldest = "backup-20261015T000000Z-000.tar.gz" // 40 days: past the 1-day horizon
	const held = "backup-20261114T000000Z-000.tar.gz"   // 10 days: what the fixed object will hold
	mkArchiveContent(t, filepath.Join(dir, oldest), now.Add(-40*24*time.Hour), "payload-oldest")
	mkArchiveContent(t, filepath.Join(dir, held), now.Add(-10*24*time.Hour), "payload-held")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if rows := singleRows(t, st, id); len(rows) != 1 || rows[0].SnapshotID != held {
		t.Fatalf("the fixed object must hold %s, rows: %+v", held, rows)
	}

	// Two snapshots arrive after that upload: both newer than the held copy, both
	// past the 1-day horizon.
	const newer = "backup-20261119T000000Z-000.tar.gz"  // 5 days
	const newest = "backup-20261124T110000Z-000.tar.gz" // 1 hour
	mkArchiveContent(t, filepath.Join(dir, newer), now.Add(-5*24*time.Hour), "payload-newer")
	mkArchiveContent(t, filepath.Join(dir, newest), now.Add(-1*time.Hour), "payload-newest")
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("lifecycle pass: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, oldest)); err == nil {
		t.Errorf("%s is older than the held snapshot %s and past the horizon: it should be pruned", oldest, held)
	}
	for _, name := range []string{held, newer, newest} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s must survive: it is the held snapshot or newer than it (%v)", name, err)
		}
	}
	if rows := singleRows(t, st, id); len(rows) != 1 || rows[0].SnapshotID != held {
		t.Fatalf("pruning must not disturb the row: %+v", rows)
	}
}

// With a single-generation client the row cannot say whether the copy is
// current — a failed upload leaves it naming the last successful one. So pruning
// waits for a successful upload: until one lands, nothing is deleted, however far
// past the horizon a snapshot is.
func TestSingleModePruneWaitsForASuccessfulUpload(t *testing.T) {
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	_, base, dir := singleModeClient(t, st, "laptop", 14)
	deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
	off := deps.Offsite.(*fakeOffsite)
	ctx := context.Background()

	const oldest = "backup-20261015T000000Z-000.tar.gz" // 40 days: past the 14-day horizon
	const held = "backup-20261114T000000Z-000.tar.gz"   // 10 days: the last successful upload
	mkArchiveContent(t, filepath.Join(dir, oldest), now.Add(-40*24*time.Hour), "payload-oldest")
	mkArchiveContent(t, filepath.Join(dir, held), now.Add(-10*24*time.Hour), "payload-held")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("upload: %v", err)
	}

	const newest = "backup-20261124T110000Z-000.tar.gz"
	mkArchiveContent(t, filepath.Join(dir, newest), now.Add(-1*time.Hour), "payload-newest")

	// The next upload fails, so the one cold copy still holds `held`.
	logs := captureLog(t)
	off.fail = true
	if err := RunOffsiteOnce(ctx, deps); err == nil {
		t.Fatal("the failing upload must be reported")
	}
	if err := RunOnce(ctx, deps); err != nil {
		t.Fatalf("lifecycle pass: %v", err)
	}

	for _, name := range []string{oldest, held, newest} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s must survive a pass that has no successful upload to rely on (%v)", name, err)
		}
	}
	if !strings.Contains(logs.String(), "the last offsite upload failed") {
		t.Errorf("the pass must say why it kept everything, not keep it silently:\n%s", logs.String())
	}
}

// The other two modes are untouched: their objects are named after the snapshot,
// nothing is moved, and "latest" still uploads one snapshot per pass.
func TestAllAndLatestStillNameTheirObjectsPerSnapshot(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		wantObjs int
	}{
		{model.OffsiteUploadAll, 2},
		{model.OffsiteUploadLatest, 1},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			st := openStore(t)
			now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
			id, base, dir := newClient(t, st, model.Client{
				Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "local",
				OffsiteUploadMode: tc.mode, RetentionDays: 14, Enabled: true,
			})
			deps := Deps{Store: st, Offsite: &fakeOffsite{}, BackupBaseDir: base, Now: func() time.Time { return now }}
			off := deps.Offsite.(*fakeOffsite)
			ctx := context.Background()

			older := "backup-20261124T090000Z-000.tar.gz"
			newer := "backup-20261124T110000Z-000.tar.gz"
			mkArchiveContent(t, filepath.Join(dir, older), now.Add(-3*time.Hour), "payload-a")
			mkArchiveContent(t, filepath.Join(dir, newer), now.Add(-1*time.Hour), "payload-b")
			if err := RunOffsiteOnce(ctx, deps); err != nil {
				t.Fatalf("offsite pass: %v", err)
			}

			if len(off.up) != tc.wantObjs {
				t.Fatalf("%s mode uploaded %d objects, want %d: %v", tc.mode, len(off.up), tc.wantObjs, singleObjectKeys(off.up))
			}
			for _, obj := range singleObjectKeys(off.up) {
				if strings.Contains(obj, model.SingleObjectName) {
					t.Errorf("%s mode wrote the single-generation object name %q", tc.mode, obj)
				}
				if !strings.HasSuffix(obj, ".tar.gz") || strings.Contains(obj, "latest") {
					t.Errorf("%s mode must keep naming objects after their snapshot, got %q", tc.mode, obj)
				}
			}
			if len(off.moves) != 0 {
				t.Errorf("%s mode moved an object (%v); only single-generation replaces a name", tc.mode, off.moves)
			}
			if rows := singleRows(t, st, id); len(rows) != tc.wantObjs {
				t.Errorf("%s mode rows = %d, want %d: %+v", tc.mode, len(rows), tc.wantObjs, rows)
			}
		})
	}
}

// The order of rclone calls, which is the whole safety argument: the archive goes
// up under the staging name, and only a successful transfer is followed by the
// move that replaces the live object. Nothing is ever copied straight onto
// latest.tar.gz, so a half-finished upload cannot be what the client restores
// from.
func TestSingleModeUploadsToTheStagingNameThenMovesItIntoPlace(t *testing.T) {
	calls := installRecordingRclone(t)
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "offsite", Backend: model.BackendB2,
		Config:    map[string]string{"account": "keyid", "key": "appkey", "bucket": "my-backups", "path": "backups"},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	_, base, dir := newClient(t, st, model.Client{
		Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "offsite",
		OffsiteUploadMode: model.OffsiteUploadSingle, RetentionDays: 14, Enabled: true,
	})
	archive := filepath.Join(dir, "backup-20261124T110000Z-000.tar.gz")
	mkArchive(t, archive, now.Add(-time.Hour))

	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(filepath.Join(t.TempDir(), "rclone.conf")),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("offsite pass: %v", err)
	}

	log := strings.TrimSpace(rcloneCalls(t, calls))
	lines := strings.Split(log, "\n")
	if len(lines) != 2 {
		t.Fatalf("a single-generation upload must issue exactly two rclone calls (copyto, moveto), got %d:\n%s", len(lines), log)
	}
	live := "offsite:my-backups/backups/laptop/" + model.SingleObjectName
	part := live + ".part"
	if want := "copyto " + archive + " " + part; !strings.Contains(lines[0], want) {
		t.Fatalf("first call is not the upload to the staging name;\nwant an invocation containing: %s\ngot: %s", want, lines[0])
	}
	if want := "moveto " + part + " " + live; !strings.HasSuffix(lines[1], want) {
		t.Fatalf("second call is not the move onto the live name;\nwant an invocation ending with: %s\ngot: %s", want, lines[1])
	}
}

// singleRows is every offsite_objects row for a client.
func singleRows(t *testing.T, st *store.Store, clientID int64) []model.OffsiteObject {
	t.Helper()
	rows, err := st.ListOffsiteObjects(context.Background(), clientID)
	if err != nil {
		t.Fatalf("list offsite objects: %v", err)
	}
	return rows
}

// TestSingleModeEndToEndThroughTheLocalBackend is the same proof against a real
// rclone and a real (encrypted) object store: two uploads through the fixed name
// leave one object on the provider, it decrypts to the SECOND archive, and the
// name the download route resolves is the one that exists.
//
// It skips when rclone is not installed, like the other end-to-end proof.
func TestSingleModeEndToEndThroughTheLocalBackend(t *testing.T) {
	rcloneBin, err := exec.LookPath("rclone")
	if err != nil {
		t.Skip("rclone is not on PATH: install it to run the single-generation end-to-end proof")
	}
	st := openStore(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	providerDir := filepath.Join(t.TempDir(), "provider")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatalf("mkdir provider: %v", err)
	}
	obscure := func(secret string) string {
		t.Helper()
		out, err := exec.Command(rcloneBin, "obscure", secret).Output()
		if err != nil {
			t.Fatalf("rclone obscure: %v", err)
		}
		return strings.TrimSpace(string(out))
	}
	plaintext := model.Remote{
		Name: "fakecloud", Backend: model.BackendLocal,
		Config:    map[string]string{"bucket": providerDir},
		CreatedAt: now,
	}
	vault := model.Remote{
		Name: "vault", Backend: model.BackendCrypt,
		Config: map[string]string{
			"remote":                    "fakecloud",
			"filename_encryption":       "standard",
			"directory_name_encryption": "true",
			"password":                  obscure("single-mode-password"),
			"password2":                 obscure("single-mode-salt"),
		},
		CreatedAt: now,
	}
	for _, r := range []model.Remote{plaintext, vault} {
		if err := st.CreateRemote(t.Context(), r); err != nil {
			t.Fatalf("create remote %q: %v", r.Name, err)
		}
	}
	confPath := filepath.Join(t.TempDir(), "rclone.conf")
	byName := map[string]model.Remote{plaintext.Name: plaintext, vault.Name: vault}
	lookup := func(name string) (model.Remote, bool) { r, ok := byName[name]; return r, ok }
	if err := os.WriteFile(confPath, []byte(plaintext.RcloneSectionFor(lookup)+"\n"+vault.RcloneSectionFor(lookup)), 0o600); err != nil {
		t.Fatalf("write rclone.conf: %v", err)
	}

	id, base, dir := newClient(t, st, model.Client{
		Name: "laptop", Mode: model.ModeTarGz, OffsiteRemote: "vault",
		OffsiteUploadMode: model.OffsiteUploadSingle, RetentionDays: 14, Enabled: true,
	})
	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(confPath),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	ctx := context.Background()

	first := filepath.Join(dir, "backup-20261124T090000Z-000.tar.gz")
	mkArchiveContent(t, first, now.Add(-3*time.Hour), "PAYLOAD-MARKER-first")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("first upload: %v", err)
	}
	second := filepath.Join(dir, "backup-20261124T110000Z-000.tar.gz")
	mkArchiveContent(t, second, now.Add(-1*time.Hour), "PAYLOAD-MARKER-second")
	if err := RunOffsiteOnce(ctx, deps); err != nil {
		t.Fatalf("second upload: %v", err)
	}

	// What the provider really holds: one object, no staging leftovers.
	var files []string
	if err := filepath.WalkDir(providerDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, p)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk provider: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("the provider holds %d objects, want exactly 1 (the fixed name, replaced): %v", len(files), files)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read provider object: %v", err)
	}
	if bytes.Contains(raw, []byte("PAYLOAD-MARKER")) {
		t.Fatal("the provider holds the backup in the clear: the crypt remote is not in effect")
	}

	// The name the download route resolves is the one that exists, and it
	// decrypts to the newest snapshot — not the first one ever uploaded.
	objectPath := model.ObjectPath("", "laptop", model.SingleObjectName)
	out, err := exec.Command(rcloneBin, "--config", confPath, "cat", "vault:"+objectPath).Output()
	if err != nil {
		t.Fatalf("rclone cat vault:%s: %v", objectPath, err)
	}
	if want := mustRead(t, second); !bytes.Equal(out, want) {
		t.Fatalf("%s decrypts to %d bytes, want the second archive's %d bytes", objectPath, len(out), len(want))
	}

	rows := singleRows(t, st, id)
	if len(rows) != 1 || rows[0].SnapshotID != filepath.Base(second) {
		t.Fatalf("rows = %+v, want one naming %s", rows, filepath.Base(second))
	}
}
