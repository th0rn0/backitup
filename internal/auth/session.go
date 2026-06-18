package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// SessionStore is an in-memory store of admin session tokens with TTL. Sessions
// drop on restart (acceptable: the admin just logs in again). A single-admin
// homelab tool does not need persistent sessions.
type SessionStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	tokens  map[string]time.Time // token -> expiry
	nowFunc func() time.Time     // injectable for tests
}

// NewSessionStore returns a store whose sessions live for ttl.
func NewSessionStore(ttl time.Duration) *SessionStore {
	return &SessionStore{ttl: ttl, tokens: map[string]time.Time{}, nowFunc: time.Now}
}

// Create issues a new opaque session token valid for the store's TTL.
func (s *SessionStore) Create() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[tok] = s.nowFunc().Add(s.ttl)
	return tok, nil
}

// Valid reports whether tok is a live session, lazily evicting if expired.
func (s *SessionStore) Valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.tokens[tok]
	if !ok {
		return false
	}
	if !s.nowFunc().Before(exp) {
		delete(s.tokens, tok)
		return false
	}
	return true
}

// Delete invalidates a session (logout).
func (s *SessionStore) Delete(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, tok)
}
