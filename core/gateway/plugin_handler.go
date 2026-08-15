package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// PluginAPIPrefix is where a plugin's own HTTP API is mounted.
const PluginAPIPrefix = "/api/plugins/"

// DefaultBackendTimeout bounds a call to a plugin's HandleHTTP.
const DefaultBackendTimeout = 30 * time.Second

// DefaultMaxRequestBody caps how much of a request body Core will buffer.
// Anything larger belongs in the file service, which streams to object storage
// instead of passing bytes through the plugin transport.
const DefaultMaxRequestBody = 8 << 20

// PluginHandler wires the filter pipeline and plugin routing into the gateway.
//
// It is installed as a middleware around the whole gateway rather than only on
// plugin routes, because filters are global by design: a rate limiter or WAF
// plugin must see every request, including ones served by Core itself or by
// another plugin.
type PluginHandler struct {
	Registry *pluginhost.Registry
	Runner   *pipeline.Runner

	// Auth resolves the session token into the identity handed to plugins.
	Auth UserResolver

	BackendTimeout time.Duration
	MaxBodyBytes   int64
}

func (h *PluginHandler) backendTimeout() time.Duration {
	if h.BackendTimeout > 0 {
		return h.BackendTimeout
	}
	return DefaultBackendTimeout
}

func (h *PluginHandler) maxBody() int64 {
	if h.MaxBodyBytes > 0 {
		return h.MaxBodyBytes
	}
	return DefaultMaxRequestBody
}

// Middleware wraps the gateway so every request passes through the pipeline.
func (h *PluginHandler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serve(w, r, next)
	})
}

func (h *PluginHandler) serve(w http.ResponseWriter, r *http.Request, next http.Handler) {
	// Load the snapshot exactly once and use it for the whole request. Calling
	// Current() again mid-request could mix two versions of the routing table
	// and filter chain, so a request might apply one plugin's filters while
	// routing to another's backend.
	snap := h.Registry.Current()
	chain := snap.Chain()

	traceID := traceIDFor(r)
	w.Header().Set("X-Request-Id", traceID)

	rc := pipeline.NewRequestContext(traceID, r, clientIP(r))
	isPluginRoute := strings.HasPrefix(r.URL.Path, PluginAPIPrefix)

	// Buffer the body only when something downstream needs it: a filter that
	// asked for it, or a plugin backend that will receive it.
	if chain.NeedsRequestBody() || isPluginRoute {
		body, err := h.readBody(w, r)
		if err != nil {
			return // readBody already wrote the response
		}
		rc.RequestBody = body
	}

	if out := h.Runner.Run(r.Context(), chain, snap, pb.Phase_PHASE_PRE_ROUTE, rc); out.Stopped() {
		h.writeResponse(w, out.ShortCircuit)
		h.logPhase(chain, snap, rc)
		return
	}

	if !isPluginRoute {
		// Not a plugin API call. Filters still ran, and still get their log
		// phase, but routing belongs to the rest of the gateway.
		rec := newRecorder(w, chain.HasPhase(pb.Phase_PHASE_POST_HANDLER))
		next.ServeHTTP(rec, requestWithContext(r, rc))
		rc.ResponseStatus = rec.status
		rc.ResponseHeader = rec.Header()
		rc.ResponseBody = rec.body()
		h.logPhase(chain, snap, rc)
		return
	}

	h.servePlugin(w, r, snap, chain, rc)
}

func (h *PluginHandler) servePlugin(w http.ResponseWriter, r *http.Request, snap *pluginhost.Snapshot, chain *pipeline.Chain, rc *pipeline.RequestContext) {
	// Identity is established before the authentication filters run, so a
	// filter sees whatever Core already resolved and can enrich or replace it.
	if h.Auth != nil {
		user, ok := h.Auth.Resolve(SessionToken(r))
		if !ok {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			h.logPhase(chain, snap, rc)
			return
		}
		rc.Identity = &pb.Identity{
			UserId:   strconv.Itoa(int(user.ID)),
			Username: user.Username,
			Roles:    []string{user.Role},
		}
	}

	for _, phase := range []pb.Phase{
		pb.Phase_PHASE_AUTHENTICATE,
		pb.Phase_PHASE_AUTHORIZE,
		pb.Phase_PHASE_PRE_HANDLER,
	} {
		if out := h.Runner.Run(r.Context(), chain, snap, phase, rc); out.Stopped() {
			h.writeResponse(w, out.ShortCircuit)
			h.logPhase(chain, snap, rc)
			return
		}
	}

	// Route on the possibly-rewritten path, so a pre_handler filter can send a
	// request to a different plugin or sub-path.
	key, sub, ok := splitPluginPath(rc.Path)
	if !ok {
		http.NotFound(w, r)
		h.logPhase(chain, snap, rc)
		return
	}

	resp, err := h.callBackend(r.Context(), snap, key, sub, rc)
	if err != nil {
		rc.ResponseStatus = http.StatusBadGateway
		if out := h.Runner.Run(r.Context(), chain, snap, pb.Phase_PHASE_ON_ERROR, rc); out.Stopped() {
			h.writeResponse(w, out.ShortCircuit)
			h.logPhase(chain, snap, rc)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		h.logPhase(chain, snap, rc)
		return
	}

	rc.ResponseStatus = int(resp.GetStatusCode())
	rc.ResponseHeader = pipeline.FromProtoHeaders(resp.GetHeaders())
	rc.ResponseBody = resp.GetBody()

	if rc.ResponseStatus >= 500 {
		if out := h.Runner.Run(r.Context(), chain, snap, pb.Phase_PHASE_ON_ERROR, rc); out.Stopped() {
			h.writeResponse(w, out.ShortCircuit)
			h.logPhase(chain, snap, rc)
			return
		}
	}

	if out := h.Runner.Run(r.Context(), chain, snap, pb.Phase_PHASE_POST_HANDLER, rc); out.Stopped() {
		h.writeResponse(w, out.ShortCircuit)
		h.logPhase(chain, snap, rc)
		return
	}

	writeFinal(w, rc)
	h.logPhase(chain, snap, rc)
}

// callBackend dispatches to the plugin that owns the route.
//
// This replaces the reverse tunnel's hand-rolled stream multiplexing: with
// go-plugin each call is an independent unary RPC over an HTTP/2 connection,
// so concurrent requests need neither stream ids nor a send mutex.
func (h *PluginHandler) callBackend(ctx context.Context, snap *pluginhost.Snapshot, key, sub string, rc *pipeline.RequestContext) (*pb.HttpResponse, error) {
	inst, ok := snap.Pick(key)
	if !ok {
		return nil, &pluginUnavailableError{key: key}
	}
	release, ok := inst.BeginRequest()
	if !ok {
		return nil, &pluginUnavailableError{key: key}
	}
	defer release()

	callCtx, cancel := context.WithTimeout(ctx, h.backendTimeout())
	defer cancel()

	resp, err := inst.Client.HandleHTTP(callCtx, &pb.HttpRequest{
		TraceId:  rc.TraceID,
		Method:   rc.Method,
		Path:     sub,
		Query:    rc.Query,
		Headers:  pipeline.ToProtoHeaders(rc.Header),
		Body:     rc.RequestBody,
		Identity: rc.Identity,
		ClientIp: rc.ClientIP,
	})
	if err != nil {
		inst.RecordFailure()
		return nil, err
	}
	inst.RecordSuccess()
	return resp, nil
}

// logPhase dispatches the log phase without blocking. The request context is
// no longer mutated by this point, and a fresh context is used because the
// request's own is cancelled the moment the handler returns.
func (h *PluginHandler) logPhase(chain *pipeline.Chain, snap *pluginhost.Snapshot, rc *pipeline.RequestContext) {
	h.Runner.RunAsync(context.Background(), chain, snap, pb.Phase_PHASE_LOG, rc)
}

func (h *PluginHandler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	limited := http.MaxBytesReader(w, r.Body, h.maxBody())
	body, err := io.ReadAll(limited)
	if err != nil {
		// MaxBytesReader has already set the status when the limit is hit.
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return nil, err
	}
	return body, nil
}

func (h *PluginHandler) writeResponse(w http.ResponseWriter, resp *pb.HttpResponse) {
	for k, hv := range resp.GetHeaders() {
		for _, v := range hv.GetValues() {
			w.Header().Add(k, v)
		}
	}
	status := int(resp.GetStatusCode())
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(resp.GetBody())
}

func writeFinal(w http.ResponseWriter, rc *pipeline.RequestContext) {
	for k, vs := range rc.ResponseHeader {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	status := rc.ResponseStatus
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(rc.ResponseBody)
}

type pluginUnavailableError struct{ key string }

func (e *pluginUnavailableError) Error() string { return "plugin " + e.key + " is unavailable" }

// splitPluginPath turns /api/plugins/<key>/<sub> into its parts. A request for
// the bare prefix, or for a key with no sub-path, is not routable.
func splitPluginPath(path string) (key, sub string, ok bool) {
	rest := strings.TrimPrefix(path, PluginAPIPrefix)
	if rest == path || rest == "" {
		return "", "", false
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return rest, "/", true
	}
	return rest[:i], rest[i:], true
}

// traceIDFor establishes the id that follows this request through every
// process it touches.
//
// W3C traceparent is preferred so the id lines up with whatever distributed
// tracing sits in front of Core; X-Request-Id is the common fallback.
func traceIDFor(r *http.Request) string {
	if tp := r.Header.Get("traceparent"); tp != "" {
		// version-traceid-spanid-flags; the trace id is the second field.
		parts := strings.Split(tp, "-")
		if len(parts) >= 3 && len(parts[1]) == 32 && parts[1] != strings.Repeat("0", 32) {
			return parts[1]
		}
	}
	if rid := r.Header.Get("X-Request-Id"); rid != "" {
		return rid
	}
	return newTraceID()
}

func newTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func clientIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// requestWithContext threads the trace id into the downstream request so
// Core's own handlers can log it alongside the plugin ones.
func requestWithContext(r *http.Request, rc *pipeline.RequestContext) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), traceIDContextKey{}, rc.TraceID))
}

type traceIDContextKey struct{}

// TraceIDFromContext returns the trace id Core assigned to the request.
func TraceIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(traceIDContextKey{}).(string)
	return id
}

// recorder captures a downstream response so post_handler filters can inspect
// or rewrite it. Buffering is opt-in: when no filter subscribed to
// post_handler, writes pass straight through and nothing is copied.
type recorder struct {
	http.ResponseWriter
	status  int
	capture bool
	buf     bytes.Buffer
}

func newRecorder(w http.ResponseWriter, capture bool) *recorder {
	return &recorder{ResponseWriter: w, status: http.StatusOK, capture: capture}
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.capture {
		r.buf.Write(p)
	}
	return r.ResponseWriter.Write(p)
}

func (r *recorder) body() []byte {
	if !r.capture {
		return nil
	}
	return r.buf.Bytes()
}
