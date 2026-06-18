package server

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
)

// mustSub roots the embedded asset FS at assets/ so /static/app.css maps to
// assets/app.css.
func mustSub(f fs.FS) fs.FS {
	sub, err := fs.Sub(f, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

// requireAdmin wraps a handler so it only runs for a valid admin session;
// otherwise it redirects to /login.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil || !s.sessions.Valid(c.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
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
	if err := r.ParseForm(); err != nil {
		s.renderLogin(w, http.StatusBadRequest, "Invalid form.")
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	admin, err := s.st.GetAdmin(ctx)
	// Generic message on every failure path — no user enumeration (design doc).
	const invalid = "Invalid username or password."
	if err != nil || admin == nil || admin.Username != username {
		s.renderLogin(w, http.StatusUnauthorized, invalid)
		return
	}
	ok, err := auth.VerifyPassword(password, admin.PasswordHash)
	if err != nil || !ok {
		s.renderLogin(w, http.StatusUnauthorized, invalid)
		return
	}

	tok, err := s.sessions.Create()
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

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	view, err := s.buildDashboard(ctx)
	if err != nil {
		http.Error(w, "failed to load fleet", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.ExecuteTemplate(w, "dashboard.html", view); err != nil {
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}
