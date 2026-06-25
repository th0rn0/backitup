// Command server is the backitup control plane: HTTP API + webgui + lifecycle
// timer in one process (design doc D1). Testable logic lives in internal/*;
// this is wiring: open the store, bootstrap the admin, serve (TLS if configured).
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/lifecycle"
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
	defer func() { _ = st.Close() }()
	log.Printf("backitup server: store ready at %s", dbPath)

	if err := bootstrapAdmin(st); err != nil {
		log.Fatalf("bootstrap admin: %v", err)
	}
	if err := st.MarkStaleOffsiteRunsFailed(context.Background()); err != nil {
		log.Printf("warn: cleanup stale offsite runs: %v", err)
	}

	srv := server.New(st, secure)
	backupDir := getenv("BACKITUP_BACKUP_DIR", "/srv/backups")
	srv.ConfigureIngest(
		os.Getenv("BACKITUP_AUTHKEYS"),
		backupDir,
		os.Getenv("BACKITUP_PUBLIC_HOST"),
		os.Getenv("BACKITUP_PUBLIC_API"),
		os.Getenv("BACKITUP_CLIENT_IMAGE"),
		getenv("BACKITUP_SSH_HOST_KEY", "/srv/hostkeys/ssh_host_ed25519_key.pub"),
	)
	srv.ConfigureRclone(getenv("BACKITUP_RCLONE_CONFIG", "/data/rclone.conf"))
	if err := srv.RegenerateRcloneConfig(context.Background()); err != nil {
		log.Printf("warn: regenerate rclone config: %v", err)
	}
	srv.ConfigureDiscord(os.Getenv("BACKITUP_DISCORD_WEBHOOK"))
	verbose := os.Getenv("BACKITUP_VERBOSE") == "true" || os.Getenv("BACKITUP_VERBOSE") == "1"
	srv.ConfigureVerbose(verbose)

	// Sync authorized_keys once at startup so any stale entry (wrong forced-command
	// from a mode mismatch or a previously failed write) is corrected before sshd
	// accepts connections. Non-fatal: logs the error and continues.
	{
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		srv.SyncAuthorizedKeys(ctx)
		cancel()
	}

	// Lifecycle worker: offsite tiering + retention + integrity, on the server's
	// own schedule (D1/D8/D9). Per-client OffsiteRemote gates whether a client
	// is tiered; rclone is only invoked for clients that opt in.
	lcDeps := lifecycle.Deps{
		Store:            st,
		Offsite:          lifecycle.NewRclone(getenv("BACKITUP_RCLONE_CONFIG", "/data/rclone.conf")),
		BackupBaseDir:    backupDir,
		LogRetentionDays: atoiEnv("BACKITUP_LOG_RETENTION_DAYS", 0),
		DiscordWebhook:   os.Getenv("BACKITUP_DISCORD_WEBHOOK"),
		Verbose:          verbose,
	}
	stopLifecycle := lifecycle.StartWorker(context.Background(), lcDeps, parseInterval(getenv("BACKITUP_LIFECYCLE_INTERVAL", "5m")))
	defer stopLifecycle()
	// Offsite upload worker: independent of the maintenance lifecycle.
	// BACKITUP_OFFSITE_POLL_INTERVAL controls how often clients are checked;
	// each client's "Upload interval" setting controls whether an upload is due.
	stopOffsite := lifecycle.StartOffsiteWorker(context.Background(), lcDeps, parseInterval(getenv("BACKITUP_OFFSITE_POLL_INTERVAL", "5m")))
	defer stopOffsite()

	// Wire the "Backup now" button: resolves client by ID then runs an immediate
	// offsite pass using the same deps as the background worker.
	srv.ConfigureOffsiteTrigger(func(ctx context.Context, clientID int64) error {
		clients, err := st.ListClients(ctx)
		if err != nil {
			return err
		}
		for _, c := range clients {
			if c.ID == clientID {
				return lifecycle.OffsiteClient(ctx, lcDeps, c)
			}
		}
		return fmt.Errorf("client %d not found", clientID)
	})
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

// bootstrapAdmin seeds the users table from BACKITUP_ADMIN_USER / BACKITUP_ADMIN_PASSWORD
// if set. On every start it upserts so that a password change in the env takes effect
// without manual DB edits. Warns if no users exist and no env vars are configured.
func bootstrapAdmin(st *store.Store) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	username := os.Getenv("BACKITUP_ADMIN_USER")
	pass := os.Getenv("BACKITUP_ADMIN_PASSWORD")
	if username == "" || pass == "" {
		if n, _ := st.CountUsers(ctx); n == 0 {
			log.Printf("backitup server: no users exist — set BACKITUP_ADMIN_USER and BACKITUP_ADMIN_PASSWORD to create the first user")
		}
		return nil
	}
	hash, err := auth.HashPassword(pass)
	if err != nil {
		return err
	}
	if err := st.UpsertUser(ctx, username, hash); err != nil {
		return err
	}
	log.Printf("backitup server: user %q configured", username)
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

func atoiEnv(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}
