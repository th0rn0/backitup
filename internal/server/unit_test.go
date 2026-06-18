package server

import "testing"

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
