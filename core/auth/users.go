package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	"golang.org/x/crypto/bcrypt"
)

// Errors returned by the user-administration operations.
var (
	ErrLastAdmin   = errors.New("cannot remove the last administrator")
	ErrUserMissing = errors.New("user not found")
)

// UserRecord is a user as surfaced to the admin console (no password hash).
type UserRecord struct {
	ID        int32      `json:"id"`
	Username  string     `json:"username"`
	Role      string     `json:"role"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// ListUsers returns all system users without password material.
func (s *Store) ListUsers(ctx context.Context) ([]UserRecord, error) {
	rows, err := s.q.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]UserRecord, 0, len(rows))
	for _, r := range rows {
		rec := UserRecord{ID: r.ID, Username: r.Username, Role: r.Role}
		if r.CreatedAt.Valid {
			rec.CreatedAt = &r.CreatedAt.Time
		}
		out = append(out, rec)
	}
	return out, nil
}

// CreateUser hashes the password and inserts a new user.
func (s *Store) CreateUser(ctx context.Context, username, password, role string) error {
	if username == "" || password == "" {
		return errors.New("username and password are required")
	}
	if role == "" {
		role = "user"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.q.CreateUser(ctx, sqlc.CreateUserParams{
		Username:     username,
		PasswordHash: string(hash),
		Role:         role,
	})
}

// SetPassword resets a user's password.
func (s *Store) SetPassword(ctx context.Context, id int32, password string) error {
	if password == "" {
		return errors.New("password is required")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	return s.q.UpdateUserPassword(ctx, sqlc.UpdateUserPasswordParams{ID: id, PasswordHash: string(hash)})
}

// SetRole changes a user's role, refusing to demote the last administrator.
func (s *Store) SetRole(ctx context.Context, id int32, role string) error {
	if role == "" {
		return errors.New("role is required")
	}
	if role != "admin" {
		if err := s.ensureNotLastAdmin(ctx, id); err != nil {
			return err
		}
	}
	return s.q.UpdateUserRole(ctx, sqlc.UpdateUserRoleParams{ID: id, Role: role})
}

// DeleteUser removes a user, refusing to remove the last administrator.
func (s *Store) DeleteUser(ctx context.Context, id int32) error {
	if err := s.ensureNotLastAdmin(ctx, id); err != nil {
		return err
	}
	return s.q.DeleteUser(ctx, id)
}

// ChangePassword verifies the current password before setting a new one.
func (s *Store) ChangePassword(ctx context.Context, id int32, oldPassword, newPassword string) error {
	row, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return ErrUserMissing
	}
	if bcrypt.CompareHashAndPassword([]byte(row.PasswordHash), []byte(oldPassword)) != nil {
		return ErrInvalidCredentials
	}
	return s.SetPassword(ctx, id, newPassword)
}

// ensureNotLastAdmin returns ErrLastAdmin when id is the only admin left.
func (s *Store) ensureNotLastAdmin(ctx context.Context, id int32) error {
	row, err := s.q.GetUserByID(ctx, id)
	if err != nil {
		return ErrUserMissing
	}
	if row.Role != "admin" {
		return nil
	}
	n, err := s.q.CountAdmins(ctx)
	if err != nil {
		return fmt.Errorf("count admins: %w", err)
	}
	if n <= 1 {
		return ErrLastAdmin
	}
	return nil
}
