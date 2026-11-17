package server

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// healthStack builds a Server + store with one client so health can be observed
// through the real dashboard path (buildDashboard) without going through HTTP.
func healthStack(t *testing.T, name string, mode model.Mode, intervalSecs int) (*store.Store, *Server, int64) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	id, err := st.CreateClient(context.Background(), model.Client{
		Name: name, Mode: mode, RetentionDays: 14,
		ExpectedIntervalSecs: intervalSecs, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return st, New(st, false), id
}

func recordRunAt(t *testing.T, st *store.Store, id int64, status model.RunStatus, ago time.Duration) {
	t.Helper()
	fin := time.Now().UTC().Add(-ago)
	if _, err := st.RecordRun(context.Background(), model.Run{
		ClientID: id, StartedAt: fin.Add(-time.Minute), FinishedAt: fin,
		Status: status, Bytes: 4096, Files: 3, SnapshotID: "20260101T000000Z",
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}
}

func dashboardRow(t *testing.T, srv *Server, slug string) clientRow {
	t.Helper()
	v, err := srv.buildDashboard(context.Background())
	if err != nil {
		t.Fatalf("buildDashboard: %v", err)
	}
	for _, row := range v.Clients {
		if row.Slug == slug {
			return row
		}
	}
	t.Fatalf("client %q missing from dashboard", slug)
	return clientRow{}
}

// S8: a client whose recent history is nothing but overlaps read "ok / just
// now", because DeriveHealth keyed off the last ROW and every overlap row
// refreshed the clock. Overlap is the symptom of "backup slower than its cron"
// — the one case the dashboard must not paper over. Health has to age from the
// last SUCCESSFUL run.
func TestOverlapOnlyHistoryDoesNotReadHealthy(t *testing.T) {
	st, srv, id := healthStack(t, "laptop-docs", model.ModeRsync, 3600) // 1h cadence

	// The last real backup finished five hours ago (2x cadence -> stale).
	recordRunAt(t, st, id, model.StatusOK, 5*time.Hour)
	// Then three overlapping cron triggers, the newest a minute ago. Each is a
	// real row with a fresh timestamp; none of them is a backup.
	for i := 3; i >= 1; i-- {
		recordRunAt(t, st, id, model.StatusOverlap, time.Duration(i)*time.Minute)
	}

	row := dashboardRow(t, srv, "laptop-docs")
	if row.Health == string(model.HealthOK) {
		t.Fatalf("health = %q after three overlap runs and no successful backup for 5h; want stale", row.Health)
	}
	if row.Health != string(model.HealthStale) {
		t.Fatalf("health = %q, want %q", row.Health, model.HealthStale)
	}
	// The row must not present the newest overlap as a backup either.
	if strings.Contains(row.LastBackup, "just now") {
		t.Fatalf("LastBackup = %q presents an overlap as a fresh backup", row.LastBackup)
	}
	if !strings.Contains(row.LastBackup, "overlap") {
		t.Fatalf("LastBackup = %q, want it to name the overlap", row.LastBackup)
	}
}

// A client that has ONLY ever overlapped has still never backed anything up:
// "never" is the honest reading, not "ok".
func TestOverlapOnlyHistoryWithNoSuccessEverReadsNever(t *testing.T) {
	st, srv, id := healthStack(t, "never-backed-up", model.ModeTarGz, 3600)
	recordRunAt(t, st, id, model.StatusOverlap, 2*time.Minute)

	if row := dashboardRow(t, srv, "never-backed-up"); row.Health != string(model.HealthNever) {
		t.Fatalf("health = %q for overlap-only history, want %q", row.Health, model.HealthNever)
	}
}

// Regression guard: a successful run inside the cadence is still "ok" even when
// an overlap follows it (the backfill is simply still going).
func TestRecentSuccessStaysOKDespiteTrailingOverlap(t *testing.T) {
	st, srv, id := healthStack(t, "still-fine", model.ModeRsync, 3600)
	recordRunAt(t, st, id, model.StatusOK, 10*time.Minute)
	recordRunAt(t, st, id, model.StatusOverlap, time.Minute)

	if row := dashboardRow(t, srv, "still-fine"); row.Health != string(model.HealthOK) {
		t.Fatalf("health = %q, want %q", row.Health, model.HealthOK)
	}
}
