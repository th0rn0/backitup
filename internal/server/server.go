// Package server builds backitup's HTTP control plane (design doc D1, D3, Lane B):
// admin login (argon2id + session cookie), the fleet dashboard, and the
// client-facing API (bearer-token config pull + status report).
package server

import (
	"context"
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets/*.css
var assetFS embed.FS

const sessionCookie = "backitup_session"

// Server holds the control plane's dependencies.
type Server struct {
	st       *store.Store
	sessions *auth.SessionStore
	tmpl     *template.Template
	secure   bool // set cookies Secure (true when served over TLS)
}

// New returns a Server backed by the given store. secure marks session cookies
// Secure (use true behind TLS).
func New(st *store.Store, secure bool) *Server {
	return &Server{
		st:       st,
		sessions: auth.NewSessionStore(12 * time.Hour),
		tmpl:     template.Must(template.ParseFS(templateFS, "templates/*.html")),
		secure:   secure,
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

	// Client API (bearer token), needed by Lane C. Control channel is HTTPS in
	// production (cmd/server serves TLS when configured).
	mux.HandleFunc("GET /api/v1/config", s.requireClient(s.getConfig))
	mux.HandleFunc("POST /api/v1/status", s.requireClient(s.postStatus))

	return securityHeaders(mux)
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

// securityHeaders applies conservative defaults to every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
