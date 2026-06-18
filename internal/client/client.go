// Package client holds the testable logic for backitup's dumb uploader
// (design doc Approach A). cmd/client is a thin wrapper around this.
package client

import (
	"errors"
	"fmt"
	"os"

	"github.com/th0rn0/backitup/internal/model"
)

// Config is the resolved client configuration for one run.
type Config struct {
	Server string     // sshd ingest host:port
	Token  string     // bearer token (control channel)
	Source string     // read-only source mount
	Mode   model.Mode // usually fetched from the server; flag is a fallback
}

// Validate checks a Config before a run. It does not touch the network.
func (c Config) Validate() error {
	if !c.Mode.Valid() {
		return fmt.Errorf("invalid mode %q (want targz or rsync)", c.Mode)
	}
	if c.Server == "" {
		return errors.New("server is required")
	}
	if c.Token == "" {
		return errors.New("token is required")
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
