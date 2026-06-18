package client

import (
	"testing"

	"github.com/th0rn0/backitup/internal/model"
)

func validConfig() Config {
	return Config{Server: "host:22", Token: "secret", Source: "/source", Mode: model.ModeTarGz}
}

func TestConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid targz", func(c *Config) {}, false},
		{"valid rsync", func(c *Config) { c.Mode = model.ModeRsync }, false},
		{"bad mode", func(c *Config) { c.Mode = "zip" }, true},
		{"empty mode", func(c *Config) { c.Mode = "" }, true},
		{"missing server", func(c *Config) { c.Server = "" }, true},
		{"missing token", func(c *Config) { c.Token = "" }, true},
		{"missing source", func(c *Config) { c.Source = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestEnvFallback(t *testing.T) {
	if got := Env("BACKITUP_TEST_UNSET_VAR", "default"); got != "default" {
		t.Fatalf("unset var = %q, want default", got)
	}
	t.Setenv("BACKITUP_TEST_SET_VAR", "value")
	if got := Env("BACKITUP_TEST_SET_VAR", "default"); got != "value" {
		t.Fatalf("set var = %q, want value", got)
	}
	t.Setenv("BACKITUP_TEST_EMPTY_VAR", "")
	if got := Env("BACKITUP_TEST_EMPTY_VAR", "default"); got != "default" {
		t.Fatalf("empty var = %q, want default (empty treated as unset)", got)
	}
}
