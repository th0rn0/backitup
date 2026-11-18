package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"

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

// requireAdmin wraps a handler so it only runs for a valid session belonging to
// an account that still exists; otherwise it redirects to /login. The current
// username is available via usernameFromContext.
//
// The existence check on every request is deliberate (M1): a session outlives
// the user row it was issued for, so a deleted account would otherwise keep full
// admin access until the 12-hour TTL expired.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.Valid(c.Value) {
			s.redirectToLogin(w, r)
			return
		}
		userCtx, cancel := s.storeCtx(r)
		defer cancel()
		user, err := s.st.GetUserByID(userCtx, s.sessions.UserID(c.Value))
		if err != nil {
			// Fail closed: an account we cannot verify is not an account. But a
			// store that did not answer is a different fault from an invalid
			// session, and bouncing the admin to /login hides it behind a page
			// that needs the same store to work. Name it instead.
			if s.storeFailure(w, userCtx, "requireAdmin: verify session user", err) {
				return
			}
			s.redirectToLogin(w, r)
			return
		}
		if user == nil {
			// The account is gone; drop the orphaned token as well.
			s.sessions.Delete(c.Value)
			s.redirectToLogin(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), ctxUsername, user.Username)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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

	ctx, cancel := s.storeCtx(r)
	defer cancel()
	user, err := s.st.GetUserByUsername(ctx, username)
	// Generic message on every failure path — no user enumeration (design doc).
	const invalid = "Invalid username or password."
	if err != nil {
		// A store that did not answer is not a wrong password, and it must not be
		// counted as a failed attempt: ten of those lock the operator out of
		// their own server for a fault that has nothing to do with them.
		if s.storeFailure(w, ctx, fmt.Sprintf("login: load user %q", username), err) {
			return
		}
		s.limiter.record(r)
		s.renderLogin(w, http.StatusUnauthorized, invalid)
		return
	}
	if user == nil {
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

	tok, err := s.sessions.Create(user.ID, user.Username)
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
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	view, err := s.buildDashboard(ctx)
	if err != nil {
		if s.storeFailure(w, ctx, "fleet status: build dashboard", err) {
			return
		}
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
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	view, err := s.buildDashboard(ctx)
	if err != nil {
		if s.storeFailure(w, ctx, "dashboard: build view", err) {
			return
		}
		http.Error(w, "failed to load fleet", http.StatusInternalServerError)
		return
	}
	view.Username = username
	view.ActivePage = "dashboard"
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "dashboard.html", view); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}
