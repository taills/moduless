package gateway

import (
	"context"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
)

// GatewayHandler is the public HTTP reverse proxy in Core. It serves
// micro-frontend static assets from the in-memory cache and tunnels API
// requests to extensions over gRPC.
type GatewayHandler struct {
	Manager *tunnel.TunnelManager

	// systemRoutes holds optional extra handlers (files, slots, diagnostics)
	// registered by later phases. Checked before extension routing.
	systemRoutes []systemRoute
}

type systemRoute struct {
	match   func(path string) bool
	handler http.HandlerFunc
}

func NewGatewayHandler(mgr *tunnel.TunnelManager) *GatewayHandler {
	return &GatewayHandler{Manager: mgr}
}

// RegisterSystemRoute lets later-phase features (file service, UI slots,
// diagnostics) hook into the gateway without the core router importing them.
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

	// 1. Static asset routing (/extensions/<key>/...).
	if strings.HasPrefix(r.URL.Path, "/extensions/") {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/extensions/"), "/", 2)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		extKey := parts[0]
		filePath := "/" + parts[1]

		content, ok := h.Manager.GetUiFile(extKey, filePath)
		if !ok {
			http.NotFound(w, r)
			return
		}

		contentType := mime.TypeByExtension(filepath.Ext(filePath))
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Write(content)
		return
	}

	// 2. API proxy routing (/api/extensions/<key>/...).
	if strings.HasPrefix(r.URL.Path, "/api/extensions/") {
		h.proxyToExtension(w, r)
		return
	}

	http.NotFound(w, r)
}

func (h *GatewayHandler) proxyToExtension(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/extensions/"), "/", 2)
	if len(parts) < 2 {
		http.NotFound(w, r)
		return
	}
	extKey := parts[0]
	subPath := "/" + parts[1]

	activeTunnel, ok := h.Manager.GetTunnel(extKey)
	if !ok {
		http.Error(w, "extension offline", http.StatusBadGateway)
		return
	}

	streamID := time.Now().Format("20060102150405.000000000")
	respChan := make(chan *pb.HttpResponseChunk, 20)
	activeTunnel.ResponseChans.Store(streamID, respChan)
	defer activeTunnel.ResponseChans.Delete(streamID)

	body, _ := io.ReadAll(r.Body)
	headers := make(map[string]string)
	for k, v := range r.Header {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	// Inject mock auth context (replaced by the Auth/RBAC interceptor in prod).
	if headers["X-User-Id"] == "" {
		headers["X-User-Id"] = "10001"
		headers["X-User-Roles"] = "admin"
	}

	err := activeTunnel.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_HttpReqChunk{
			HttpReqChunk: &pb.HttpRequestChunk{
				StreamId:  streamID,
				IsFirst:   true,
				IsLast:    true,
				Method:    r.Method,
				Path:      subPath,
				Query:     r.URL.RawQuery,
				Headers:   headers,
				BodyChunk: body,
			},
		},
	})
	if err != nil {
		http.Error(w, "tunnel write error", http.StatusInternalServerError)
		return
	}

	// Await response首包 and subsequent chunks.
	var writerInitialized bool
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	for {
		select {
		case chunk := <-respChan:
			if chunk.IsFirst && !writerInitialized {
				for k, v := range chunk.Headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(int(chunk.StatusCode))
				writerInitialized = true
			}
			if !writerInitialized {
				http.Error(w, "invalid protocol order", http.StatusInternalServerError)
				return
			}
			w.Write(chunk.BodyChunk)
			if chunk.IsLast {
				return
			}
		case <-ctx.Done():
			if !writerInitialized {
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			}
			return
		}
	}
}
