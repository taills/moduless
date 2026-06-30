package middleware

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"

	sqlc "github.com/taills/moduless/core/db/sqlc"
)

// AuditRecorder is the minimal persistence surface the middleware needs,
// making it trivial to unit-test without a real database.
type AuditRecorder interface {
	InsertAuditLog(ctx context.Context, arg sqlc.InsertAuditLogParams) error
}

// AuditLogger records state-modifying operations against extension APIs.
// In production the set of audited paths is driven by each extension's
// manifest `is_audit` declarations; by default every POST/PUT/DELETE/PATCH on
// an extension API is recorded so developers write zero audit code.
func AuditLogger(recorder AuditRecorder) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)

			if !strings.HasPrefix(r.URL.Path, "/api/extensions/") {
				return
			}
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/extensions/"), "/", 2)
			if len(parts) < 2 {
				return
			}
			extKey := parts[0]

			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			default:
				return
			}

			userID := r.Header.Get("X-User-Id")
			if userID == "" {
				userID = "anonymous"
			}

			// Persist asynchronously so auditing never blocks the response.
			params := sqlc.InsertAuditLogParams{
				UserID:       userID,
				Action:       r.Method + " operation on " + r.URL.Path,
				ExtensionKey: extKey,
				HttpPath:     r.URL.Path,
				ClientIp:     clientIP(r),
			}
			go func() {
				if err := recorder.InsertAuditLog(context.Background(), params); err != nil {
					log.Printf("[audit] failed to record log: %v", err)
				}
			}()
		})
	}
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
