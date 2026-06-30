package sdk

import (
	"context"
	"net/http"
	"testing"
)

func TestContextExtraction(t *testing.T) {
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-User-Id", "10001")
	req.Header.Set("X-User-Roles", "admin,user")

	userCtx := &UserContext{
		UserID: req.Header.Get("X-User-Id"),
		Roles:  splitNonEmpty(req.Header.Get("X-User-Roles")),
	}
	ctx := context.WithValue(req.Context(), userContextKey, userCtx)

	user := GetUser(ctx)
	if user == nil || user.UserID != "10001" {
		t.Fatalf("expected user id 10001, got %v", user)
	}
	if len(user.Roles) != 2 || user.Roles[0] != "admin" {
		t.Fatalf("expected admin role, got %v", user.Roles)
	}
}

func TestGetUserMissing(t *testing.T) {
	if u := GetUser(context.Background()); u != nil {
		t.Fatalf("expected nil user, got %v", u)
	}
}

func TestSplitNonEmpty(t *testing.T) {
	if got := splitNonEmpty(""); got != nil {
		t.Fatalf("expected nil for empty string, got %v", got)
	}
	if got := splitNonEmpty("a,b,c"); len(got) != 3 {
		t.Fatalf("expected 3 elements, got %v", got)
	}
}
