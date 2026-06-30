package gateway

import (
	"net/http"
)

// AppInfo is one entry in the host app menu / qiankun registration list.
type AppInfo struct {
	Key         string `json:"key"`
	DisplayName string `json:"display_name"`
	MenuIcon    string `json:"menu_icon"`
	MenuPath    string `json:"menu_path"`
	Entry       string `json:"entry"`
	Online      bool   `json:"online"`
}

// AppsHandler serves GET /api/system/ui/apps: registered extensions with their
// menu metadata and micro-frontend entry, for the host app to build its menu
// and register qiankun micro-apps. Requires a valid session when auth is on.
func (h *GatewayHandler) AppsHandler(w http.ResponseWriter, r *http.Request) {
	if h.Auth != nil {
		if _, ok := h.Auth.Resolve(SessionToken(r)); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
			return
		}
	}

	exts := h.Manager.ListExtensions()
	apps := make([]AppInfo, 0, len(exts))
	for _, e := range exts {
		name := e.DisplayName
		if name == "" {
			name = e.Key
		}
		menuPath := e.MenuPath
		if menuPath == "" {
			menuPath = "/" + e.Key
		}
		apps = append(apps, AppInfo{
			Key:         e.Key,
			DisplayName: name,
			MenuIcon:    e.MenuIcon,
			MenuPath:    menuPath,
			Entry:       "/extensions/" + e.Key + "/",
			Online:      e.Online,
		})
	}
	writeJSON(w, http.StatusOK, apps)
}
