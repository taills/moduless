package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/tunnel"
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

// AppsHandler serves GET /api/system/ui/apps: registered extensions with their
// menu metadata and micro-frontend entry, for the host app to build its menu
// and register qiankun micro-apps. Requires a valid session when auth is on.
//
// When Store is set, menus are read from the DB-backed JSONB column so the
// host can render the menu tree even after a Core restart (the in-memory
// tunnel manager only knows about currently-connected replicas). When Store
// is nil the handler falls back to TunnelManager-only metadata for backwards
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
	writeJSON(w, http.StatusOK, apps)
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
