// Package server builds backitup's HTTP control plane (design doc D1, D3, Lane B):
// admin login (argon2id + session cookie), the fleet dashboard, and the
// client-facing API (bearer-token config pull + status report).
package server

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/*.css assets/*.js assets/*.svg
var assetFS embed.FS

const sessionCookie = "backitup_session"

// Server holds the control plane's dependencies.
type Server struct {
	st       *store.Store
	sessions *auth.SessionStore
	tmpl     *template.Template
	limiter  *loginLimiter
	secure   bool // set cookies Secure (true when served over TLS)

	// Ingest config (Lane A): where to write authorized_keys, the per-client
	// backup base dir, and what to show in the generated docker run command.
	authKeysPath   string
	backupBaseDir  string
	publicHost     string // sshd host:port shown as BACKITUP_SERVER in docker run
	publicAPI      string // full control-channel base URL shown as BACKITUP_API (e.g. http://host:8080)
	clientImage    string
	sshHostKeyPath string // path to sshd host public key for known_hosts generation
}

// New returns a Server backed by the given store. secure marks session cookies
// Secure (use true behind TLS).
func New(st *store.Store, secure bool) *Server {
	funcs := template.FuncMap{
		"humanDuration": func(from, to time.Time) string {
			d := to.Sub(from).Round(time.Second)
			if d < 0 {
				return "—"
			}
			if d < time.Minute {
				return fmt.Sprintf("%ds", int(d.Seconds()))
			}
			m := int(d.Minutes())
			s := int(d.Seconds()) % 60
			if s == 0 {
				return fmt.Sprintf("%dm", m)
			}
			return fmt.Sprintf("%dm %ds", m, s)
		},
		"humanBytes": func(n int64) string {
			const unit = 1024
			if n < unit {
				return fmt.Sprintf("%d B", n)
			}
			div, exp := int64(unit), 0
			for v := n / unit; v >= unit; v /= unit {
				div *= unit
				exp++
			}
			return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
		},
	}
	return &Server{
		st:            st,
		sessions:      auth.NewSessionStore(12 * time.Hour),
		tmpl:          template.Must(template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")),
		limiter:       newLoginLimiter(),
		secure:        secure,
		authKeysPath:  "/srv/authkeys/authorized_keys",
		backupBaseDir: "/srv/backups",
		publicHost:    "your-server:2222",
		clientImage:   "th0rn0/backitup-client:latest",
	}
}

// ConfigureIngest sets the Lane A ingest parameters (called from cmd/server with
// env values). Empty arguments leave the existing default in place.
func (s *Server) ConfigureIngest(authKeysPath, backupBaseDir, publicHost, publicAPI, clientImage, sshHostKeyPath string) {
	if authKeysPath != "" {
		s.authKeysPath = authKeysPath
	}
	if backupBaseDir != "" {
		s.backupBaseDir = backupBaseDir
	}
	if publicHost != "" {
		s.publicHost = publicHost
	}
	if publicAPI != "" {
		s.publicAPI = publicAPI
	}
	if clientImage != "" {
		s.clientImage = clientImage
	}
	if sshHostKeyPath != "" {
		s.sshHostKeyPath = sshHostKeyPath
	}
}

// Handler builds the HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(mustSub(assetFS))))

	mux.HandleFunc("GET /login", s.getLogin)
	mux.HandleFunc("POST /login", s.postLogin)
	mux.HandleFunc("POST /logout", s.postLogout)
	mux.HandleFunc("GET /{$}", s.requireAdmin(s.dashboard))
	mux.HandleFunc("GET /clients/new", s.requireAdmin(s.getNewClient))
	mux.HandleFunc("POST /clients", s.requireAdmin(s.postClients))
	mux.HandleFunc("GET /clients/{name}", s.requireAdmin(s.getClient))
	mux.HandleFunc("GET /clients/{name}/runs/{runID}", s.requireAdmin(s.getRunLog))
	mux.HandleFunc("POST /clients/{name}/rotate", s.requireAdmin(s.postRotateClient))
	mux.HandleFunc("POST /clients/{name}/delete", s.requireAdmin(s.postDeleteClient))

	mux.HandleFunc("GET /users", s.requireAdmin(s.getUsers))
	mux.HandleFunc("POST /users", s.requireAdmin(s.postCreateUser))
	mux.HandleFunc("POST /users/{id}/delete", s.requireAdmin(s.postDeleteUser))

	// Fleet status API (session-authed); polled by the dashboard for live updates.
	mux.HandleFunc("GET /api/v1/fleet", s.requireAdmin(s.getFleetStatus))

	// Client API (bearer token), needed by Lane C. Control channel is HTTPS in
	// production (cmd/server serves TLS when configured).
	mux.HandleFunc("GET /api/v1/config", s.requireClient(s.getConfig))
	mux.HandleFunc("POST /api/v1/status", s.requireClient(s.postStatus))

	return s.securityHeaders(mux)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := s.st.ListClients(ctx); err != nil {
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// securityHeaders applies hardened defaults to every response.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self'; object-src 'none'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.secure {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
