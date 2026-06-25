package server

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/th0rn0/backitup/internal/auth"
)

func (s *Server) getUsers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	users, err := s.st.ListUsers(ctx)
	if err != nil {
		http.Error(w, "failed to load users", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "users.html", map[string]any{
		"Username":   usernameFromContext(r.Context()),
		"ActivePage": "users",
		"Users":      users,
	})
}

func (s *Server) postCreateUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	username := r.PostFormValue("username")
	password := r.PostFormValue("password")
	if username == "" || password == "" {
		s.renderUsersError(w, r, "Username and password are required.")
		return
	}
	if len(password) < 8 {
		s.renderUsersError(w, r, "Password must be at least 8 characters.")
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		http.Error(w, "hash error", http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.st.CreateUser(ctx, username, hash); err != nil {
		s.renderUsersError(w, r, "Could not create user (is the username already taken?).")
		return
	}
	log.Printf("user created: %q by %q", username, usernameFromContext(r.Context()))
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) postDeleteUser(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		http.Error(w, "confirmation required", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Guard: cannot delete the last user (would lock everyone out).
	n, err := s.st.CountUsers(ctx)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if n <= 1 {
		http.Error(w, "cannot delete the last user", http.StatusBadRequest)
		return
	}

	if err := s.st.DeleteUser(ctx, id); err != nil {
		http.NotFound(w, r)
		return
	}
	log.Printf("user deleted: id=%d by %q", id, usernameFromContext(r.Context()))
	http.Redirect(w, r, "/users", http.StatusSeeOther)
}

func (s *Server) renderUsersError(w http.ResponseWriter, r *http.Request, msg string) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	users, _ := s.st.ListUsers(ctx)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = s.tmpl.ExecuteTemplate(w, "users.html", map[string]any{
		"Username":   usernameFromContext(r.Context()),
		"ActivePage": "users",
		"Users":      users,
		"Error":      msg,
	})
}
