package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

type sessionEntry struct {
	expiry   time.Time
	username string
}

// SessionStore is an in-memory store of session tokens with TTL. Sessions drop
// on restart (acceptable: the user just logs in again).
type SessionStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	tokens  map[string]sessionEntry
	nowFunc func() time.Time
}

// NewSessionStore returns a store whose sessions live for ttl.
func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{ttl: ttl, tokens: map[string]sessionEntry{}, nowFunc: time.Now}
}

// Create issues a new opaque session token for the given user, valid for the store's TTL.
func (s *SessionStore) Create(username string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[tok] = sessionEntry{expiry: s.nowFunc().Add(s.ttl), username: username}
	return tok, nil
}

// Valid reports whether tok is a live session, lazily evicting if expired.
func (s *SessionStore) Valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if !s.nowFunc().Before(e.expiry) {
		delete(s.tokens, tok)
		return false
	}
	return true
}

// Username returns the username associated with tok, or "" if the token is
// unknown or expired.
func (s *SessionStore) Username(tok string) string {
	if tok == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tokens[tok]
	if !ok || !s.nowFunc().Before(e.expiry) {
		return ""
	}
	return e.username
}

// Delete invalidates a session (logout).
func (s *SessionStore) Delete(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, tok)
}
