package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pluginhost"
)

// The console's menu, end to end.
//
// This is the requirement in its own words: when a plugin is enabled the
// console shows its menus and pages, and when it is disabled or removed those
// disappear. Everything else about that behaviour is already covered — the SSE
// stream that tells the browser to refetch, the manifest parsing, the tree
// merge — but nothing checked the thing itself: that what the console is served
// actually changes when a plugin's state does.

type appsPayload struct {
	Apps []struct {
		Key         string `json:"key"`
		DisplayName string `json:"display_name"`
		Online      bool   `json:"online"`
	} `json:"apps"`
	Menu []menuNode `json:"menu"`
}

type menuNode struct {
	Path     string     `json:"path"`
	Title    string     `json:"title"`
	Icon     string     `json:"icon"`
	Entry    string     `json:"entry"`
	Children []menuNode `json:"children"`
}

// findMenu walks the tree for a path.
func findMenu(nodes []menuNode, path string) (menuNode, bool) {
	for _, n := range nodes {
		if n.Path == path {
			return n, true
		}
		if found, ok := findMenu(n.Children, path); ok {
			return found, true
		}
	}
	return menuNode{}, false
}

func fetchApps(t *testing.T, url string) appsPayload {
	t.Helper()

	resp, err := http.Get(url + "/api/system/ui/apps")
	if err != nil {
		t.Fatalf("GET apps: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("apps returned %d", resp.StatusCode)
	}

	var out appsPayload
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding the apps payload: %v", err)
	}
	return out
}

// menuStack serves the console's apps endpoint over a real manager.
func menuStack(t *testing.T) (url string, mgr *pluginhost.Manager) {
	t.Helper()

	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, _ = newManagerOver(t, root, nil)
	mgr.Scan()

	// Auth is nil, so no session is required and the role is empty — a plugin's
	// role-restricted entries must therefore stay hidden.
	h := &gateway.GatewayHandler{Plugins: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system/ui/apps", h.AppsHandler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL, mgr
}

// The requirement, directly: enabling a plugin puts its menus in front of the
// user, disabling it takes them away.
func TestMenuAppearsAndDisappearsWithThePlugin(t *testing.T) {
	url, mgr := menuStack(t)
	ctx := context.Background()

	// Before enabling: nothing.
	before := fetchApps(t, url)
	if len(before.Apps) != 0 {
		t.Fatalf("%d app(s) listed before anything was enabled", len(before.Apps))
	}
	if _, found := findMenu(before.Menu, "/echo"); found {
		t.Fatal("a disabled plugin's menu is already in the tree")
	}

	// Enabled: the menu is there.
	if err := mgr.Enable(ctx, "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	enabled := fetchApps(t, url)
	if len(enabled.Apps) != 1 {
		t.Fatalf("%d app(s) listed after enabling one", len(enabled.Apps))
	}
	if got := enabled.Apps[0].DisplayName; got != "Echo fixture" {
		t.Errorf("display name = %q, want the manifest's", got)
	}

	node, found := findMenu(enabled.Menu, "/echo")
	if !found {
		t.Fatal("the enabled plugin's menu is not in the tree")
	}
	if node.Title != "Echo" || node.Icon != "box" {
		t.Errorf("menu node = %+v, want the manifest's title and icon", node)
	}
	// A leaf with no explicit entry mounts the plugin's own micro-frontend.
	if node.Entry == "" {
		t.Error("the leaf menu has no entry, so the console has nothing to mount")
	}

	// Disabled: gone again.
	if err := mgr.Disable(ctx, "echo"); err != nil {
		t.Fatalf("disable: %v", err)
	}

	after := fetchApps(t, url)
	if len(after.Apps) != 0 {
		t.Errorf("%d app(s) still listed after disabling", len(after.Apps))
	}
	if _, found := findMenu(after.Menu, "/echo"); found {
		t.Error("the disabled plugin's menu is still in the tree; the console would show a dead link")
	}
}

// A parent node exists to group children and must not be mounted itself. If it
// carried an entry the console would try to load a micro-frontend for a menu
// that is only there to hold other menus.
func TestMenuParentNodesAreNotMountable(t *testing.T) {
	url, mgr := menuStack(t)
	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	apps := fetchApps(t, url)

	parent, found := findMenu(apps.Menu, "/echo/tools")
	if !found {
		t.Fatal("the parent menu node is missing")
	}
	if len(parent.Children) == 0 {
		t.Fatal("the parent node has no children")
	}
	if parent.Entry != "" {
		t.Errorf("a node with children carries entry %q; the console would try to mount a grouping node", parent.Entry)
	}

	child, found := findMenu(apps.Menu, "/echo/tools/inspect")
	if !found {
		t.Fatal("a child menu node is missing")
	}
	if child.Entry == "" {
		t.Error("a leaf child has no entry, so it cannot be opened")
	}
}

// Role-restricted entries must be filtered on Core's side, not hidden by the
// browser. A menu the console never receives cannot be revealed by editing the
// page.
func TestMenuHidesRoleRestrictedEntries(t *testing.T) {
	url, mgr := menuStack(t)
	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	apps := fetchApps(t, url)

	// This stack has no auth, so the caller's role is empty and must not see
	// the admin-only entry.
	if node, found := findMenu(apps.Menu, "/echo/tools/admin"); found {
		t.Errorf("an admin-only menu was served to a caller with no role: %+v; "+
			"filtering it in the browser would only hide it, not withhold it", node)
	}

	// The unrestricted sibling is still there, so the filter is not simply
	// dropping the whole subtree.
	if _, found := findMenu(apps.Menu, "/echo/tools/inspect"); !found {
		t.Error("role filtering removed an unrestricted entry as well")
	}
}

// Removing a plugin's package entirely — not just disabling it — must also
// take its menus away on the next scan.
func TestMenuGoesAwayWhenThePackageDoes(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, _ := newManagerOver(t, root, nil)
	mgr.Scan()

	h := &gateway.GatewayHandler{Plugins: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system/ui/apps", h.AppsHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx := context.Background()
	if err := mgr.Enable(ctx, "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if _, found := findMenu(fetchApps(t, srv.URL).Menu, "/echo"); !found {
		t.Fatal("the menu never appeared")
	}

	// The operator deletes the package and Core rescans.
	if err := mgr.Disable(ctx, "echo"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "echo")); err != nil {
		t.Fatalf("removing the package: %v", err)
	}
	mgr.Scan()

	apps := fetchApps(t, srv.URL)
	if len(apps.Apps) != 0 {
		t.Errorf("%d app(s) listed after the package was removed", len(apps.Apps))
	}
	if _, found := findMenu(apps.Menu, "/echo"); found {
		t.Error("the removed plugin's menu is still being served")
	}
}

// roleResolver is a session store with exactly one user, whose role the test
// chooses. Enough to drive the role filter from the outside.
type roleResolver struct{ role string }

func (r roleResolver) Resolve(token string) (auth.User, bool) {
	if token != "test-session" {
		return auth.User{}, false
	}
	return auth.User{ID: 1, Username: "tester", Role: r.role}, true
}

func fetchAppsAs(t *testing.T, url, token string) appsPayload {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url+"/api/system/ui/apps", nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET apps: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("apps returned %d", resp.StatusCode)
	}

	var out appsPayload
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return out
}

// The other direction. Without this, a filter that dropped every
// role-restricted entry regardless of who asked would pass the test above and
// nobody would ever see the admin menu.
func TestMenuShowsRoleRestrictedEntriesToThatRole(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, _ := newManagerOver(t, root, nil)
	mgr.Scan()
	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	serveAs := func(role string) string {
		h := &gateway.GatewayHandler{Plugins: mgr, Auth: roleResolver{role: role}}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/system/ui/apps", h.AppsHandler)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv.URL
	}

	adminMenu := fetchAppsAs(t, serveAs("admin"), "test-session")
	if _, found := findMenu(adminMenu.Menu, "/echo/tools/admin"); !found {
		t.Error("an admin does not see the admin-only menu; the filter is dropping restricted entries for everyone")
	}

	userMenu := fetchAppsAs(t, serveAs("user"), "test-session")
	if _, found := findMenu(userMenu.Menu, "/echo/tools/admin"); found {
		t.Error("a non-admin sees the admin-only menu")
	}
	if _, found := findMenu(userMenu.Menu, "/echo/tools/inspect"); !found {
		t.Error("a non-admin lost the unrestricted entry too")
	}
}

// An unauthenticated caller gets no menu at all when auth is on, rather than
// the anonymous subset. The console is behind a login; there is no such thing
// as a logged-out view of it.
func TestMenuRequiresASessionWhenAuthIsOn(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, _ := newManagerOver(t, root, nil)
	mgr.Scan()
	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	h := &gateway.GatewayHandler{Plugins: mgr, Auth: roleResolver{role: "admin"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system/ui/apps", h.AppsHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/system/ui/apps")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d for a request with no session, want 401", resp.StatusCode)
	}
}

// A plugin that declares menus but ships no frontend gets no menu entry.
//
// This is the state every shipped example was in: eight backend-only plugins,
// six of them declaring menus. Each got an entry pointing at /plugins/<key>/,
// the asset handler answered 404, and qiankun mounted that response body — so
// the page read "404 page not found" under a working sidebar.
//
// The unit test in core/gateway covers the handler's decision. This one covers
// the path that decides the input to it: Core reads FrontendDir off the
// package directory, so whether the entry appears depends on a directory
// existing on disk, which only an end-to-end test exercises.
func TestBackendOnlyPluginGetsNoMenuEntry(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	// Same package, minus the UI: backend-only, exactly like the examples.
	if err := os.RemoveAll(filepath.Join(root, "echo", "frontend")); err != nil {
		t.Fatalf("removing the fixture frontend: %v", err)
	}

	mgr, _ := newManagerOver(t, root, nil)
	mgr.Scan()

	h := &gateway.GatewayHandler{Plugins: mgr}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/system/ui/apps", h.AppsHandler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	got := fetchApps(t, srv.URL)
	if _, found := findMenu(got.Menu, "/echo"); found {
		t.Error("a backend-only plugin was given a menu entry, which resolves to a 404 page")
	}
	if len(got.Apps) != 0 {
		t.Errorf("%d app(s) listed for a plugin with no UI to mount", len(got.Apps))
	}
}
