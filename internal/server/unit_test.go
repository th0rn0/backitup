package server

import (
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
	s.ConfigureIngest("", "", "", "")
	if s.authKeysPath != defAuth {
		t.Fatalf("empty arg overwrote default: %q", s.authKeysPath)
	}

	// Non-empty args override.
	s.ConfigureIngest("/ak", "/b", "host:22", "img")
	if s.authKeysPath != "/ak" || s.backupBaseDir != "/b" || s.publicHost != "host:22" || s.clientImage != "img" {
		t.Fatalf("ConfigureIngest did not apply: %+v", *s)
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
