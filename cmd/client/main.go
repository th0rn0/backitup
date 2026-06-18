// Command client is backitup's dumb, stateless uploader (design doc Approach A).
// It is triggered by HOST cron, runs once, and exits. It NEVER writes to the
// source directory (read-only bind mount + guards). Logic lives in
// internal/client; the modes register themselves via the blank imports below.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"path/filepath"

	"github.com/th0rn0/backitup/internal/client"
	"github.com/th0rn0/backitup/internal/model"

	// Register the backup modes.
	_ "github.com/th0rn0/backitup/internal/mode/rsync"
	_ "github.com/th0rn0/backitup/internal/mode/targz"
)

func main() {
	cfg := client.Config{
		APIBase:    client.Env("BACKITUP_API", ""),
		Token:      client.Env("BACKITUP_TOKEN", ""),
		SSHServer:  client.Env("BACKITUP_SERVER", ""),
		SSHUser:    client.Env("BACKITUP_SSH_USER", "backitup"),
		SSHKey:     client.Env("BACKITUP_SSH_KEY", "/secrets/id"),
		KnownHosts: client.Env("BACKITUP_KNOWN_HOSTS", ""),
		CABundle:   client.Env("BACKITUP_CA", ""),
		Source:     client.Env("BACKITUP_SOURCE", "/source"),
		Mode:       model.Mode(client.Env("BACKITUP_MODE", "targz")),
		Insecure:   client.Env("BACKITUP_INSECURE", "") == "1",
	}
	flag.StringVar(&cfg.APIBase, "api", cfg.APIBase, "control-channel base URL (https://host:8080)")
	flag.StringVar(&cfg.Token, "token", cfg.Token, "bearer token")
	flag.StringVar(&cfg.SSHServer, "server", cfg.SSHServer, "data-channel host:port")
	flag.StringVar(&cfg.SSHKey, "ssh-key", cfg.SSHKey, "path to client private key")
	flag.StringVar(&cfg.KnownHosts, "known-hosts", cfg.KnownHosts, "known_hosts file")
	flag.StringVar(&cfg.Source, "source", cfg.Source, "read-only source dir")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := client.Run(context.Background(), cfg, lockPath(cfg)); err != nil {
		log.Fatalf("backup: %v", err)
	}
}

// lockPath derives a stable per-client lock file from the source + server, so
// multiple clients on one host don't collide but a given client serialises.
func lockPath(cfg client.Config) string {
	sum := sha256.Sum256([]byte(cfg.Source + "|" + cfg.SSHServer + "|" + cfg.Token))
	return filepath.Join(os.TempDir(), "backitup-"+hex.EncodeToString(sum[:8])+".lock")
}
