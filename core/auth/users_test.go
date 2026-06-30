package auth

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/taills/moduless/core/db"
	sqlc "github.com/taills/moduless/core/db/sqlc"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	connStr := os.Getenv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping user-admin integration test")
	}
	conn, err := db.InitDB(connStr) // runs migrations
	if err != nil {
		t.Skipf("cannot init test database: %v", err)
	}
	if _, err := conn.Exec("TRUNCATE system_users RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return NewStore(sqlc.New(conn))
}

func TestUserAdminGuards(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Seed the sole admin and a regular user.
	if _, err := s.SeedDefaultAdmin(ctx, "admin", "admin123"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.CreateUser(ctx, "bob", "secret", "user"); err != nil {
		t.Fatalf("create user: %v", err)
	}

	users, err := s.ListUsers(ctx)
	if err != nil || len(users) != 2 {
		t.Fatalf("expected 2 users, got %d err=%v", len(users), err)
	}

	admin := findUser(t, users, "admin")
	bob := findUser(t, users, "bob")

	// Deleting the last admin is refused.
	if err := s.DeleteUser(ctx, admin.ID); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("expected ErrLastAdmin deleting sole admin, got %v", err)
	}
	// Demoting the last admin is refused.
	if err := s.SetRole(ctx, admin.ID, "user"); !errors.Is(err, ErrLastAdmin) {
		t.Fatalf("expected ErrLastAdmin demoting sole admin, got %v", err)
	}
	// A non-admin can be deleted freely.
	if err := s.DeleteUser(ctx, bob.ID); err != nil {
		t.Fatalf("delete regular user: %v", err)
	}

	// Promote a second admin, then the original may be removed.
	if err := s.CreateUser(ctx, "carol", "secret", "admin"); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	if err := s.DeleteUser(ctx, admin.ID); err != nil {
		t.Fatalf("delete admin with a spare admin present: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.CreateUser(ctx, "dave", "oldpass", "user"); err != nil {
		t.Fatalf("create: %v", err)
	}
	dave := findUser(t, mustList(t, s, ctx), "dave")

	if err := s.ChangePassword(ctx, dave.ID, "wrong", "newpass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if err := s.ChangePassword(ctx, dave.ID, "oldpass", "newpass"); err != nil {
		t.Fatalf("change password: %v", err)
	}
	if _, _, err := s.Login(ctx, "dave", "newpass"); err != nil {
		t.Fatalf("login with new password: %v", err)
	}
}

func mustList(t *testing.T, s *Store, ctx context.Context) []UserRecord {
	t.Helper()
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	return users
}

func findUser(t *testing.T, users []UserRecord, name string) UserRecord {
	t.Helper()
	for _, u := range users {
		if u.Username == name {
			return u
		}
	}
	t.Fatalf("user %q not found", name)
	return UserRecord{}
}
