package sshutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/th0rn0/backitup/internal/keys"
)

func TestLoadSigner(t *testing.T) {
	priv, _, err := keys.GenerateKeypair("backitup:test")
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id")
	if err := os.WriteFile(path, []byte(priv), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigner(path); err != nil {
		t.Fatalf("LoadSigner: %v", err)
	}
}

func TestLoadSignerErrors(t *testing.T) {
	if _, err := LoadSigner("/no/such/key"); err == nil {
		t.Error("expected error for missing key file")
	}
	bad := filepath.Join(t.TempDir(), "bad")
	_ = os.WriteFile(bad, []byte("not a key"), 0o600)
	if _, err := LoadSigner(bad); err == nil {
		t.Error("expected parse error for garbage key")
	}
}

func TestHostKeyCallback(t *testing.T) {
	// insecure -> non-nil callback, no error
	cb, err := HostKeyCallback("", true)
	if err != nil || cb == nil {
		t.Fatalf("insecure callback: cb=%v err=%v", cb, err)
	}
	// secure but no known_hosts -> error (fail closed)
	if _, err := HostKeyCallback("", false); err == nil {
		t.Fatal("expected error: secure mode needs known_hosts")
	}
	// secure with a missing known_hosts file -> error
	if _, err := HostKeyCallback("/no/such/known_hosts", false); err == nil {
		t.Fatal("expected error loading missing known_hosts")
	}
}
