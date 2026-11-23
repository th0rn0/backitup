package lifecycle

import (
	"context"
	"database/sql"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// Offsite retention is no longer backitup's job: cold storage is Backblaze B2 and
// the bucket's lifecycle rule deletes objects. The property this card buys is
// negative and total — a lifecycle pass deletes no offsite object, at any age,
// with any horizon, under any flag — so the test seeds the ages the removed
// pruner would have eaten (past a plausible horizon, past a year, and one whose
// uploaded_at cannot be parsed at all) and asserts the row count is identical
// before and after.
func TestRunOnceDeletesNoOffsiteObjectWhateverItsAge(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	id, base, dir := newClient(t, st, model.Client{
		Name: "c", Mode: model.ModeTarGz, OffsiteRemote: "local",
		RetentionDays: 14, Enabled: true,
	})
	// Two hot snapshots so the hot pruner runs in the same pass: hot retention
	// must keep working while offsite deletion is gone.
	mkArchive(t, filepath.Join(dir, "backup-20260509T000000Z-000.tar.gz"), now.Add(-40*24*time.Hour))
	mkArchive(t, filepath.Join(dir, "backup-20260618T000000Z-000.tar.gz"), now.Add(-time.Hour))

	// The two hot snapshots as the offsite worker recorded them, so the hot
	// pruner has something it is allowed to delete (offsite-first). The fake
	// remote holds exactly these two objects: the lifecycle pass reconciles the
	// records with what the remote reports *before* it prunes, so a row whose
	// object is not there is (correctly) not permission to delete the hot copy.
	// The seeded rows above describe a differently aged narrative and are not
	// expected to be found.
	off := &fakeOffsite{up: map[string]int64{
		"c/backup-20260509T000000Z-000.tar.gz": 1,
		"c/backup-20260618T000000Z-000.tar.gz": 1,
	}}
	for _, o := range []model.OffsiteObject{
		{ClientID: id, SnapshotID: "year-old", Remote: "local", UploadedAt: now.Add(-400 * 24 * time.Hour)},
		{ClientID: id, SnapshotID: "past-any-horizon", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour)},
		{ClientID: id, SnapshotID: "just-past-a-week", Remote: "local", UploadedAt: now.Add(-8 * 24 * time.Hour)},
		{ClientID: id, SnapshotID: "fresh", Remote: "local", UploadedAt: now.Add(-time.Hour)},
		{ClientID: id, SnapshotID: "deduped", Remote: "local", DedupOf: "past-any-horizon", UploadedAt: now.Add(-7 * 24 * time.Hour)},
		// The two hot snapshots as the offsite worker recorded them, so the hot
		// pruner has something it is allowed to delete (offsite-first).
		{ClientID: id, SnapshotID: "backup-20260509T000000Z-000.tar.gz", Remote: "local", UploadedAt: now.Add(-40 * 24 * time.Hour)},
		{ClientID: id, SnapshotID: "backup-20260618T000000Z-000.tar.gz", Remote: "local", UploadedAt: now.Add(-time.Hour)},
	} {
		if err := st.RecordOffsiteObject(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	// A record whose timestamp the store cannot parse. The removed pruner read it
	// as year 1 and called the object ancient; whatever else happens, no code path
	// may read this row as permission to delete anything.
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO offsite_objects (client_id, snapshot_id, remote, bytes, uploaded_at) VALUES (?,?,?,0,?)`,
		id, "unreadable-timestamp", "local", "2026-06-18 11:30:00Z"); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}

	before := offsiteSnapshots(t, raw)
	if len(before) != 8 {
		t.Fatalf("seeded %d offsite rows, want 8: %v", len(before), before)
	}

	if err := RunOnce(ctx, Deps{Store: st, Offsite: off, BackupBaseDir: base, Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(off.del) != 0 {
		t.Fatalf("a lifecycle pass deleted %v; offsite retention is the B2 bucket's rule, so offsite rows must stay at %d", off.del, len(before))
	}
	after := offsiteSnapshots(t, raw)
	if len(after) != len(before) {
		t.Fatalf("offsite rows: before=%d after=%d\nbefore: %v\nafter:  %v", len(before), len(after), before, after)
	}
	for _, snap := range before {
		if !slices.Contains(after, snap) {
			t.Fatalf("offsite record %q disappeared from the database (before=%d rows, after=%d rows)", snap, len(before), len(after))
		}
	}

	// And the hot half still pruned, so "deletes nothing" above is not the pass
	// having quietly stopped working.
	if _, err := os.Stat(filepath.Join(dir, "backup-20260509T000000Z-000.tar.gz")); err == nil {
		t.Error("the old hot snapshot should still be pruned by hot retention")
	}
}

// offsiteSnapshots lists the snapshot ids recorded in offsite_objects, sorted, so
// a before/after comparison is a property of the table rather than of row order.
func offsiteSnapshots(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT snapshot_id FROM offsite_objects ORDER BY snapshot_id`)
	if err != nil {
		t.Fatalf("list offsite rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan offsite row: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list offsite rows: %v", err)
	}
	return out
}

// The startup line exists so "why is B2 not shrinking?" is answered by the log
// rather than by a debugging session. Two things are asserted: the exact wording
// (the card fixes it, and a reworded line is no longer the answer an operator was
// told to look for) and that a process prints it once, however many times the
// startup path asks — the server starts two workers, and a line per worker would
// read as a recurring event.
func TestLogOffsiteRetentionDelegatedSaysItOncePerProcess(t *testing.T) {
	const want = "lifecycle: offsite retention is delegated to the B2 bucket lifecycle rule; backitup will not delete offsite objects"
	if OffsiteRetentionDelegatedMessage != want {
		t.Fatalf("the delegation line changed:\nwant: %s\ngot:  %s", want, OffsiteRetentionDelegatedMessage)
	}

	offsiteRetentionDelegatedOnce = sync.Once{} // a fresh process
	logs := &syncBuffer{}
	prev := log.Writer()
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	LogOffsiteRetentionDelegated()
	LogOffsiteRetentionDelegated()

	if got := strings.Count(logs.String(), want); got != 1 {
		t.Fatalf("the line was printed %d times, want exactly 1:\n%s", got, logs.String())
	}
}
