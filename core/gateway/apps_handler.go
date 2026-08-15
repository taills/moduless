package gateway

import (
	"net/http"

	"github.com/taills/moduless/manifest"
)

// AppInfo is one entry in the console's app list.
type AppInfo struct {
	Key         string     `json:"key"`
	DisplayName string     `json:"display_name"`
	MenuIcon    string     `json:"menu_icon"`
	MenuPath    string     `json:"menu_path"`
	Entry       string     `json:"entry"`
	Online      bool       `json:"online"`
	Replicas    int        `json:"replicas"`
	Menus       []MenuNode `json:"menus"`
}

// AppsResponse is what the console consumes to build its UI.
//
// Menu is already merged across every plugin, role-filtered and sorted. The
// console used to do that merge itself, which meant the same algorithm existed
// in two languages and could drift.
type AppsResponse struct {
	Apps []AppInfo  `json:"apps"`
	Menu []MenuNode `json:"menu"`
}

// AppsHandler serves GET /api/system/ui/apps: the enabled plugins and the
// merged menu tree. Requires a valid session when auth is on.
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

	apps := h.appsFromPlugins(userRole)

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
// broken link.
func (h *GatewayHandler) appsFromPlugins(userRole string) []AppInfo {
	if h.Plugins == nil {
		return []AppInfo{}
	}

	apps := make([]AppInfo, 0, 4)
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
			// A plugin shipping a UI but declaring no menu still needs
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
		// own micro-frontend; a node with children is organisational and must
		// stay entry-less so the console does not try to mount it.
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
