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

	// LatestOffsite drives the per-client upload schedule, so a corrupt row must
	// be skipped rather than turned into an error: an error there is logged by
	// RunOffsiteOnce and the client's offsite uploads stop silently. The newest
	// readable row is the answer.
	last, err := st.LatestOffsite(ctx, id)
	if err != nil {
		t.Fatalf("LatestOffsite: %v (a corrupt row must be skipped, not reported as an error that stops that client's uploads)", err)
	}
	if last == nil {
		t.Fatalf("LatestOffsite = nil, want the readable row %v", good)
	}
	if !last.Equal(good) {
		t.Fatalf("LatestOffsite = %v, want %v (the corrupt row must be skipped, wherever it sorts)", last, good)
	}

	// With only a corrupt row left there is no readable time at all: (nil, nil)
	// reads as "never offsited", i.e. due now — the safe direction, and the
	// dedupe path makes the extra upload cheap.
	if _, err := st.db.Exec(`DELETE FROM offsite_objects WHERE snapshot_id='good'`); err != nil {
		t.Fatal(err)
	}
	last, err = st.LatestOffsite(ctx, id)
	if err != nil {
		t.Fatalf("LatestOffsite with only a corrupt row: %v", err)
	}
	if last != nil {
		t.Fatalf("LatestOffsite = %v, want nil (nothing readable reads as never offsited)", last)
	}
}
