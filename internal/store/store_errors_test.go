package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

func TestOpenBadPath(t *testing.T) {
	// A path under a nonexistent directory cannot be created.
	_, err := Open(filepath.Join(t.TempDir(), "no-such-dir", "x.db"))
	if err == nil {
		t.Fatal("expected error opening db in nonexistent directory, got nil")
	}
}

func TestDisabledClientRoundTrip(t *testing.T) {
	// Exercises the Enabled=false path (b2i false) and its round trip.
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
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
	defer st.Close()

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
	defer st.Close()
	clients, err := st.ListClients(context.Background())
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(clients) != 0 {
		t.Fatalf("expected 0 clients, got %d", len(clients))
	}
}
