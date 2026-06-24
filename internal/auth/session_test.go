package auth

import (
	"testing"
	"time"
)

func TestSessionLifecycle(t *testing.T) {
	s := NewSessionStore(time.Hour)
	tok, err := s.Create("alice")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !s.Valid(tok) {
		t.Fatal("fresh session should be valid")
	}
	if s.Username(tok) != "alice" {
		t.Fatalf("username = %q, want alice", s.Username(tok))
	}
	if s.Valid("not-a-real-token") {
		t.Fatal("bogus token should be invalid")
	}
	if s.Valid("") {
		t.Fatal("empty token should be invalid")
	}
	s.Delete(tok)
	if s.Valid(tok) {
		t.Fatal("deleted session should be invalid")
	}
	if s.Username(tok) != "" {
		t.Fatal("username after delete should be empty")
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewSessionStore(time.Hour)
	now := time.Unix(1_000_000, 0)
	s.nowFunc = func() time.Time { return now }
	tok, _ := s.Create("bob")
	if !s.Valid(tok) {
		t.Fatal("should be valid before expiry")
	}
	now = now.Add(2 * time.Hour) // advance past TTL
	if s.Valid(tok) {
		t.Fatal("should be invalid after expiry")
	}
}

func TestSessionTokensUnique(t *testing.T) {
	s := NewSessionStore(time.Hour)
	a, _ := s.Create("u1")
	b, _ := s.Create("u2")
	if a == b {
		t.Fatal("session tokens should be unique")
	}
}
