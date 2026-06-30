package auth

import (
	"testing"
	"time"
)

func TestSessionResolveAndLogout(t *testing.T) {
	s := NewStore(nil) // queries unused by Resolve/Logout
	s.sessions["tok"] = session{
		user:      User{ID: 1, Username: "admin", Role: "admin"},
		expiresAt: time.Now().Add(time.Hour),
	}

	u, ok := s.Resolve("tok")
	if !ok || u.Username != "admin" {
		t.Fatalf("valid token not resolved: %+v ok=%v", u, ok)
	}
	if _, ok := s.Resolve("unknown"); ok {
		t.Fatal("unknown token resolved")
	}
	if _, ok := s.Resolve(""); ok {
		t.Fatal("empty token resolved")
	}

	s.Logout("tok")
	if _, ok := s.Resolve("tok"); ok {
		t.Fatal("token still valid after logout")
	}
}

func TestSessionExpiry(t *testing.T) {
	s := NewStore(nil)
	s.sessions["old"] = session{
		user:      User{ID: 1},
		expiresAt: time.Now().Add(-time.Minute),
	}
	if _, ok := s.Resolve("old"); ok {
		t.Fatal("expired token resolved")
	}
	// Expired session should be evicted on resolve.
	if _, exists := s.sessions["old"]; exists {
		t.Fatal("expired session not evicted")
	}
}
