package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

func TestClientAndRunRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	id, err := st.CreateClient(ctx, model.Client{
		Name:                 "laptop-docs",
		Mode:                 model.ModeRsync,
		Excludes:             []string{"*.tmp", "node_modules/"},
		RetentionDays:        14,
		OffsiteRetentionDays: 90,
		ExpectedIntervalSecs: 3600,
		OffsiteRemote:        "s3",
		Enabled:              true,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	clients, err := st.ListClients(ctx)
	if err != nil {
		t.Fatalf("list clients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("got %d clients, want 1", len(clients))
	}
	c := clients[0]
	if c.Mode != model.ModeRsync || c.OffsiteRetentionDays != 90 || len(c.Excludes) != 2 {
		t.Fatalf("client round-trip mismatch: %+v", c)
	}

	// No runs yet -> LatestRun returns nil -> Health "never".
	latest, err := st.LatestRun(ctx, id)
	if err != nil {
		t.Fatalf("latest run (empty): %v", err)
	}
	if h := model.DeriveHealth(latest, time.Hour, time.Now()); h != model.HealthNever {
		t.Fatalf("empty client health = %q, want never", h)
	}

	now := time.Now().UTC()
	if _, err := st.RecordRun(ctx, model.Run{
		ClientID: id, StartedAt: now.Add(-time.Minute), FinishedAt: now,
		Status: model.StatusOK, Bytes: 4200, Files: 12, SnapshotID: "2026-06-18T12-00-00Z-001",
	}); err != nil {
		t.Fatalf("record run: %v", err)
	}
	latest, err = st.LatestRun(ctx, id)
	if err != nil || latest == nil {
		t.Fatalf("latest run: %v (nil=%v)", err, latest == nil)
	}
	if latest.Bytes != 4200 || latest.Status != model.StatusOK {
		t.Fatalf("run round-trip mismatch: %+v", latest)
	}
	if h := model.DeriveHealth(latest, time.Hour, time.Now()); h != model.HealthOK {
		t.Fatalf("fresh ok run health = %q, want ok", h)
	}
}

func TestGetClient(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// Not found -> (nil, nil).
	got, err := st.GetClient(ctx, 123)
	if err != nil || got != nil {
		t.Fatalf("GetClient(missing) = %v, %v; want nil, nil", got, err)
	}

	id, err := st.CreateClient(ctx, model.Client{Name: "g", Mode: model.ModeTarGz, RetentionDays: 9, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err = st.GetClient(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("GetClient = %v, %v", got, err)
	}
	if got.Name != "g" || got.RetentionDays != 9 {
		t.Fatalf("GetClient mismatch: %+v", got)
	}
}

func TestRotateClientCreds(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	id, err := st.CreateClient(ctx, model.Client{
		Name: "rotate-me", Mode: model.ModeTarGz, RetentionDays: 7,
		SSHPubKey: "ssh-ed25519 AAAA old", TokenHash: "oldhash", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	cl0, err := st.GetClient(ctx, id)
	if err != nil || cl0 == nil {
		t.Fatalf("get before rotate: %v", err)
	}
	if err := st.RotateClientCreds(ctx, id, "ssh-ed25519 BBBB new", "newhash", "prefix12", cl0.Version); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	got, err := st.GetClient(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("get after rotate: %v, nil=%v", err, got == nil)
	}
	if got.SSHPubKey != "ssh-ed25519 BBBB new" || got.TokenHash != "newhash" || got.TokenPrefix != "prefix12" {
		t.Fatalf("creds not updated: pubkey=%q hash=%q prefix=%q", got.SSHPubKey, got.TokenHash, got.TokenPrefix)
	}
	// Other fields must be untouched.
	if got.Name != "rotate-me" || got.RetentionDays != 7 {
		t.Fatalf("unrelated fields clobbered: %+v", got)
	}
	// Version must have incremented.
	if got.Version != cl0.Version+1 {
		t.Fatalf("version not incremented: want %d, got %d", cl0.Version+1, got.Version)
	}

	// Stale version must return ErrConflict.
	if err := st.RotateClientCreds(ctx, id, "key", "hash", "pfx", cl0.Version); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: want ErrConflict, got %v", err)
	}

	// Rotating a nonexistent client must return an error.
	if err := st.RotateClientCreds(ctx, 9999, "key", "hash", "pfx", 1); err == nil {
		t.Fatal("expected error for missing client, got nil")
	}
}

func TestDeleteClient(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	id, err := st.CreateClient(ctx, model.Client{Name: "del-me", Mode: model.ModeTarGz, Enabled: true})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := st.DeleteClient(ctx, id); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Client must be gone.
	got, err := st.GetClient(ctx, id)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil after delete, got %+v", got)
	}

	// Deleting a nonexistent client must return an error.
	if err := st.DeleteClient(ctx, 9999); err == nil {
		t.Fatal("expected error for missing client, got nil")
	}
}

func TestUniqueClientName(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	c := model.Client{Name: "dup", Mode: model.ModeTarGz, Enabled: true}
	if _, err := st.CreateClient(ctx, c); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if _, err := st.CreateClient(ctx, c); err == nil {
		t.Fatal("expected UNIQUE violation on duplicate client name, got nil")
	}
}
