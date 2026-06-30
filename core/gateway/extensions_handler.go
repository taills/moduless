package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/taills/moduless/core/extension"
)

// ExtensionsHandler serves the admin extension-management API under
// /api/system/extensions, including the approval workflow and per-key secrets.
// Every route requires an admin session.
type ExtensionsHandler struct {
	Auth  UserResolver
	Coord *extension.Coordinator
}

func NewExtensionsHandler(resolver UserResolver, coord *extension.Coordinator) *ExtensionsHandler {
	return &ExtensionsHandler{Auth: resolver, Coord: coord}
}

type generateSecretRequest struct {
	Label string `json:"label"`
}

// Serve dispatches extension-management requests.
func (h *ExtensionsHandler) Serve(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.Auth.Resolve(SessionToken(r))
	if !ok || caller.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin privileges required"})
		return
	}

	path := r.URL.Path
	if path == "/api/system/extensions" && r.Method == http.MethodGet {
		h.list(w, r)
		return
	}

	rest := strings.TrimPrefix(path, "/api/system/extensions/")
	if rest == "" || rest == path {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	segments := strings.Split(rest, "/")
	key := segments[0]

	switch {
	case len(segments) == 2 && segments[1] == "approve" && r.Method == http.MethodPost:
		h.approve(w, r, key)
	case len(segments) == 2 && segments[1] == "reject" && r.Method == http.MethodPost:
		h.reject(w, r, key)
	case len(segments) == 1 && r.Method == http.MethodDelete:
		h.remove(w, r, key)
	case len(segments) == 2 && segments[1] == "secrets" && r.Method == http.MethodGet:
		h.listSecrets(w, r, key)
	case len(segments) == 2 && segments[1] == "secrets" && r.Method == http.MethodPost:
		h.generateSecret(w, r, key)
	case len(segments) == 3 && segments[1] == "secrets" && r.Method == http.MethodDelete:
		h.revokeSecret(w, r, key, segments[2])
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *ExtensionsHandler) list(w http.ResponseWriter, r *http.Request) {
	exts, err := h.Coord.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, exts)
}

func (h *ExtensionsHandler) approve(w http.ResponseWriter, r *http.Request, key string) {
	issued, err := h.Coord.Approve(r.Context(), key)
	if err != nil {
		writeJSON(w, statusForExtErr(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "approved", "issued_secrets": issued})
}

func (h *ExtensionsHandler) reject(w http.ResponseWriter, r *http.Request, key string) {
	if err := h.Coord.Reject(r.Context(), key); err != nil {
		writeJSON(w, statusForExtErr(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rejected"})
}

func (h *ExtensionsHandler) remove(w http.ResponseWriter, r *http.Request, key string) {
	if err := h.Coord.Delete(r.Context(), key); err != nil {
		writeJSON(w, statusForExtErr(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *ExtensionsHandler) listSecrets(w http.ResponseWriter, r *http.Request, key string) {
	secrets, err := h.Coord.ListSecrets(r.Context(), key)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, secrets)
}

func (h *ExtensionsHandler) generateSecret(w http.ResponseWriter, r *http.Request, key string) {
	var req generateSecretRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // label is optional
	secret, err := h.Coord.GenerateSecret(r.Context(), key, req.Label)
	if err != nil {
		writeJSON(w, statusForExtErr(err), map[string]string{"error": err.Error()})
		return
	}
	// The plaintext secret is returned exactly once.
	writeJSON(w, http.StatusCreated, map[string]string{"secret": secret})
}

func (h *ExtensionsHandler) revokeSecret(w http.ResponseWriter, r *http.Request, key, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid secret id"})
		return
	}
	if err := h.Coord.RevokeSecret(r.Context(), key, id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func statusForExtErr(err error) int {
	if errors.Is(err, extension.ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}
