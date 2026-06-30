package sdk

import (
	"bytes"
	"context"
	"net/http"
)

// UserContext carries the authenticated identity injected by Core.
type UserContext struct {
	UserID      string
	Roles       []string
	Permissions []string
}

type contextKey string

const userContextKey contextKey = "user_info"

// GetUser extracts the authenticated user from a request context.
func GetUser(ctx context.Context) *UserContext {
	if val := ctx.Value(userContextKey); val != nil {
		if u, ok := val.(*UserContext); ok {
			return u
		}
	}
	return nil
}

// Config controls how the extension connects to Core.
type Config struct {
	ExtensionKey string
	CoreGrpcURL  string
	IsDev        bool
	DevFEUrl     string
	Version      string
	// ExtensionSecret authenticates an already-approved extension. It is usually
	// loaded from manifest.yaml (persisted there after the first approval), but
	// may be set explicitly (e.g. from an EXTENSION_SECRET env var) to pin a
	// pre-generated secret for an additional replica. Empty on a first-time
	// registration, which Core parks as pending for admin approval.
	ExtensionSecret string
	// ManifestPath, when set, makes the SDK load manifest.yaml and send the
	// declared collections/indexes/slots to Core on registration so Core can
	// provision tables and register UI slots automatically.
	ManifestPath string
	// FrontendDir, when set in production mode (IsDev=false), points at the
	// built micro-frontend directory (e.g. dist). The SDK zips it in memory and
	// streams it to Core during registration so Core serves the assets from its
	// own gateway. Ignored when IsDev is true (Core then proxies DevFEUrl).
	FrontendDir string
}

// mockResponseWriter captures handler output so it can be marshalled into a
// HttpResponseChunk and pushed back over the tunnel.
type mockResponseWriter struct {
	headers     http.Header
	statusCode  int
	body        *bytes.Buffer
	wroteHeader bool
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{
		headers:    make(http.Header),
		statusCode: http.StatusOK,
		body:       new(bytes.Buffer),
	}
}

func (w *mockResponseWriter) Header() http.Header { return w.headers }

func (w *mockResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(b)
}

func (w *mockResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.statusCode = statusCode
	w.wroteHeader = true
}
