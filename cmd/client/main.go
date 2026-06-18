// Command client is backitup's dumb, stateless uploader (design doc Approach A).
// It is triggered by HOST cron, runs once, and exits. It NEVER writes to the
// source directory (read-only bind mount + guards). Testable logic lives in
// internal/client.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/th0rn0/backitup/internal/client"
	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

func main() {
	var (
		server   = flag.String("server", client.Env("BACKITUP_SERVER", ""), "server host:port")
		token    = flag.String("token", client.Env("BACKITUP_TOKEN", ""), "bearer token (control channel)")
		source   = flag.String("source", client.Env("BACKITUP_SOURCE", "/source"), "read-only source mount")
		modeFlag = flag.String("mode", client.Env("BACKITUP_MODE", "targz"), "targz|rsync (usually fetched from server)")
	)
	flag.Parse()

	cfg := client.Config{
		Server: *server,
		Token:  *token,
		Source: *source,
		Mode:   model.Mode(*modeFlag),
	}
	if err := cfg.Validate(); err != nil {
		log.Fatalf("config: %v", err)
	}

	// Run flow (design doc client section), filled in by Lane C:
	//   1. acquire lockfile (report "overlap" + exit 0 if held)
	//   2. fetch config over HTTPS (verify cert; refuse on failure, never leak token)
	//   3. run the mode's Backup() — read-only on source
	//   4. on rsync success, flip `latest`
	//   5. POST status; release lock
	cm, ok := mode.Client(cfg.Mode)
	if !ok {
		log.Fatalf("mode %q not yet implemented (Lane C)", cfg.Mode)
	}
	if _, err := cm.Backup(context.Background(), mode.BackupOpts{
		SourceDir: cfg.Source,
		Server:    cfg.Server,
	}); err != nil {
		log.Fatalf("backup: %v", err)
	}
}
