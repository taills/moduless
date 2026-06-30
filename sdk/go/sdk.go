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
