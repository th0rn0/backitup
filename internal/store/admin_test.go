package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

func TestAdminRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// No admin yet.
	a, err := st.GetAdmin(ctx)
	if err != nil {
		t.Fatalf("get admin (empty): %v", err)
	}
	if a != nil {
		t.Fatalf("expected nil admin, got %+v", a)
	}

	// Set, then read back.
	if err := st.SetAdmin(ctx, model.Admin{Username: "admin", PasswordHash: "hash-1"}); err != nil {
		t.Fatalf("set admin: %v", err)
	}
	a, err = st.GetAdmin(ctx)
	if err != nil || a == nil {
		t.Fatalf("get admin: %v (nil=%v)", err, a == nil)
	}
	if a.Username != "admin" || a.PasswordHash != "hash-1" {
		t.Fatalf("admin mismatch: %+v", a)
	}

	// Upsert overwrites (password reset), id stays 1 (single admin).
	if err := st.SetAdmin(ctx, model.Admin{Username: "admin2", PasswordHash: "hash-2"}); err != nil {
		t.Fatalf("update admin: %v", err)
	}
	a, _ = st.GetAdmin(ctx)
	if a.Username != "admin2" || a.PasswordHash != "hash-2" {
		t.Fatalf("admin not updated: %+v", a)
	}
}
