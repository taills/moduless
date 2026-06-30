// Package extension implements the admin-managed extension registry and the
// approval workflow that gates which extensions Core will route. An extension is
// first seen as "pending" when it dials Core without a valid secret; an admin
// then approves it (Core mints a per-instance secret) or rejects it. A single
// extension key may own many secrets so multi-replica deployments can each carry
// an independently revocable credential.
package extension

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
	"golang.org/x/crypto/bcrypt"
)

// Approval status values stored in the extensions.status column.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusRejected = "rejected"
)

// Store is the database-backed extension registry.
type Store struct {
	q *sqlc.Queries
}

// NewStore builds a registry backed by the given queries.
func NewStore(q *sqlc.Queries) *Store {
	return &Store{q: q}
}

// Authenticate decides how Core should treat an incoming registration request,
// implementing tunnel.Authenticator. It records first-time extensions as pending
// so admins can review them.
func (s *Store) Authenticate(ctx context.Context, req *pb.RegisterRequest) (tunnel.AuthResult, error) {
	key := req.ExtensionKey
	ext, err := s.q.GetExtension(ctx, key)
	notFound := errors.Is(err, sql.ErrNoRows)
	if err != nil && !notFound {
		return tunnel.AuthResult{}, fmt.Errorf("lookup extension %s: %w", key, err)
	}

	// A secret was presented: it must match an active secret on an approved key.
	if req.ExtensionSecret != "" {
		ok, verr := s.verifySecret(ctx, key, req.ExtensionSecret)
		if verr != nil {
			return tunnel.AuthResult{}, verr
		}
		if ok && !notFound && ext.Status == StatusApproved {
			return tunnel.AuthResult{Action: tunnel.AuthApprove}, nil
		}
		return tunnel.AuthResult{Action: tunnel.AuthDeny, Message: "invalid or revoked extension secret"}, nil
	}

	// No secret presented.
	switch {
	case notFound || ext.Status == StatusPending:
		if err := s.recordPending(ctx, req); err != nil {
			return tunnel.AuthResult{}, err
		}
		return tunnel.AuthResult{Action: tunnel.AuthPending}, nil
	case ext.Status == StatusApproved:
		return tunnel.AuthResult{Action: tunnel.AuthDeny,
			Message: "extension already approved; provide a secret issued from the console"}, nil
	default: // rejected
		return tunnel.AuthResult{Action: tunnel.AuthReject,
			Message: "registration rejected by administrator"}, nil
	}
}

// IsApproved reports whether key is an approved extension. Used to gate
// data-plane (DB/File/Event) unary calls.
func (s *Store) IsApproved(ctx context.Context, key string) bool {
	ext, err := s.q.GetExtension(ctx, key)
	if err != nil {
		return false
	}
	return ext.Status == StatusApproved
}

// recordPending upserts the pending registry row from the request metadata.
func (s *Store) recordPending(ctx context.Context, req *pb.RegisterRequest) error {
	_, err := s.q.UpsertPendingExtension(ctx, sqlc.UpsertPendingExtensionParams{
		Key:         req.ExtensionKey,
		DisplayName: req.DisplayName,
		Version:     req.Version,
		MenuIcon:    req.MenuIcon,
		MenuPath:    req.MenuPath,
	})
	return err
}

// verifySecret reports whether secret matches any active secret for key. On a
// match the secret's last_used_at is refreshed (best effort).
func (s *Store) verifySecret(ctx context.Context, key, secret string) (bool, error) {
	secrets, err := s.q.ListActiveExtensionSecrets(ctx, key)
	if err != nil {
		return false, fmt.Errorf("list secrets for %s: %w", key, err)
	}
	for _, row := range secrets {
		if bcrypt.CompareHashAndPassword([]byte(row.SecretHash), []byte(secret)) == nil {
			_ = s.q.TouchExtensionSecret(ctx, row.ID)
			return true, nil
		}
	}
	return false, nil
}

// mintSecret generates, hashes and stores a new secret for key, returning the
// plaintext (shown only once).
func (s *Store) mintSecret(ctx context.Context, key, label string) (string, error) {
	secret, err := newSecret()
	if err != nil {
		return "", err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	if _, err := s.q.CreateExtensionSecret(ctx, sqlc.CreateExtensionSecretParams{
		ExtensionKey: key,
		SecretHash:   string(hash),
		Label:        label,
	}); err != nil {
		return "", fmt.Errorf("store secret for %s: %w", key, err)
	}
	return secret, nil
}

// newSecret returns a 68-byte opaque secret ("ext_" + 32 random bytes hex).
func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ext_" + hex.EncodeToString(b), nil
}
