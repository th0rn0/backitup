// Package server builds backitup's HTTP control plane (design doc D1). Keeping
// handler construction here (rather than in cmd/server) makes it testable with
// httptest. Lanes B/D add admin auth, the webgui, /api/v1/*, and the lifecycle
// timer.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/th0rn0/backitup/internal/store"
)

// Server holds the control plane's dependencies.
type Server struct {
	st *store.Store
}

// New returns a Server backed by the given store.
func New(st *store.Store) *Server { return &Server{st: st} }

// Handler builds the HTTP routes. TODO(Lane B): TLS, admin login (D3), webgui,
// GET /api/v1/config, POST /api/v1/status.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	return mux
}

// healthz reports readiness by touching the store, so it fails loudly if the DB
// is unreachable rather than reporting a false green.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := s.st.ListClients(ctx); err != nil {
		http.Error(w, "db error", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
