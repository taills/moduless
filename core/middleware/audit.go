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

// AuditOptions is what the middleware needs from the rest of Core.
//
// Both fields are supplied by the caller rather than known here, and that is
// deliberate. This middleware previously carried its own copy of the API
// prefix as a string literal; when the route was renamed from /api/extensions/
// to /api/plugins/, the literal stayed behind and auditing silently stopped
// happening — every request still went through the middleware, and none of
// them matched. Its test kept passing because it used the dead prefix too.
type AuditOptions struct {
	// Prefix is the API root whose writes are audited. It must come from
	// whoever owns the route, so there is one definition rather than two that
	// can drift.
	Prefix string

	// Identify resolves the authenticated caller from a request.
	//
	// It exists because the caller's identity has to be established the same
	// way the rest of Core establishes it — against the session store. This
	// middleware used to read an X-User-Id header, which nothing set and any
	// client could send, so the user recorded against an audited action was
	// whoever the client claimed to be. An audit log that records an
	// attacker's chosen name is worse than none: it is evidence that is wrong.
	//
	// Nil means nothing is authenticated and every entry is anonymous.
	Identify func(r *http.Request) string
}

// AuditLogger records state-modifying operations against the plugin API.
//
// Panics on an empty Prefix rather than auditing everything or nothing: this
// is called once at start-up, and a misconfiguration that shows up as a
// permanently empty audit table is the failure this middleware has already had
// once.
func AuditLogger(recorder AuditRecorder, opts AuditOptions) func(http.Handler) http.Handler {
	if opts.Prefix == "" {
		panic("middleware: AuditLogger needs the API prefix it should audit")
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)

			if !strings.HasPrefix(r.URL.Path, opts.Prefix) {
				return
			}
			parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, opts.Prefix), "/", 2)
			if len(parts) < 2 {
				return
			}
			extKey := parts[0]

			switch r.Method {
			case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
			default:
				return
			}

			userID := "anonymous"
			if opts.Identify != nil {
				if id := opts.Identify(r); id != "" {
					userID = id
				}
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

// clientIP reports where the request came from.
//
// X-Forwarded-For is honoured, which means it is only as trustworthy as the
// proxy in front of Core: a client talking to Core directly can put anything
// there. Deployments that audit for accountability must terminate at a proxy
// that overwrites the header rather than appending to it.
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
