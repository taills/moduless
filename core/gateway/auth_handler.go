package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/taills/moduless/core/auth"
)

// AuthHandler exposes login/logout/me over HTTP, backed by the session store.
type AuthHandler struct {
	Store *auth.Store
}

func NewAuthHandler(store *auth.Store) *AuthHandler {
	return &AuthHandler{Store: store}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Login handles POST /api/system/auth/login.
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	token, user, err := h.Store.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"token": token, "user": user})
}

// Me handles GET /api/system/auth/me.
func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	user, ok := h.Store.Resolve(SessionToken(r))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

// Logout handles POST /api/system/auth/logout.
func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	h.Store.Logout(SessionToken(r))
	w.WriteHeader(http.StatusNoContent)
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" header.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// SessionToken resolves the session token from the Authorization header, or the
// "moduless_token" cookie as a fallback. The cookie lets micro-frontend
// sub-apps authenticate their same-origin /api/extensions calls automatically,
// without threading the token into every request.
func SessionToken(r *http.Request) string {
	if t := BearerToken(r); t != "" {
		return t
	}
	if c, err := r.Cookie("moduless_token"); err == nil {
		return c.Value
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
