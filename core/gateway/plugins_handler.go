package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
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
	SetConfig(ctx context.Context, key string, cfg map[string]string) error
}

// ConfigStore is where an operator's settings are written down.
//
// Separate from the manager because the two have different lifetimes: the
// manager pushes a value to processes that are running now, the store is what
// survives a restart. Core needs both, and either without the other is the
// bug this interface exists to prevent.
type ConfigStore interface {
	Get(ctx context.Context, pluginKey string) (map[string]string, error)
	Set(ctx context.Context, pluginKey string, values map[string]string) error
}

// DeadLetterStore is the queue's failure sink, for the two things an operator
// needs from it: seeing what was given up on, and putting one back.
//
// Optional for the same reason as ConfigStore — Core runs without a database —
// and the endpoint says so rather than reporting an empty list, which would
// read as "nothing has failed".
type DeadLetterStore interface {
	Dead(ctx context.Context, pluginKey string, limit int) ([]db.DeadMessage, error)
	RetryDead(ctx context.Context, pluginKey string, id int64) error
}

// SecretPlaceholder is what the console sees in place of a secret's value.
//
// Submitting it back is understood as "leave this one alone". Without that,
// an operator editing any other field would save the mask over the real
// secret — and would not find out until the plugin next tried to use it.
const SecretPlaceholder = "••••••••"

// PluginsHandler serves the admin plugin-management API.
//
// Every route requires an admin session, matching the extension API it
// replaces: enabling a plugin runs third-party code inside Core's trust
// boundary, so it is not something a regular user may do.
type PluginsHandler struct {
	Auth    UserResolver
	Manager PluginManager

	// Config is optional: Core runs without a database, and then settings
	// cannot be stored. The endpoint says so rather than accepting a value
	// and losing it at the next restart.
	Config ConfigStore

	// DeadLetters is optional for the same reason.
	DeadLetters DeadLetterStore
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

	// Configuration is the one resource here that is read as well as written,
	// so it is dispatched before the action-only routes below.
	if len(segments) == 2 && segments[1] == "config" {
		h.serveConfig(w, r, key)
		return
	}

	// The dead-letter queue: list, and retry one by id.
	if len(segments) >= 2 && segments[1] == "dead" {
		h.serveDeadLetters(w, r, key, segments[2:])
		return
	}

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

// serveConfig reads and writes a plugin's admin-managed settings.
//
// Reading returns the manifest's declarations alongside the stored values, so
// the console renders the form the plugin author described rather than a
// free-text key/value editor where a typo is silently accepted by both sides.
func (h *PluginsHandler) serveConfig(w http.ResponseWriter, r *http.Request, key string) {
	decls, ok := h.declarations(key)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such plugin: " + key})
		return
	}
	if h.Config == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "plugin configuration needs a database; Core was started without DATABASE_URL"})
		return
	}

	stored, err := h.Config.Get(r.Context(), key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"key":      key,
			"declared": decls,
			"values":   maskSecrets(decls, stored),
		})

	case http.MethodPost, http.MethodPut:
		var body struct {
			Values map[string]string `json:"values"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}

		values, err := resolveConfig(decls, stored, body.Values)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}

		// Stored first. A push that fails leaves a plugin running the old
		// value, which its next launch corrects; a push that succeeded
		// without being stored would silently revert at the next restart,
		// and nobody would connect the two events.
		if err := h.Config.Set(r.Context(), key, values); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		pushErr := h.Manager.SetConfig(r.Context(), key, values)

		resp := map[string]any{"key": key, "values": maskSecrets(decls, values)}
		if pushErr != nil {
			// Saved but not delivered: reported as a warning rather than an
			// error, because the two have different remedies and retrying the
			// save is not one of them.
			resp["warning"] = "saved, but not delivered to the running plugin: " + pushErr.Error()
		}
		writeJSON(w, http.StatusOK, resp)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// declarations finds what a plugin says it can be configured with.
func (h *PluginsHandler) declarations(key string) ([]manifest.ConfigDecl, bool) {
	for _, s := range h.Manager.List() {
		if s.Key == key {
			return s.Config, true
		}
	}
	return nil, false
}

// resolveConfig turns what the console submitted into what will be stored.
//
// Undeclared keys are refused. A manifest declaring its settings is the whole
// reason the console can render a form, and accepting anything else would let
// a typo sit in the database looking like a configured value while the plugin
// goes on using the default.
func resolveConfig(decls []manifest.ConfigDecl, stored, submitted map[string]string) (map[string]string, error) {
	byKey := make(map[string]manifest.ConfigDecl, len(decls))
	for _, d := range decls {
		byKey[d.Key] = d
	}

	out := make(map[string]string, len(submitted))
	for k, v := range submitted {
		d, ok := byKey[k]
		if !ok {
			return nil, fmt.Errorf("%q is not a setting this plugin declares", k)
		}
		if d.Secret && v == SecretPlaceholder {
			// The console never received the real value, so it cannot be
			// submitting one. Keep what is stored.
			if old, ok := stored[k]; ok {
				out[k] = old
			}
			continue
		}
		out[k] = v
	}
	return out, nil
}

// maskSecrets replaces the values of settings declared secret.
//
// Only ones that actually have a value: masking an unset secret would show an
// operator a filled field over an empty setting.
func maskSecrets(decls []manifest.ConfigDecl, values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for k, v := range values {
		out[k] = v
	}
	for _, d := range decls {
		if d.Secret && out[d.Key] != "" {
			out[d.Key] = SecretPlaceholder
		}
	}
	return out
}

// serveDeadLetters lists a plugin's dead letters, and retries one by id.
//
// Until this existed the console showed a count and nothing else, which is not
// something an operator can act on: it says four things were lost without
// saying which, why, or whether they mattered. Finding out meant reading the
// queue table directly.
func (h *PluginsHandler) serveDeadLetters(w http.ResponseWriter, r *http.Request, key string, rest []string) {
	if h.DeadLetters == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the queue needs a database; Core is running without one",
		})
		return
	}

	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		messages, err := h.DeadLetters.Dead(r.Context(), key, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		// An explicit empty list rather than null, so a console rendering the
		// response does not have to tell the two apart.
		if messages == nil {
			messages = []db.DeadMessage{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": messages})

	case len(rest) == 2 && rest[1] == "retry" && r.Method == http.MethodPost:
		id, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message id must be a number"})
			return
		}
		if err := h.DeadLetters.RetryDead(r.Context(), key, id); err != nil {
			// Not found rather than a server error: the id is the caller's, and
			// the usual reason is that somebody already retried it.
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"retried": id})

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}
