package auth

import (
	"crypto/rand"
	"encoding/base64"
	"sync"
	"time"
)

// sessionEntry is one live login. userID is the account's stable identity: a
// session is bound to the row it was issued for, so deleting that row (and
// revoking by id) cannot leak access to a later account reusing the same name.
type sessionEntry struct {
	expiry   time.Time
	userID   int64
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
func (s *SessionStore) Create(userID int64, username string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[tok] = sessionEntry{expiry: s.nowFunc().Add(s.ttl), userID: userID, username: username}
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

// UserID returns the id of the user this session was issued for, or 0 if the
// token is unknown or expired.
func (s *SessionStore) UserID(tok string) int64 {
	if tok == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tokens[tok]
	if !ok || !s.nowFunc().Before(e.expiry) {
		return 0
	}
	return e.userID
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

// DeleteByUserID invalidates every session belonging to userID. Call it when an
// account is deleted: without it a deleted user keeps full admin access until
// the session TTL expires (M1).
func (s *SessionStore) DeleteByUserID(userID int64) {
	if userID == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, e := range s.tokens {
		if e.userID == userID {
			delete(s.tokens, tok)
		}
	}
}
