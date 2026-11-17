package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// An unreadable uploaded_at must never be turned into a zero time. Offsite
// pruning deletes anything "older than the horizon", and time.Time{} is year 1,
// so a corrupt row used to make a recent remote object look ancient.
func TestUnparseableOffsiteTimestampFailsClosed(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	id, _ := st.CreateClient(ctx, model.Client{Name: "c", Mode: model.ModeTarGz, Enabled: true})

	good := time.Date(2026, 6, 18, 11, 0, 0, 0, time.UTC)
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: id, SnapshotID: "good", Remote: "r", UploadedAt: good,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO offsite_objects (client_id, snapshot_id, remote, bytes, uploaded_at)
		VALUES (?,?,?,0,?)`, id, "bad", "r", "18/06/2026 11:30"); err != nil {
		t.Fatalf("insert corrupt row: %v", err)
	}

	objs, err := st.ListOffsiteObjects(ctx, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(objs) != 1 || objs[0].SnapshotID != "good" {
		t.Fatalf("list = %+v, want only the readable row: the corrupt one must be skipped, never dated year 1", objs)
	}

	// With only a corrupt row left, LatestOffsite reports the error instead of a
	// zero time that would look like "never uploaded" or "uploaded in year 1".
	if _, err := st.db.Exec(`DELETE FROM offsite_objects WHERE snapshot_id='good'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LatestOffsite(ctx, id); err == nil {
		t.Fatal("LatestOffsite swallowed an unparseable uploaded_at")
	}
}
