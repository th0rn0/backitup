package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

func TestOffsiteLabel(t *testing.T) {
	// not configured
	if got := offsiteLabel(model.Client{OffsiteRemote: ""}, nil); got != "—" {
		t.Errorf("no remote = %q, want —", got)
	}
	// configured, never tiered
	if got := offsiteLabel(model.Client{OffsiteRemote: "s3"}, nil); !strings.Contains(got, "pending") {
		t.Errorf("never-tiered = %q, want pending", got)
	}
	// configured and tiered
	ago := time.Now().Add(-2 * time.Hour)
	got := offsiteLabel(model.Client{OffsiteRemote: "gdrive"}, &ago)
	if !strings.HasPrefix(got, "✓ gdrive") {
		t.Errorf("tiered = %q, want ✓ gdrive ...", got)
	}
}

func TestConfigureIngest(t *testing.T) {
	s := New(nil, false)
	defAuth := s.authKeysPath

	// Empty args preserve defaults.
	s.ConfigureIngest("", "", "", "", "")
	if s.authKeysPath != defAuth {
		t.Fatalf("empty arg overwrote default: %q", s.authKeysPath)
	}

	// Non-empty args override.
	s.ConfigureIngest("/ak", "/b", "host:22", "img", "/hostkey.pub")
	if s.authKeysPath != "/ak" || s.backupBaseDir != "/b" || s.publicHost != "host:22" || s.clientImage != "img" || s.sshHostKeyPath != "/hostkey.pub" {
		t.Fatalf("ConfigureIngest did not apply: %+v", *s)
	}
}

func TestLoginLimiter(t *testing.T) {
	l := newLoginLimiter()
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "192.0.2.1:12345"

	for i := 0; i < loginMaxFails; i++ {
		if !l.allow(req) {
			t.Fatalf("allow returned false before limit at attempt %d", i+1)
		}
		l.record(req)
	}
	if l.allow(req) {
		t.Fatalf("allow returned true after %d failed attempts; expected rate-limited", loginMaxFails)
	}
}

func TestKnownHostsLine(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "ssh_host_ed25519_key.pub")

	// Write a plausible pubkey file (type + base64, optional comment).
	_ = os.WriteFile(keyPath, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA backitup-host\n"), 0o600)

	got := knownHostsLine("myserver:2222", keyPath)
	if !strings.HasPrefix(got, "[myserver]:2222 ssh-ed25519 ") {
		t.Errorf("unexpected line: %q", got)
	}

	// Port 22: no brackets.
	got22 := knownHostsLine("myserver:22", keyPath)
	if !strings.HasPrefix(got22, "myserver ssh-ed25519 ") {
		t.Errorf("port 22 line: %q", got22)
	}

	// Missing file: silent empty string.
	if got := knownHostsLine("myserver:2222", "/nonexistent/path.pub"); got != "" {
		t.Errorf("missing file returned %q, want empty", got)
	}
}

func TestAtoiDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"", 14, 14},
		{"30", 14, 30},
		{"-5", 14, 14}, // negative -> default
		{"abc", 7, 7},  // non-numeric -> default
		{"0", 90, 0},   // zero is allowed
	}
	for _, c := range cases {
		if got := atoiDefault(c.in, c.def); got != c.want {
			t.Errorf("atoiDefault(%q, %d) = %d, want %d", c.in, c.def, got, c.want)
		}
	}
}
