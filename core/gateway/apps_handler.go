package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/tunnel"
	"github.com/taills/moduless/manifest"
)

// AppInfo is one entry in the host app menu / qiankun registration list.
type AppInfo struct {
	Key         string     `json:"key"`
	DisplayName string     `json:"display_name"`
	MenuIcon    string     `json:"menu_icon"`
	MenuPath    string     `json:"menu_path"`
	Entry       string     `json:"entry"`
	Online      bool       `json:"online"`
	Replicas    int        `json:"replicas"`
	Menus       []MenuNode `json:"menus"` // extension's declared menu tree
}

// AppsResponse is what the console consumes to build its UI.
//
// Menu is already merged across every extension and plugin, already
// role-filtered and already sorted. The console used to do that merge itself,
// which meant the same algorithm existed twice — once here (unused) and once
// in JavaScript — and the two could drift. Core is the single source now.
type AppsResponse struct {
	Apps []AppInfo  `json:"apps"`
	Menu []MenuNode `json:"menu"`
}

// AppsHandler serves GET /api/system/ui/apps: registered extensions and
// enabled plugins with their menu metadata and micro-frontend entry, plus the
// merged menu tree. Requires a valid session when auth is on.
//
// When Store is set, extension menus are read from the DB-backed JSONB column
// so the host can render them even after a Core restart (the in-memory tunnel
// manager only knows about currently-connected replicas). When Store is nil the
// handler falls back to TunnelManager-only metadata for backwards
// compatibility (this branch is exercised in tests).
func (h *GatewayHandler) AppsHandler(w http.ResponseWriter, r *http.Request) {
	userRole := ""
	if h.Auth != nil {
		user, ok := h.Auth.Resolve(SessionToken(r))
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
		userRole = user.Role
	}

	var apps []AppInfo
	if h.Store != nil {
		apps = h.appsFromStore(r.Context(), userRole)
	} else {
		apps = h.appsFromManager(userRole)
	}
	apps = append(apps, h.appsFromPlugins(userRole)...)

	menus := make([][]MenuNode, 0, len(apps))
	for _, a := range apps {
		menus = append(menus, a.Menus)
	}

	writeJSON(w, http.StatusOK, AppsResponse{
		Apps: apps,
		Menu: buildTree(menus, userRole),
	})
}

// appsFromPlugins lists plugins that are enabled and actually serving.
//
// A plugin with no ready replica is omitted rather than shown as offline: its
// micro-frontend would fail to load, so a menu entry pointing at it would be a
// broken link. Extensions behave differently because they dial in on their own
// schedule and their absence is expected to be temporary.
func (h *GatewayHandler) appsFromPlugins(userRole string) []AppInfo {
	if h.Plugins == nil {
		return nil
	}

	var apps []AppInfo
	for _, st := range h.Plugins.List() {
		if !st.Enabled || st.Ready == 0 {
			continue
		}
		pkg, ok := h.Plugins.Package(st.Key)
		if !ok {
			continue
		}

		entry := PluginAssetPrefix + st.Key + "/"
		menus := menuNodesFrom(pkg.Manifest.Menus, entry)
		if len(menus) == 0 && pkg.FrontendDir != "" {
			// A plugin that ships a UI but declares no menu still needs
			// somewhere to be reachable from.
			menus = []MenuNode{{
				Path:  "/" + st.Key,
				Title: displayNameOr(st.DisplayName, st.Key),
				Entry: entry,
			}}
		}
		filtered := filterByRole(menus, userRole)

		app := AppInfo{
			Key:         st.Key,
			DisplayName: displayNameOr(st.DisplayName, st.Key),
			Entry:       entry,
			Online:      true,
			Replicas:    st.Ready,
			Menus:       filtered,
		}
		if len(filtered) > 0 {
			app.MenuPath = filtered[0].Path
			app.MenuIcon = filtered[0].Icon
		}
		apps = append(apps, app)
	}
	return apps
}

// menuNodesFrom converts a manifest's menu tree into the wire type, filling in
// the micro-frontend entry for leaf nodes that did not name one.
func menuNodesFrom(items []manifest.MenuItem, defaultEntry string) []MenuNode {
	if len(items) == 0 {
		return nil
	}
	out := make([]MenuNode, 0, len(items))
	for _, it := range items {
		node := MenuNode{
			Path:     it.Path,
			Title:    it.Title,
			Icon:     it.Icon,
			Order:    it.Order,
			Entry:    it.Entry,
			Roles:    it.Roles,
			Children: menuNodesFrom(it.Children, defaultEntry),
		}
		// A leaf with no children and no explicit entry mounts the plugin's
		// own micro-frontend; a node with children is an organisational node
		// and must stay entry-less so the console does not try to mount it.
		if node.Entry == "" && len(node.Children) == 0 {
			node.Entry = defaultEntry
		}
		out = append(out, node)
	}
	return out
}

func displayNameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// appsFromStore reads every extension's menus from the DB-backed JSONB column
// and overlays live tunnel state (online/replicas). Menus are role-filtered
// per extension so an admin's tree can hide nodes an operator shouldn't see.
func (h *GatewayHandler) appsFromStore(ctx context.Context, userRole string) []AppInfo {
	rows, err := h.Store.ListExtensions(ctx)
	if err != nil {
		return []AppInfo{}
	}
	apps := make([]AppInfo, 0, len(rows))
	for _, e := range rows {
		apps = append(apps, appFromRow(e, h.Manager, userRole))
	}
	return apps
}

// appsFromManager is the pre-DB fallback: menu trees are taken from the
// in-memory metadata that the latest register request left on the manager.
// It only knows about currently-connected extensions (no Core-restart
// recovery), but it is fully self-contained and useful in unit tests.
func (h *GatewayHandler) appsFromManager(userRole string) []AppInfo {
	exts := h.Manager.ListExtensions()
	apps := make([]AppInfo, 0, len(exts))
	for _, e := range exts {
		entry := "/extensions/" + e.Key + "/"
		menuPath := e.MenuPath
		if menuPath == "" {
			menuPath = "/" + e.Key
		}
		menus := []MenuNode{{
			Path:  menuPath,
			Title: e.DisplayName,
			Icon:  e.MenuIcon,
			Order: 0,
			Entry: entry,
		}}
		apps = append(apps, AppInfo{
			Key:         e.Key,
			DisplayName: e.DisplayName,
			MenuIcon:    e.MenuIcon,
			MenuPath:    menuPath,
			Entry:       entry,
			Online:      e.Online,
			Replicas:    e.Replicas,
			Menus:       filterByRole(menus, userRole),
		})
	}
	return apps
}

// appFromRow builds an AppInfo from a sqlc.Extension row plus live tunnel
// state. The Menus JSONB is decoded; a nil/empty value falls back to a
// single-node tree from menu_icon / menu_path (preserving the legacy
// single-menu behavior).
func appFromRow(e sqlc.Extension, mgr *tunnel.TunnelManager, userRole string) AppInfo {
	menus := decodeMenusJSON(e.Menus, e.DisplayName, e.MenuIcon, e.MenuPath, "/extensions/"+e.Key+"/")
	filtered := filterByRole(menus, userRole)
	menuPath := e.MenuPath
	if menuPath == "" && len(filtered) > 0 {
		menuPath = filtered[0].Path
	}
	replicas := mgr.CountReplicas(e.Key)
	return AppInfo{
		Key:         e.Key,
		DisplayName: e.DisplayName,
		MenuIcon:    e.MenuIcon,
		MenuPath:    menuPath,
		Entry:       "/extensions/" + e.Key + "/",
		Online:      replicas > 0,
		Replicas:    replicas,
		Menus:       filtered,
	}
}

// decodeMenusJSON parses the extensions.menus JSONB blob into a MenuNode tree.
// On any decode error (missing column for legacy rows, malformed JSON) it
// falls back to a one-node tree built from the legacy icon/path fields.
func decodeMenusJSON(raw []byte, displayName, icon, path, defaultEntry string) []MenuNode {
	if len(raw) > 0 {
		var nodes []MenuNode
		if err := json.Unmarshal(raw, &nodes); err == nil && len(nodes) > 0 {
			return nodes
		}
	}
	if path == "" && icon == "" {
		return nil
	}
	return []MenuNode{{
		Path:  path,
		Title: displayName,
		Icon:  icon,
		Order: 0,
		Entry: defaultEntry,
	}}
}
