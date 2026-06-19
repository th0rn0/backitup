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
	defer st.Close()
	ctx := context.Background()
	id, _ := st.CreateClient(ctx, model.Client{Name: "c", Mode: model.ModeRsync, Enabled: true})

	// None yet.
	if lo, _ := st.LatestOffsite(ctx, id); lo != nil {
		t.Fatal("expected nil latest offsite")
	}

	old := time.Now().UTC().Add(-40 * 24 * time.Hour)
	st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s-old", Remote: "r", Bytes: 10, UploadedAt: old})
	st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s-new", Remote: "r", Bytes: 20})

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
	st.RecordOffsiteObject(ctx, model.OffsiteObject{ClientID: id, SnapshotID: "s-new", Remote: "r", Bytes: 99})
	objs, _ = st.ListOffsiteObjects(ctx, id)
	if len(objs) != 2 {
		t.Fatalf("upsert created a dup: %d", len(objs))
	}

	st.DeleteOffsiteObject(ctx, id, "s-old", "r")
	objs, _ = st.ListOffsiteObjects(ctx, id)
	if len(objs) != 1 || objs[0].SnapshotID != "s-new" {
		t.Fatalf("delete failed: %+v", objs)
	}
}

func TestPruneRuns(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	id, _ := st.CreateClient(ctx, model.Client{Name: "c", Mode: model.ModeTarGz, Enabled: true})

	now := time.Now().UTC()
	st.RecordRun(ctx, model.Run{ClientID: id, StartedAt: now.Add(-200 * 24 * time.Hour), FinishedAt: now.Add(-200 * 24 * time.Hour), Status: model.StatusOK})
	st.RecordRun(ctx, model.Run{ClientID: id, StartedAt: now, FinishedAt: now, Status: model.StatusOK})

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
