package authkeys

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/keys"
	"github.com/th0rn0/backitup/internal/model"
)

func freshKey(t *testing.T) string {
	t.Helper()
	_, pub, err := keys.GenerateKeypair("backitup:test")
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	return pub
}

func TestRenderPerModeCommands(t *testing.T) {
	rsyncKey := freshKey(t)
	targzKey := freshKey(t)
	clients := []model.Client{
		{ID: 1, Mode: model.ModeRsync, SSHPubKey: rsyncKey, Enabled: true},
		{ID: 2, Mode: model.ModeTarGz, SSHPubKey: targzKey, Enabled: true},
	}
	content, skipped := Render(clients, "/srv/backups")
	if len(skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", skipped)
	}
	if !strings.HasPrefix(content, Header) {
		t.Fatal("missing managed-file header")
	}
	if !strings.Contains(content, `restrict,command="rrsync /srv/backups/1" `+rsyncKey) {
		t.Errorf("rsync line wrong:\n%s", content)
	}
	if !strings.Contains(content, `restrict,command="backitup-recv /srv/backups/2" `+targzKey) {
		t.Errorf("targz line wrong:\n%s", content)
	}
}

func TestRenderSkipsDisabledAndEmpty(t *testing.T) {
	clients := []model.Client{
		{ID: 1, Mode: model.ModeRsync, SSHPubKey: freshKey(t), Enabled: false}, // disabled
		{ID: 2, Mode: model.ModeTarGz, SSHPubKey: "", Enabled: true},           // no key
		{ID: 3, Mode: model.ModeRsync, SSHPubKey: freshKey(t), Enabled: true},  // kept
	}
	content, _ := Render(clients, "/srv/backups")
	if strings.Contains(content, "/srv/backups/1") || strings.Contains(content, "/srv/backups/2") {
		t.Fatalf("disabled/keyless clients must be omitted:\n%s", content)
	}
	if !strings.Contains(content, "/srv/backups/3") {
		t.Fatalf("enabled client missing:\n%s", content)
	}
}

func TestRenderRejectsInjection(t *testing.T) {
	good := freshKey(t)
	cases := []struct {
		name string
		key  string
	}{
		{"newline", good + "\nrestrict,command=\"/bin/sh\" " + good},
		{"quote", strings.Replace(good, " ", `" `, 1)},
		{"carriage return", good + "\rfoo"},
		{"unparseable", "ssh-ed25519 not-base64 backitup"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clients := []model.Client{{ID: 1, Mode: model.ModeRsync, SSHPubKey: tc.key, Enabled: true}}
			content, skipped := Render(clients, "/srv/backups")
			if len(skipped) != 1 || skipped[0].ClientID != 1 {
				t.Fatalf("expected client 1 skipped, got %+v", skipped)
			}
			if strings.Contains(content, "/bin/sh") {
				t.Fatalf("injection leaked into output:\n%s", content)
			}
			// Only the header should remain — no key line emitted.
			if strings.TrimSpace(content) != strings.TrimSpace(Header) {
				t.Fatalf("malformed key should produce no key line:\n%s", content)
			}
		})
	}
}

func TestRenderResilientToOneBadRow(t *testing.T) {
	// A poisoned row must not break auth for the good clients (outside-voice).
	clients := []model.Client{
		{ID: 1, Mode: model.ModeRsync, SSHPubKey: "garbage", Enabled: true},
		{ID: 2, Mode: model.ModeTarGz, SSHPubKey: freshKey(t), Enabled: true},
	}
	content, skipped := Render(clients, "/srv/backups")
	if len(skipped) != 1 {
		t.Fatalf("expected 1 skip, got %d", len(skipped))
	}
	if !strings.Contains(content, "/srv/backups/2") {
		t.Fatal("good client lost its access because of a bad row")
	}
}

func TestRenderSkipsUnknownMode(t *testing.T) {
	clients := []model.Client{{ID: 1, Mode: "weird", SSHPubKey: freshKey(t), Enabled: true}}
	content, skipped := Render(clients, "/srv/backups")
	if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "unknown mode") {
		t.Fatalf("expected unknown-mode skip, got %+v", skipped)
	}
	if strings.Contains(content, "/srv/backups/1") {
		t.Fatalf("unknown-mode client should not be emitted:\n%s", content)
	}
}

func TestWriteAtomicBadDir(t *testing.T) {
	// Parent directory does not exist -> CreateTemp fails -> error (not a panic).
	if err := WriteAtomic(filepath.Join(t.TempDir(), "no-such-dir", "authorized_keys"), "x"); err == nil {
		t.Fatal("expected error writing into a nonexistent directory")
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "authorized_keys")

	if err := WriteAtomic(path, "first\n"); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	if err := WriteAtomic(path, "second\n"); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "second\n" {
		t.Fatalf("content = %q, want overwrite", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("perms = %o, want 600", info.Mode().Perm())
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected only the authorized_keys file, got %d entries", len(entries))
	}
}
