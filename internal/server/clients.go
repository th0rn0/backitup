package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/authkeys"
	"github.com/th0rn0/backitup/internal/keys"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// getNewClient renders the add-client form.
func (s *Server) getNewClient(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "clients_new.html", nil)
}

// postClients creates a client: generate an SSH keypair + bearer token, store
// the client (pubkey + token HASH only), regenerate authorized_keys, and show
// the private key + token + cron line ONCE (D4, DD5).
func (s *Server) postClients(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.PostFormValue("name")
	mode := model.Mode(r.PostFormValue("mode"))
	if name == "" || !mode.Valid() {
		s.renderNewClientError(w, "Name is required and mode must be targz or rsync.")
		return
	}

	privPEM, pubLine, token, tokenHash, ok := generateClientCreds(w, name)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, err := s.st.CreateClient(ctx, model.Client{
		Name:                 name,
		Mode:                 mode,
		SourceLabel:          r.PostFormValue("source_label"),
		RetentionDays:        atoiDefault(r.PostFormValue("retention_days"), 14),
		OffsiteRetentionDays: atoiDefault(r.PostFormValue("offsite_retention_days"), 90),
		ExpectedIntervalSecs: atoiDefault(r.PostFormValue("expected_interval_secs"), 0),
		OffsiteRemote:        r.PostFormValue("offsite_remote"),
		SkipSymlinks:         r.PostFormValue("skip_symlinks") == "1",
		SSHPubKey:            pubLine,
		TokenHash:            tokenHash,
		Enabled:              true,
	})
	if err != nil {
		// Most likely a duplicate name (UNIQUE) — report it as a form error.
		s.renderNewClientError(w, "Could not create client (is the name already taken?).")
		return
	}

	if err := s.regenAuthorizedKeys(ctx); err != nil {
		log.Printf("authkeys regenerate failed: %v", err)
		// The client exists; surface the issue but still show the secrets.
	}

	apiBase := s.apiBase(r.PostFormValue("api_scheme"))
	dockerKnown, dockerInsecure := s.dockerCmds(token, apiBase)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "client_created.html", map[string]any{
		"Name":              name,
		"Mode":              string(mode),
		"PrivateKey":        privPEM,
		"Token":             token,
		"Server":            s.publicHost,
		"APIBase":           apiBase,
		"KnownHostsLine":    knownHostsLine(s.publicHost, s.sshHostKeyPath),
		"DockerRunKnown":    dockerKnown,
		"DockerRunInsecure": dockerInsecure,
	})
}

// getClient renders the client detail page with run history.
func (s *Server) getClient(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	runs, _ := s.st.ListRuns(ctx, c.ID, 20)
	var latest *model.Run
	if len(runs) > 0 {
		latest = &runs[0]
	}
	h := model.DeriveHealth(latest, time.Duration(c.ExpectedIntervalSecs)*time.Second, time.Now())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "client_detail.html", map[string]any{
		"Username":    usernameFromContext(r.Context()),
		"Client":      c,
		"Health":      string(h),
		"HealthLabel": healthLabel(h),
		"Icon":        healthIcon(h),
		"Runs":        runs,
	})
}

// getRunLog renders the log output for a single run.
func (s *Server) getRunLog(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.PathValue("runID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil || c == nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		http.Error(w, "failed to load run", http.StatusInternalServerError)
		return
	}
	if run == nil || run.ClientID != c.ID {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "run_log.html", map[string]any{
		"Username": usernameFromContext(r.Context()),
		"Client":   c,
		"Run":      run,
	})
}

// postRotateClient reissues the SSH key + bearer token for an existing client.
// Run history and all other settings are preserved. The old credentials are
// invalidated atomically; the operator must redeploy the new cron line.
func (s *Server) postRotateClient(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		http.Error(w, "confirmation required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}

	privPEM, pubLine, token, tokenHash, ok := generateClientCreds(w, c.Name)
	if !ok {
		return
	}

	if err := s.st.RotateClientCreds(ctx, c.ID, pubLine, tokenHash, c.Version); err != nil {
		if errors.Is(err, store.ErrConflict) {
			http.Error(w, "concurrent rotation detected — reload the page and try again", http.StatusConflict)
			return
		}
		http.Error(w, "rotate failed", http.StatusInternalServerError)
		return
	}

	// Use a fresh context for the authkeys write: it is a local filesystem
	// operation that must not be bounded by the already-partially-spent HTTP
	// request context (the DB write above may have consumed most of the 5s).
	akCtx, akCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer akCancel()
	authKeysFailed := false
	if err := s.regenAuthorizedKeys(akCtx); err != nil {
		log.Printf("authkeys regenerate failed after rotate client %d: %v", c.ID, err)
		authKeysFailed = true
	}

	apiBase := s.apiBase(r.PostFormValue("api_scheme"))
	dockerKnown, dockerInsecure := s.dockerCmds(token, apiBase)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "client_created.html", map[string]any{
		"Name":              c.Name,
		"Mode":              string(c.Mode),
		"PrivateKey":        privPEM,
		"Token":             token,
		"Server":            s.publicHost,
		"APIBase":           apiBase,
		"KnownHostsLine":    knownHostsLine(s.publicHost, s.sshHostKeyPath),
		"DockerRunKnown":    dockerKnown,
		"DockerRunInsecure": dockerInsecure,
		"Rotated":           true,
		"AuthKeysFailed":    authKeysFailed,
	})
}

// postDeleteClient removes a client and all its run history, regenerates
// authorized_keys, then redirects to the dashboard.
func (s *Server) postDeleteClient(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		http.Error(w, "confirmation required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}

	if err := s.st.DeleteClient(ctx, c.ID); err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	akCtx, akCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer akCancel()
	if err := s.regenAuthorizedKeys(akCtx); err != nil {
		log.Printf("authkeys regenerate failed after delete client %d: %v", c.ID, err)
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// generateClientCreds generates a new SSH keypair and bearer token for the
// named client, writing errors directly to w. Returns ok=false on any error.
func generateClientCreds(w http.ResponseWriter, name string) (privPEM, pubLine, token, tokenHash string, ok bool) {
	var err error
	privPEM, pubLine, err = keys.GenerateKeypair("backitup:" + name)
	if err != nil {
		http.Error(w, "keygen failed", http.StatusInternalServerError)
		return
	}
	token, err = keys.GenerateToken()
	if err != nil {
		http.Error(w, "token gen failed", http.StatusInternalServerError)
		return
	}
	tokenHash, err = auth.HashPassword(token)
	if err != nil {
		http.Error(w, "token hash failed", http.StatusInternalServerError)
		return
	}
	ok = true
	return
}

func (s *Server) renderNewClientError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = s.tmpl.ExecuteTemplate(w, "clients_new.html", map[string]any{"Error": msg})
}

// regenAuthorizedKeys rewrites the sshd authorized_keys file from the current
// client list, atomically (D4). A no-op if no path is configured.
func (s *Server) regenAuthorizedKeys(ctx context.Context) error {
	if s.authKeysPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.authKeysPath), 0o700); err != nil {
		return err
	}
	clients, err := s.st.ListClients(ctx)
	if err != nil {
		return err
	}
	// Each enabled client needs its backup directory to exist before its first
	// run: rrsync (rsync mode) locks its jail root on connect, and rsync won't
	// create the missing snapshots/ parent. tar.gz's receiver mkdir's as needed,
	// but pre-creating is harmless and keeps the layout predictable.
	for _, c := range clients {
		if !c.Enabled {
			continue
		}
		dir := filepath.Join(s.backupBaseDir, model.Slug(c.Name))
		if c.Mode == model.ModeRsync {
			dir = filepath.Join(dir, "snapshots")
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create client dir %s: %w", dir, err)
		}
	}
	content, skipped := authkeys.Render(clients, s.backupBaseDir)
	for _, sk := range skipped {
		log.Printf("authkeys: skipped client %d: %s", sk.ClientID, sk.Reason)
	}
	return authkeys.WriteAtomic(s.authKeysPath, content)
}

// apiBase returns the control-channel base URL for use in generated docker
// commands. If BACKITUP_PUBLIC_API is set it is used as-is; otherwise the
// scheme is derived from apiScheme ("http"/"https") and a placeholder host
// reminds the admin to configure BACKITUP_PUBLIC_API.
func (s *Server) apiBase(apiScheme string) string {
	if s.publicAPI != "" {
		return s.publicAPI
	}
	if apiScheme != "https" {
		apiScheme = "http"
	}
	return apiScheme + "://YOUR-SERVER:8080  # set BACKITUP_PUBLIC_API on the server"
}

// dockerCmds returns two ready-to-run docker commands for a client:
//   - known: mounts /secrets with BACKITUP_KNOWN_HOSTS (recommended)
//   - insecure: adds BACKITUP_INSECURE=1, no host-key verification
func (s *Server) dockerCmds(token, apiBase string) (known, insecure string) {
	base := []string{
		"docker run --rm \\",
		"  --user $(id -u):$(id -g) \\",
		"  --mount type=bind,src=/PATH/TO/BACKUP,dst=/source,readonly \\",
		"  -v /PATH/TO/SECRETS:/secrets:ro \\",
		fmt.Sprintf("  -e BACKITUP_API=%s \\", apiBase),
		fmt.Sprintf("  -e BACKITUP_SERVER=%s \\", s.publicHost),
		fmt.Sprintf("  -e BACKITUP_TOKEN=%s \\", token),
		"  -e BACKITUP_SSH_KEY=/secrets/id \\",
	}
	join := func(extra ...string) string {
		parts := append(append([]string{}, base...), extra...)
		parts = append(parts, "  "+s.clientImage)
		return strings.Join(parts, "\n")
	}
	known = join("  -e BACKITUP_KNOWN_HOSTS=/secrets/known_hosts \\")
	insecure = join("  -e BACKITUP_INSECURE=1 \\")
	return
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// knownHostsLine reads the sshd host public key from keyPath and formats it
// as a known_hosts entry for publicHost (host:port). Returns "" if the file
// is missing or unparseable — callers treat "" as "host key not yet available".
func knownHostsLine(publicHost, keyPath string) string {
	if keyPath == "" {
		return ""
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return ""
	}
	// File format: "ssh-ed25519 AAAA... comment\n"
	// known_hosts format: "[host]:port keytype base64key"
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 2 {
		return ""
	}
	host, port, err := net.SplitHostPort(publicHost)
	if err != nil {
		host = publicHost
		port = "22"
	}
	var addr string
	if port == "22" {
		addr = host
	} else {
		addr = fmt.Sprintf("[%s]:%s", host, port)
	}
	return fmt.Sprintf("%s %s %s", addr, fields[0], fields[1])
}
