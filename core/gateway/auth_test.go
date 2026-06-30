package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
)

type stubResolver struct{}

func (stubResolver) Resolve(token string) (auth.User, bool) {
	if token == "good" {
		return auth.User{ID: 7, Username: "admin", Role: "admin"}, true
	}
	return auth.User{}, false
}

func TestAppsHandlerRequiresAuth(t *testing.T) {
	gw := NewGatewayHandler(tunnel.NewTunnelManager())
	gw.Auth = stubResolver{}

	w := httptest.NewRecorder()
	gw.AppsHandler(w, httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", w.Code)
	}
}

func TestAppsHandlerListsRegisteredExtensions(t *testing.T) {
	mgr := tunnel.NewTunnelManager()
	mgr.Register("go_example", nil, &pb.RegisterRequest{
		ExtensionKey: "go_example",
		DisplayName:  "Go 示例",
		MenuPath:     "/go",
	})
	gw := NewGatewayHandler(mgr)
	gw.Auth = stubResolver{}

	req := httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil)
	req.Header.Set("Authorization", "Bearer good")
	w := httptest.NewRecorder()
	gw.AppsHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var apps []AppInfo
	if err := json.Unmarshal(w.Body.Bytes(), &apps); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(apps) != 1 {
		t.Fatalf("expected 1 app, got %d", len(apps))
	}
	got := apps[0]
	if got.Key != "go_example" || got.DisplayName != "Go 示例" || got.Entry != "/extensions/go_example/" {
		t.Fatalf("unexpected app info: %+v", got)
	}
}

func TestProxyRejectsUnauthenticated(t *testing.T) {
	mgr := tunnel.NewTunnelManager()
	mgr.Register("go_example", nil, &pb.RegisterRequest{ExtensionKey: "go_example"})
	gw := NewGatewayHandler(mgr)
	gw.Auth = stubResolver{}

	// A registered tunnel exists, but without a valid session the gateway must
	// reject the call before forwarding it.
	w := httptest.NewRecorder()
	gw.proxyToExtension(w, httptest.NewRequest(http.MethodPost, "/api/extensions/go_example/items", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated extension call, got %d", w.Code)
	}
}

func TestCookieTokenResolved(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil)
	req.AddCookie(&http.Cookie{Name: "moduless_token", Value: "abc"})
	if SessionToken(req) != "abc" {
		t.Fatal("cookie token not resolved")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set("Authorization", "Bearer xyz")
	if SessionToken(req2) != "xyz" {
		t.Fatal("bearer token not preferred")
	}
}
