package gateway

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/taills/moduless/core/auth"
)

// UsersHandler serves the admin user-management API under /api/system/users.
// All routes require an admin session except a user changing their own password.
type UsersHandler struct {
	Store *auth.Store
}

func NewUsersHandler(store *auth.Store) *UsersHandler {
	return &UsersHandler{Store: store}
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

type updateUserRequest struct {
	Role     *string `json:"role"`
	Password *string `json:"password"`
}

type changePasswordRequest struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

// Serve dispatches user-management requests.
func (h *UsersHandler) Serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Any authenticated user may change their own password.
	if path == "/api/system/users/me/password" {
		h.changeOwnPassword(w, r)
		return
	}

	caller, ok := h.Store.Resolve(SessionToken(r))
	if !ok || caller.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin privileges required"})
		return
	}

	switch {
	case path == "/api/system/users" && r.Method == http.MethodGet:
		h.list(w, r)
	case path == "/api/system/users" && r.Method == http.MethodPost:
		h.create(w, r)
	case strings.HasPrefix(path, "/api/system/users/") && r.Method == http.MethodPut:
		h.update(w, r, caller)
	case strings.HasPrefix(path, "/api/system/users/") && r.Method == http.MethodDelete:
		h.remove(w, r, caller)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
}

func (h *UsersHandler) list(w http.ResponseWriter, r *http.Request) {
	users, err := h.Store.ListUsers(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, users)
}

func (h *UsersHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := h.Store.CreateUser(r.Context(), req.Username, req.Password, req.Role); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "created"})
}

func (h *UsersHandler) update(w http.ResponseWriter, r *http.Request, caller auth.User) {
	id, ok := userIDFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid user id"})
		return
	}
	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Role != nil {
		// Guard against an admin demoting themselves into a lockout.
		if id == caller.ID && *req.Role != "admin" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot change your own role"})
			return
		}
		if err := h.Store.SetRole(r.Context(), id, *req.Role); err != nil {
			writeJSON(w, statusForUserErr(err), map[string]string{"error": err.Error()})
			return
		}
	}
	if req.Password != nil {
		if err := h.Store.SetPassword(r.Context(), id, *req.Password); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func (h *UsersHandler) remove(w http.ResponseWriter, r *http.Request, caller auth.User) {
	id, ok := userIDFromPath(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid user id"})
		return
	}
	if id == caller.ID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cannot delete your own account"})
		return
	}
	if err := h.Store.DeleteUser(r.Context(), id); err != nil {
		writeJSON(w, statusForUserErr(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *UsersHandler) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	caller, ok := h.Store.Resolve(SessionToken(r))
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthenticated"})
		return
	}
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if err := h.Store.ChangePassword(r.Context(), caller.ID, req.OldPassword, req.NewPassword); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

func userIDFromPath(path string) (int32, bool) {
	rest := strings.TrimPrefix(path, "/api/system/users/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return int32(n), true
}

func statusForUserErr(err error) int {
	switch err {
	case auth.ErrLastAdmin:
		return http.StatusConflict
	case auth.ErrUserMissing:
		return http.StatusNotFound
	default:
		return http.StatusBadRequest
	}
}
