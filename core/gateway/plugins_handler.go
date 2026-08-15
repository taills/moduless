package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/taills/moduless/core/pluginhost"
)

// PluginsAPIPrefix is the admin plugin-management API root.
const PluginsAPIPrefix = "/api/system/plugins"

// PluginManager is the slice of pluginhost.Manager this handler needs.
type PluginManager interface {
	Scan()
	List() []pluginhost.Status
	Enable(ctx context.Context, key string) error
	Disable(ctx context.Context, key string) error
	Upgrade(ctx context.Context, key string) error
}

// PluginsHandler serves the admin plugin-management API.
//
// Every route requires an admin session, matching the extension API it
// replaces: enabling a plugin runs third-party code inside Core's trust
// boundary, so it is not something a regular user may do.
type PluginsHandler struct {
	Auth    UserResolver
	Manager PluginManager
}

func NewPluginsHandler(resolver UserResolver, mgr PluginManager) *PluginsHandler {
	return &PluginsHandler{Auth: resolver, Manager: mgr}
}

// Serve dispatches plugin-management requests.
func (h *PluginsHandler) Serve(w http.ResponseWriter, r *http.Request) {
	if h.Auth != nil {
		caller, ok := h.Auth.Resolve(SessionToken(r))
		if !ok || caller.Role != "admin" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin privileges required"})
			return
		}
	}

	path := r.URL.Path
	if path == PluginsAPIPrefix && r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"plugins": h.Manager.List()})
		return
	}

	rest := strings.TrimPrefix(path, PluginsAPIPrefix+"/")
	if rest == "" || rest == path {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	segments := strings.Split(rest, "/")

	// Rescan picks up packages added to the plugin directory since startup.
	if len(segments) == 1 && segments[0] == "rescan" && r.Method == http.MethodPost {
		h.Manager.Scan()
		writeJSON(w, http.StatusOK, map[string]any{"plugins": h.Manager.List()})
		return
	}

	key := segments[0]
	if len(segments) != 2 || r.Method != http.MethodPost {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	// These calls launch, drain or replace OS processes and can take seconds.
	// They deliberately use the request context, so an admin who navigates
	// away cancels the operation rather than leaving it running unobserved.
	ctx := r.Context()

	var err error
	switch segments[1] {
	case "enable":
		err = h.Manager.Enable(ctx, key)
	case "disable":
		err = h.Manager.Disable(ctx, key)
	case "upgrade":
		err = h.Manager.Upgrade(ctx, key)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		// A failed enable or upgrade has already rolled itself back: nothing
		// was published, so the previous state is intact. Report and move on.
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": h.Manager.List()})
}
