package server

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
)

type contextKey int

const ctxUsername contextKey = iota

func usernameFromContext(ctx context.Context) string {
	s, _ := ctx.Value(ctxUsername).(string)
	return s
}

// mustSub roots the embedded asset FS at assets/ so /static/app.css maps to
// assets/app.css.
func mustSub(f fs.FS) fs.FS {
	sub, err := fs.Sub(f, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

// requireAdmin wraps a handler so it only runs for a valid session; otherwise
// it redirects to /login. The current username is available via usernameFromContext.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.Valid(c.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		username := s.sessions.Username(c.Value)
		ctx := context.WithValue(r.Context(), ctxUsername, username)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) getLogin(w http.ResponseWriter, r *http.Request) {
	s.renderLogin(w, http.StatusOK, "")
}

func (s *Server) renderLogin(w http.ResponseWriter, code int, errMsg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_ = s.tmpl.ExecuteTemplate(w, "login.html", map[string]any{"Error": errMsg})
}

func (s *Server) postLogin(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(r) {
		http.Error(w, "Too many login attempts. Try again in a minute.", http.StatusTooManyRequests)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, http.StatusBadRequest, "Invalid form.")
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	user, err := s.st.GetUserByUsername(ctx, username)
	// Generic message on every failure path — no user enumeration (design doc).
	const invalid = "Invalid username or password."
	if err != nil || user == nil {
		s.limiter.record(r)
		s.renderLogin(w, http.StatusUnauthorized, invalid)
		return
	}
	ok, err := auth.VerifyPassword(password, user.PasswordHash)
	if err != nil || !ok {
		s.limiter.record(r)
		s.renderLogin(w, http.StatusUnauthorized, invalid)
		return
	}

	tok, err := s.sessions.Create(username)
	if err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) postLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type fleetStatusResp struct {
	Summary struct {
		OK      int `json:"ok"`
		Stale   int `json:"stale"`
		Failed  int `json:"failed"`
		Never   int `json:"never"`
		Running int `json:"running"`
	} `json:"summary"`
	Clients []fleetClientStatus `json:"clients"`
}

type fleetClientStatus struct {
	Slug        string `json:"slug"`
	Health      string `json:"health"`
	HealthLabel string `json:"health_label"`
	Icon        string `json:"icon"`
	LastBackup  string `json:"last_backup"`
	Size        string `json:"size"`
}

func (s *Server) getFleetStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	view, err := s.buildDashboard(ctx)
	if err != nil {
		http.Error(w, "failed to load fleet", http.StatusInternalServerError)
		return
	}
	var resp fleetStatusResp
	resp.Summary.OK = view.Summary.OK
	resp.Summary.Stale = view.Summary.Stale
	resp.Summary.Failed = view.Summary.Failed
	resp.Summary.Never = view.Summary.Never
	resp.Summary.Running = view.Summary.Running
	for _, c := range view.Clients {
		resp.Clients = append(resp.Clients, fleetClientStatus{
			Slug: c.Slug, Health: c.Health, HealthLabel: c.HealthLabel,
			Icon: c.Icon, LastBackup: c.LastBackup, Size: c.Size,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	username := usernameFromContext(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	view, err := s.buildDashboard(ctx)
	if err != nil {
		http.Error(w, "failed to load fleet", http.StatusInternalServerError)
		return
	}
	view.Username = username
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "dashboard.html", view); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}
