package gateway

import (
	"net/http"
	"strings"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/pluginhost"
)

// GatewayHandler is Core's HTTP entry point.
//
// It serves the system API and the console. Plugin traffic never reaches it:
// PluginHandler wraps this handler as middleware and intercepts
// /api/plugins/* before delegating the rest here.
type GatewayHandler struct {
	// systemRoutes is a linear match list registered at startup by whichever
	// features are enabled. Linear is fine: there are a couple of dozen of
	// them, each checked against a string prefix once per request.
	systemRoutes []systemRoute

	// Auth, when set, resolves the session token. Nil disables authentication,
	// which is how Core runs without a database.
	Auth UserResolver

	// Host serves the console SPA for any non-API path.
	Host http.Handler

	// Plugins contributes enabled plugins to the app and menu listing.
	Plugins PluginSource
}

// PluginSource is the slice of pluginhost.Manager the gateway reads for menus
// and micro-frontend assets.
type PluginSource interface {
	List() []pluginhost.Status
	Package(key string) (*pluginhost.Package, bool)
}

// UserResolver maps a session token to an authenticated user.
type UserResolver interface {
	Resolve(token string) (auth.User, bool)
}

type systemRoute struct {
	match   func(path string) bool
	handler http.HandlerFunc
}

func NewGatewayHandler() *GatewayHandler {
	return &GatewayHandler{}
}

// RegisterSystemRoute lets startup wire in a feature's routes without this
// package importing it.
func (h *GatewayHandler) RegisterSystemRoute(match func(path string) bool, handler http.HandlerFunc) {
	h.systemRoutes = append(h.systemRoutes, systemRoute{match: match, handler: handler})
}

func (h *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	for _, route := range h.systemRoutes {
		if route.match(r.URL.Path) {
			route.handler(w, r)
			return
		}
	}

	// An unmatched API path must 404 rather than fall through to the SPA,
	// which would answer with index.html and turn a typo into a confusing
	// "why is my API returning HTML".
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	if h.Host != nil {
		h.Host.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}
