package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

func TestBuildStatusOK(t *testing.T) {
	res := mode.BackupResult{Bytes: 100, Files: 4, SnapshotID: "s1"}
	s := buildStatus(res, nil)
	if s.Status != string(model.StatusOK) || s.Bytes != 100 || s.SnapshotID != "s1" {
		t.Fatalf("ok status wrong: %+v", s)
	}
	if s.FinishedAt.IsZero() || s.StartedAt.IsZero() {
		t.Fatal("timestamps should be backfilled when zero")
	}
}

func TestBuildStatusFailed(t *testing.T) {
	s := buildStatus(mode.BackupResult{}, errors.New("boom"))
	if s.Status != string(model.StatusFailed) {
		t.Fatalf("status = %q, want failed", s.Status)
	}
	// LogTail is set by Run (from the RunLogger buffer), not by buildStatus.
}

// TestRunOverlap exercises the held-lock path: Run should report "overlap" and
// return nil without attempting a backup.
func TestRunOverlap(t *testing.T) {
	var gotStatus string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status" {
			gotStatus = r.URL.Query().Get("x") // unused; just record we were hit
			w.WriteHeader(http.StatusCreated)
			return
		}
		t.Errorf("unexpected request to %s during overlap (no backup should run)", r.URL.Path)
	}))
	defer ts.Close()
	_ = gotStatus

	lockPath := filepath.Join(t.TempDir(), "run.lock")
	// Pre-hold the lock so Run sees an overlap.
	held, _, err := Acquire(lockPath)
	if err != nil {
		t.Fatalf("pre-acquire: %v", err)
	}
	defer held.Release()

	cfg := Config{APIBase: ts.URL, Token: "t", SSHServer: "h:22", SSHKey: "/k", Source: "/s", Mode: model.ModeTarGz, Insecure: true}
	if err := Run(context.Background(), cfg, lockPath); err != nil {
		t.Fatalf("Run on overlap should return nil, got %v", err)
	}
}
