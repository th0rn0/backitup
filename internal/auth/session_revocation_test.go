package auth

import (
	"testing"
	"time"
)

// M1: deleting a user must invalidate the sessions it still holds, and only
// that user's sessions. Keyed by user id, so a later account that reuses the
// same username cannot inherit the old tokens.
func TestDeleteByUserIDRevokesOnlyThatUsersSessions(t *testing.T) {
	s := NewSessionStore(time.Hour)
	a1, err := s.Create(1, "alice")
	if err != nil {
		t.Fatalf("create a1: %v", err)
	}
	a2, err := s.Create(1, "alice")
	if err != nil {
		t.Fatalf("create a2: %v", err)
	}
	b, err := s.Create(2, "bob")
	if err != nil {
		t.Fatalf("create b: %v", err)
	}

	s.DeleteByUserID(1)

	if s.Valid(a1) || s.Valid(a2) {
		t.Fatal("sessions of the deleted user are still valid")
	}
	if s.UserID(a1) != 0 || s.Username(a1) != "" {
		t.Fatalf("deleted session still resolves: id=%d username=%q", s.UserID(a1), s.Username(a1))
	}
	if !s.Valid(b) || s.UserID(b) != 2 {
		t.Fatal("revoking one user's sessions must not touch another user's")
	}
}

func TestSessionCarriesUserID(t *testing.T) {
	s := NewSessionStore(time.Hour)
	tok, err := s.Create(42, "carol")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := s.UserID(tok); got != 42 {
		t.Fatalf("UserID = %d, want 42", got)
	}
	if got := s.Username(tok); got != "carol" {
		t.Fatalf("Username = %q, want carol", got)
	}
	if got := s.UserID("bogus"); got != 0 {
		t.Fatalf("UserID(bogus) = %d, want 0", got)
	}
}
