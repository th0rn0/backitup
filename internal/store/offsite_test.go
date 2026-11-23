package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

func TestOffsiteObjects(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	id, _ := st.CreateClient(ctx, model.Client{Name: "c", Mode: model.ModeRsync, Enabled: true})

	// None yet.
	if lo, _ := st.LatestOffsite(ctx, id); lo != nil {
		t.Fatal("expected nil latest offsite")
	}

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	_ = st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s-old", Remote: "r", Bytes: 10, UploadedAt: old})
	_ = st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s-new", Remote: "r", Bytes: 20})

	objs, err := st.ListOffsiteObjects(ctx, id)
	if err != nil || len(objs) != 2 {
		t.Fatalf("list = %d objs, err %v", len(objs), err)
	}
	if objs[0].SnapshotID != "s-new" { // newest first
		t.Fatalf("expected s-new first, got %s", objs[0].SnapshotID)
	}
	lo, _ := st.LatestOffsite(ctx, id)
	if lo == nil || lo.Before(old.Add(time.Hour)) {
		t.Fatalf("latest offsite should be the recent one: %v", lo)
	}

	// Idempotent upsert (same client+snapshot+remote).
	_ = st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s-new", Remote: "r", Bytes: 99})
	objs, _ = st.ListOffsiteObjects(ctx, id)
	if len(objs) != 2 {
		t.Fatalf("upsert created a dup: %d", len(objs))
	}

	_ = st.DeleteOffsiteObject(ctx, id, "s-old", "r")
	objs, _ = st.ListOffsiteObjects(ctx, id)
	if len(objs) != 1 || objs[0].SnapshotID != "s-new" {
		t.Fatalf("delete failed: %+v", objs)
	}
}

func TestRecordOffsiteObjectClearsAVerificationFinding(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	id, _ := st.CreateClient(ctx, model.Client{Name: "c", Mode: model.ModeTarGz, Enabled: true})

	// A row the verification pass reported missing.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s", Remote: "r", Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	verifiedAt := time.Now().UTC().Truncate(time.Second)
	if err := st.UpdateOffsiteRemoteStatus(ctx, offsiteRowIDFor(t, st, id, "s"), true, verifiedAt); err != nil {
		t.Fatal(err)
	}
	if o := onlyOffsiteObject(t, st, id); !o.RemoteMissing || o.RemoteVerifiedAt.IsZero() {
		t.Fatalf("the finding was not recorded: %+v", o)
	}

	// Re-recording means the content was just put on the remote (a re-upload, or a
	// dedupe whose owner is present now). The old finding describes an object this
	// row no longer describes, and leaving it set would make the snapshot
	// permanently un-offsited: never prunable, re-uploaded every pass.
	if err := st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s", Remote: "r", Bytes: 1}); err != nil {
		t.Fatal(err)
	}
	if o := onlyOffsiteObject(t, st, id); o.RemoteMissing || !o.RemoteVerifiedAt.IsZero() {
		t.Fatalf("a re-recorded object still carries the previous verification finding: %+v", o)
	}
}

func offsiteRowIDFor(t *testing.T, st *Store, clientID int64, snapshotID string) int64 {
	t.Helper()
	objs, err := st.ListOffsiteObjects(context.Background(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objs {
		if o.SnapshotID == snapshotID {
			return o.ID
		}
	}
	t.Fatalf("no offsite row for %s", snapshotID)
	return 0
}

func onlyOffsiteObject(t *testing.T, st *Store, clientID int64) model.OffsiteObject {
	t.Helper()
	objs, err := st.ListOffsiteObjects(context.Background(), clientID)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 {
		t.Fatalf("offsite rows = %d, want 1: %+v", len(objs), objs)
	}
	return objs[0]
}

func TestPruneRuns(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	id, _ := st.CreateClient(ctx, model.Client{Name: "c", Mode: model.ModeTarGz, Enabled: true})

	now := time.Now().UTC()
	_, _ = st.RecordRun(ctx, model.Run{ClientID: id, StartedAt: now.Add(-200 * 24 * time.Hour), FinishedAt: now.Add(-200 * 24 * time.Hour), Status: model.StatusOK})
	_, _ = st.RecordRun(ctx, model.Run{ClientID: id, StartedAt: now, FinishedAt: now, Status: model.StatusOK})

	n, err := st.PruneRuns(ctx, id, 90)
	if err != nil || n != 1 {
		t.Fatalf("prune deleted %d (err %v), want 1", n, err)
	}
	// keepDays 0 = no-op.
	if n, _ := st.PruneRuns(ctx, id, 0); n != 0 {
		t.Fatalf("keepDays 0 should delete nothing, deleted %d", n)
	}
	latest, _ := st.LatestRun(ctx, id)
	if latest == nil {
		t.Fatal("recent run should survive prune")
	}
}
