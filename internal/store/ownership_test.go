package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

// record_ownership is stored per client and defaults to off. The default is the
// whole compatibility story: a snapshot tree that suddenly carried
// pseudo-ownership would confuse every operator and every restore script that
// predates the setting, so an upgraded row must keep the old shape.
func TestClientRecordOwnershipRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	if _, err := st.CreateClient(ctx, model.Client{
		Name: "default-off", Mode: model.ModeRsync, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA a", TokenHash: "h", TokenPrefix: "aaaa",
	}); err != nil {
		t.Fatalf("create client: %v", err)
	}
	c, err := st.GetClientBySlug(ctx, "default-off")
	if err != nil || c == nil {
		t.Fatalf("load client: %v (client=%v)", err, c)
	}
	if c.RecordOwnership {
		t.Fatalf("a client created without the setting has it on: %+v", c)
	}

	if err := st.UpdateClientOwnership(ctx, c.ID, true); err != nil {
		t.Fatalf("turn it on: %v", err)
	}
	// Every read path has its own query; the column list is the trap here.
	for name, load := range map[string]func() (*model.Client, error){
		"GetClientBySlug": func() (*model.Client, error) { return st.GetClientBySlug(ctx, "default-off") },
		"GetClient":       func() (*model.Client, error) { return st.GetClient(ctx, c.ID) },
	} {
		got, err := load()
		if err != nil || got == nil {
			t.Fatalf("%s: %v (client=%v)", name, err, got)
		}
		if !got.RecordOwnership {
			t.Fatalf("%s: the setting did not survive the round trip", name)
		}
	}

	if err := st.UpdateClientOwnership(ctx, c.ID, false); err != nil {
		t.Fatalf("turn it off: %v", err)
	}
	got, err := st.GetClientBySlug(ctx, "default-off")
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if got.RecordOwnership {
		t.Fatal("turning the setting off did not stick")
	}

	// A create that ASKS for it must store it: the create path has its own
	// INSERT with its own placeholder count.
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "asked-for", Mode: model.ModeRsync, Enabled: true, RecordOwnership: true,
		SSHPubKey: "ssh-ed25519 AAAA b", TokenHash: "h", TokenPrefix: "bbbb",
	}); err != nil {
		t.Fatalf("create client that asks for it: %v", err)
	}
	asked, err := st.GetClientBySlug(ctx, "asked-for")
	if err != nil || asked == nil {
		t.Fatalf("load: %v", err)
	}
	if !asked.RecordOwnership {
		t.Fatal("CreateClient dropped record_ownership")
	}
}

// Updating a row that is not there is an error, not a silent success: the
// handler turns it into a flash, and "saved" on a client that no longer exists
// would be a lie.
func TestUpdateClientOwnershipUnknownClient(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.UpdateClientOwnership(context.Background(), 4242, true); err == nil {
		t.Fatal("updating a missing client returned no error")
	}
}

// RecordOwnership and GroupReadable are independent settings that live in the
// same row: writing one must not clear the other (they have separate UPDATEs
// precisely so a page that only submits one form cannot forget the other).
func TestOwnershipAndGroupReadableAreIndependent(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.CreateClient(ctx, model.Client{
		Name: "both", Mode: model.ModeRsync, Enabled: true,
		SSHPubKey: "ssh-ed25519 AAAA c", TokenHash: "h", TokenPrefix: "cccc",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c, err := st.GetClientBySlug(ctx, "both")
	if err != nil || c == nil {
		t.Fatalf("load: %v", err)
	}
	if err := st.UpdateClientOwnership(ctx, c.ID, true); err != nil {
		t.Fatalf("ownership on: %v", err)
	}
	if err := st.UpdateClientGroupReadable(ctx, c.ID, true); err != nil {
		t.Fatalf("group-readable on: %v", err)
	}
	got, err := st.GetClientBySlug(ctx, "both")
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if !got.RecordOwnership || !got.GroupReadable {
		t.Fatalf("one setting cleared the other: ownership=%v group_readable=%v",
			got.RecordOwnership, got.GroupReadable)
	}
}
