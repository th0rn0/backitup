package mode

import (
	"context"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// fakeClientMode / fakeServerMode exercise the registry without a real backend.
type fakeClientMode struct{ m model.Mode }

func (f fakeClientMode) Mode() model.Mode { return f.m }
func (f fakeClientMode) Backup(context.Context, BackupOpts) (BackupResult, error) {
	return BackupResult{SnapshotID: "snap-1", FinishedAt: time.Unix(0, 0)}, nil
}

type fakeServerMode struct{ m model.Mode }

func (f fakeServerMode) Mode() model.Mode { return f.m }
func (f fakeServerMode) List(context.Context, string) ([]Snapshot, error) {
	return nil, ErrNotImplemented
}
func (f fakeServerMode) PrepareOffsite(context.Context, string, Snapshot) (string, error) {
	return "", ErrNotImplemented
}
func (f fakeServerMode) DeleteSnapshot(context.Context, string, string) error {
	return ErrNotImplemented
}

func TestRegistryRoundTrip(t *testing.T) {
	// Use a mode value that real implementations won't register, to avoid
	// cross-test contamination via the package-level registry.
	const testMode model.Mode = "test-fake"
	RegisterClient(fakeClientMode{m: testMode})
	RegisterServer(fakeServerMode{m: testMode})

	cm, ok := Client(testMode)
	if !ok {
		t.Fatal("Client(test-fake) not found after registration")
	}
	res, err := cm.Backup(context.Background(), BackupOpts{})
	if err != nil || res.SnapshotID != "snap-1" {
		t.Fatalf("fake backup = %+v, %v", res, err)
	}

	sm, ok := Server(testMode)
	if !ok {
		t.Fatal("Server(test-fake) not found after registration")
	}
	if _, err := sm.List(context.Background(), "/dir"); err != ErrNotImplemented {
		t.Fatalf("List err = %v, want ErrNotImplemented", err)
	}
}

func TestRegistryUnknownMode(t *testing.T) {
	if _, ok := Client("does-not-exist"); ok {
		t.Fatal("Client(unknown) returned ok=true")
	}
	if _, ok := Server("does-not-exist"); ok {
		t.Fatal("Server(unknown) returned ok=true")
	}
}
