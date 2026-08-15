package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

type stubResolver struct{}

func (stubResolver) Resolve(token string) (auth.User, bool) {
	if token == "good" {
		return auth.User{ID: 7, Username: "admin", Role: "admin"}, true
	}
	return auth.User{}, false
}

// fakePlugins stands in for the plugin manager as the gateway sees it.
type fakePlugins struct {
	statuses []pluginhost.Status
	packages map[string]*pluginhost.Package
}

func (f fakePlugins) List() []pluginhost.Status { return f.statuses }
func (f fakePlugins) Package(key string) (*pluginhost.Package, bool) {
	pkg, ok := f.packages[key]
	return pkg, ok
}

func pluginsWith(key, displayName string, menus []manifest.MenuItem, ready int) fakePlugins {
	return fakePlugins{
		statuses: []pluginhost.Status{{
			Key: key, DisplayName: displayName, Version: "1.0.0",
			Enabled: true, Replicas: 1, Ready: ready,
		}},
		packages: map[string]*pluginhost.Package{
			key: {Manifest: &manifest.Manifest{Key: key, DisplayName: displayName, Menus: menus}},
		},
	}
}

func TestAppsHandlerRequiresAuth(t *testing.T) {
	gw := NewGatewayHandler()
	gw.Auth = stubResolver{}

	req := httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil)
	req.Header.Set("Authorization", "Bearer bad")
	w := httptest.NewRecorder()
	gw.AppsHandler(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestAppsHandlerListsEnabledPlugins(t *testing.T) {
	gw := NewGatewayHandler()
	gw.Auth = stubResolver{}
	gw.Plugins = pluginsWith("notes", "笔记", []manifest.MenuItem{
		{Path: "/notes", Title: "笔记", Icon: "file", Order: 10},
	}, 1)

	req := httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil)
	req.Header.Set("Authorization", "Bearer good")
	w := httptest.NewRecorder()
	gw.AppsHandler(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp AppsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(resp.Apps) != 1 || resp.Apps[0].Key != "notes" {
		t.Fatalf("apps = %+v", resp.Apps)
	}
	if resp.Apps[0].Entry != "/plugins/notes/" {
		t.Errorf("entry = %q, want the plugin asset path", resp.Apps[0].Entry)
	}
	// Core merges the menu tree itself now, so the response carries the
	// assembled result rather than leaving the console to do it.
	if len(resp.Menu) != 1 || resp.Menu[0].Path != "/notes" {
		t.Fatalf("merged menu = %+v", resp.Menu)
	}
	// A leaf with no explicit entry mounts the plugin's own micro-frontend.
	if resp.Menu[0].Entry != "/plugins/notes/" {
		t.Errorf("menu entry = %q", resp.Menu[0].Entry)
	}
}

// A plugin with no ready replica is omitted rather than listed as offline: its
// micro-frontend would fail to load, so a menu entry pointing at it would be a
// broken link.
func TestAppsHandlerOmitsPluginsWithNoReadyReplica(t *testing.T) {
	gw := NewGatewayHandler()
	gw.Auth = stubResolver{}
	gw.Plugins = pluginsWith("notes", "笔记", []manifest.MenuItem{{Path: "/notes", Title: "笔记"}}, 0)

	req := httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil)
	req.Header.Set("Authorization", "Bearer good")
	w := httptest.NewRecorder()
	gw.AppsHandler(w, req)

	var resp AppsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Apps) != 0 || len(resp.Menu) != 0 {
		t.Errorf("a plugin with no ready replica was listed: %+v", resp)
	}
}

// The console mirrors its token into a cookie so same-origin micro-frontends
// can call the API without re-implementing auth.
func TestCookieTokenResolved(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil)
	req.AddCookie(&http.Cookie{Name: "moduless_token", Value: "good"})

	if got := SessionToken(req); got != "good" {
		t.Fatalf("SessionToken = %q, want the cookie value", got)
	}
}

// An Authorization header wins over the cookie.
func TestBearerTokenTakesPrecedence(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/system/ui/apps", nil)
	req.AddCookie(&http.Cookie{Name: "moduless_token", Value: "from-cookie"})
	req.Header.Set("Authorization", "Bearer from-header")

	if got := SessionToken(req); got != "from-header" {
		t.Fatalf("SessionToken = %q, want the header value", got)
	}
}
