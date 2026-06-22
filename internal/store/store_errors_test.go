package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

func TestOpenBadPath(t *testing.T) {
	// A path whose parent component is an existing *file* (not a dir) cannot be
	// opened — os.MkdirAll fails with ENOTDIR, not silently creates it.
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte{}, 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := Open(filepath.Join(blocker, "x.db"))
	if err == nil {
		t.Fatal("expected error when parent path is a file, got nil")
	}
}

func TestDisabledClientRoundTrip(t *testing.T) {
	// Exercises the Enabled=false path (b2i false) and its round trip.
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if _, err := st.CreateClient(ctx, model.Client{Name: "off", Mode: model.ModeTarGz, Enabled: false}); err != nil {
		t.Fatalf("create: %v", err)
	}
	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(clients) != 1 || clients[0].Enabled {
		t.Fatalf("expected one disabled client, got %+v", clients)
	}
}

func TestRecordRunForeignKey(t *testing.T) {
	// foreign_keys is ON; a run for a nonexistent client must be rejected
	// rather than orphaned.
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()

	_, err = st.RecordRun(context.Background(), model.Run{
		ClientID: 9999, StartedAt: time.Now(), FinishedAt: time.Now(), Status: model.StatusOK,
	})
	if err == nil {
		t.Fatal("expected foreign-key error recording run for missing client, got nil")
	}
}

func TestListClientsEmpty(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	clients, err := st.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("expected 0 clients, got %d", len(clients))
	}
}
