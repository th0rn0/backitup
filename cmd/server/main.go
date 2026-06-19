// Command server is the backitup control plane: HTTP API + webgui + lifecycle
// timer in one process (design doc D1). Testable logic lives in internal/*;
// this is wiring: open the store, bootstrap the admin, serve (TLS if configured).
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/lifecycle"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/server"
	"github.com/th0rn0/backitup/internal/store"

	// Register the server-side mode behaviours used by the lifecycle worker.
	_ "github.com/th0rn0/backitup/internal/mode/rsync"
	_ "github.com/th0rn0/backitup/internal/mode/targz"
)

func main() {
	dbPath := getenv("BACKITUP_DB", "/data/backitup.db")
	addr := getenv("BACKITUP_ADDR", ":8080")
	tlsCert := os.Getenv("BACKITUP_TLS_CERT")
	tlsKey := os.Getenv("BACKITUP_TLS_KEY")
	secure := tlsCert != "" && tlsKey != ""

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	log.Printf("backitup server: store ready at %s", dbPath)

	if err := bootstrapAdmin(st); err != nil {
		log.Fatalf("bootstrap admin: %v", err)
	}

	srv := server.New(st, secure)
	backupDir := getenv("BACKITUP_BACKUP_DIR", "/srv/backups")
	srv.ConfigureIngest(
		os.Getenv("BACKITUP_AUTHKEYS"),
		backupDir,
		os.Getenv("BACKITUP_PUBLIC_HOST"),
		os.Getenv("BACKITUP_CLIENT_IMAGE"),
	)

	// Lifecycle worker: offsite tiering + retention + integrity, on the server's
	// own schedule (D1/D8/D9). Per-client OffsiteRemote gates whether a client
	// is tiered; rclone is only invoked for clients that opt in.
	stopLifecycle := lifecycle.StartWorker(context.Background(), lifecycle.Deps{
		Store:         st,
		Offsite:       lifecycle.NewRclone(os.Getenv("BACKITUP_RCLONE_CONFIG")),
		BackupBaseDir: backupDir,
	}, parseInterval(getenv("BACKITUP_LIFECYCLE_INTERVAL", "1h")))
	defer stopLifecycle()
	if secure {
		log.Printf("backitup server: listening on %s (TLS)", addr)
		err = http.ListenAndServeTLS(addr, tlsCert, tlsKey, srv.Handler())
	} else {
		log.Printf("backitup server: listening on %s (PLAINTEXT — set BACKITUP_TLS_CERT/KEY for production)", addr)
		err = http.ListenAndServe(addr, srv.Handler())
	}
	if err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// bootstrapAdmin upserts the admin account from env if BACKITUP_ADMIN_USER and
// BACKITUP_ADMIN_PASSWORD are set. If neither is set and no admin exists yet, it
// warns (the webgui is unusable until an admin is created).
func bootstrapAdmin(st *store.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	user := os.Getenv("BACKITUP_ADMIN_USER")
	pass := os.Getenv("BACKITUP_ADMIN_PASSWORD")
	if user == "" || pass == "" {
		if admin, _ := st.GetAdmin(ctx); admin == nil {
			log.Printf("backitup server: no admin set — set BACKITUP_ADMIN_USER and BACKITUP_ADMIN_PASSWORD to enable login")
		}
		return nil
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		return err
	}
	if err := st.SetAdmin(ctx, model.Admin{Username: user, PasswordHash: hash}); err != nil {
		return err
	}
	log.Printf("backitup server: admin %q configured", user)
	return nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseInterval(s string) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		log.Printf("backitup server: bad lifecycle interval %q, using 1h", s)
		return time.Hour
	}
	return d
}
