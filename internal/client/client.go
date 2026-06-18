// Package client holds the testable logic for backitup's dumb uploader
// (design doc Approach A): config, the control-channel API client, a per-run
// lock, and the run orchestration. cmd/client is a thin wrapper.
package client

import (
	"errors"
	"fmt"
	"os"

	"github.com/th0rn0/backitup/internal/model"
)

// Config is the resolved client configuration for one run.
type Config struct {
	APIBase    string     // control channel base URL, e.g. https://host:8080
	Token      string     // bearer token (control channel)
	SSHServer  string     // data channel host:port (sshd ingest)
	SSHUser    string     // ssh login user (the ingest user, "backitup")
	SSHKey     string     // path to the client's private key
	KnownHosts string     // known_hosts path for host-key verification
	CABundle   string     // optional CA cert file for the control channel (self-signed)
	Source     string     // read-only source mount
	Mode       model.Mode // fallback; the server's config is authoritative
	Insecure   bool       // skip host-key / TLS verification (dev/test only)
}

// Validate checks a Config before a run. It does not touch the network.
func (c Config) Validate() error {
	if !c.Mode.Valid() {
		return fmt.Errorf("invalid mode %q (want targz or rsync)", c.Mode)
	}
	if c.APIBase == "" {
		return errors.New("api base URL is required")
	}
	if c.Token == "" {
		return errors.New("token is required")
	}
	if c.SSHServer == "" {
		return errors.New("ssh server is required")
	}
	if c.SSHKey == "" {
		return errors.New("ssh key is required")
	}
	if c.Source == "" {
		return errors.New("source is required")
	}
	return nil
}

// Env returns the value of environment variable k, or def if unset/empty.
func Env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
