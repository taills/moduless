// Package auth provides credential verification against system_users and an
// in-memory session store. The in-memory store fits Core's single-instance
// deployment model (see docs/deployment.md); HA is intentionally out of scope.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	"golang.org/x/crypto/bcrypt"
)

// ErrInvalidCredentials is returned for any login failure (unknown user or bad
// password) without distinguishing the two, to avoid user enumeration.
var ErrInvalidCredentials = errors.New("invalid username or password")

// User is the authenticated identity surfaced to the host app and injected into
// extension requests by the gateway.
type User struct {
	ID       int32  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type session struct {
	user      User
	expiresAt time.Time
}

// Store verifies credentials and issues opaque session tokens kept in memory.
type Store struct {
	q   *sqlc.Queries
	ttl time.Duration

	mu       sync.RWMutex
	sessions map[string]session
}

// NewStore builds a session store backed by the given queries with a 24h TTL.
func NewStore(q *sqlc.Queries) *Store {
	return &Store{q: q, ttl: 24 * time.Hour, sessions: make(map[string]session)}
}

// Login verifies credentials with bcrypt and returns a fresh session token.
func (s *Store) Login(ctx context.Context, username, password string) (string, User, error) {
	row, err := s.q.GetUserByUsername(ctx, username)
	if err != nil {
		// Run a dummy compare so timing does not reveal whether the user exists.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$invalidinvalidinvalidinvalidinvalidinvalidinvalidinva"), []byte(password))
		return "", User{}, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(row.PasswordHash), []byte(password)) != nil {
		return "", User{}, ErrInvalidCredentials
	}

	user := User{ID: row.ID, Username: row.Username, Role: row.Role}
	token, err := newToken()
	if err != nil {
		return "", User{}, err
	}
	s.mu.Lock()
	s.sessions[token] = session{user: user, expiresAt: time.Now().Add(s.ttl)}
	s.mu.Unlock()
	return token, user, nil
}

// Resolve returns the user for a valid, unexpired token.
func (s *Store) Resolve(token string) (User, bool) {
	if token == "" {
		return User{}, false
	}
	s.mu.RLock()
	sess, ok := s.sessions[token]
	s.mu.RUnlock()
	if !ok {
		return User{}, false
	}
	if time.Now().After(sess.expiresAt) {
		s.mu.Lock()
		delete(s.sessions, token)
		s.mu.Unlock()
		return User{}, false
	}
	return sess.user, true
}

// Logout invalidates a token. It is a no-op for unknown tokens.
func (s *Store) Logout(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// SeedDefaultAdmin inserts a default admin user when system_users is empty.
// Returns true when a user was created.
func (s *Store) SeedDefaultAdmin(ctx context.Context, username, password string) (bool, error) {
	n, err := s.q.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return false, err
	}
	if err := s.q.CreateUser(ctx, sqlc.CreateUserParams{
		Username:     username,
		PasswordHash: string(hash),
		Role:         "admin",
	}); err != nil {
		return false, err
	}
	return true, nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
