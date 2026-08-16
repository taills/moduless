package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
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
	// Load the snapshot exactly once and use it for the whole request, so the
	// filter chain cannot change underneath it: a request must not apply one
	// version of the routing table in an early phase and another in a late one.
	//
	// Which process answers a given key is a separate question, and that one is
	// resolved live — h.Registry, not snap, is handed to the Runner. Freezing
	// that too is what used to make an upgrade mid-request fail: the instance
	// frozen here is killed as soon as its in-flight count hits zero, so a late
	// phase found nothing to call.
	snap := h.Registry.Current()
	chain := snap.Chain()

	traceID := traceIDFor(r)
	w.Header().Set("X-Request-Id", traceID)

	rc := pipeline.NewRequestContext(traceID, r, clientIP(r))
	// Capacity this request reserves on any plugin is held until the response
	// has been written, and freed here. A draining instance waits for exactly
	// this, and a request that was admitted is never refused halfway through.
	defer rc.ReleaseAdmissions()

	// The log phase runs on the way out, whichever way out that is.
	//
	// It used to be a call at each of the ten points this function can return
	// from, kept in step by hand — and one of them did not have it. The body is
	// buffered before the pipeline starts, so an over-limit body was answered
	// 413 and returned before any phase ran. That is the one rejection a caller
	// can produce deliberately, and it was the one that left no audit record.
	//
	// Deferring it makes the guarantee structural rather than clerical: a
	// future eleventh exit gets it without anyone remembering. Registered after
	// ReleaseAdmissions so it runs before it, which is the order the explicit
	// calls had. A chain with no log filters costs one length check here.
	defer h.logPhase(chain, rc)

	isPluginRoute := strings.HasPrefix(r.URL.Path, PluginAPIPrefix)

	// Buffer the body only when something downstream needs it: a filter that
	// asked for it, or a plugin backend that will receive it.
	buffered := chain.NeedsRequestBody() || isPluginRoute
	if buffered {
		body, err := h.readBody(w, r)
		if err != nil {
			// readBody already wrote the response. Record what it was, or the
			// log phase reports this request with no status at all.
			rc.ResponseStatus = http.StatusRequestEntityTooLarge
			return
		}
		rc.RequestBody = body
	}

	if out := h.Runner.Run(r.Context(), chain, h.Registry, pb.Phase_PHASE_PRE_ROUTE, rc); out.Stopped() {
		h.writeResponse(w, rc, out.ShortCircuit)
		return
	}

	// A pre_route filter may have rewritten the path into the plugin
	// namespace, which is the ordinary URL-rewriting case: a short public path
	// in front of a plugin's own, the thing an ISAPI filter is for.
	//
	// Whether this was a plugin route used to be decided once, from the
	// original URL, before the phase documented to run before routing. So the
	// rewrite updated rc.Path, the split below routed on it correctly, and the
	// request never got here — it had already been handed to the rest of the
	// gateway and answered 404. The rewrite worked inside the namespace and
	// could not reach into it, which is the direction anyone would try first.
	//
	// The body is the reason this cannot simply move: filters must see it, so
	// it is buffered before they run, and whether to buffer depended on the
	// same decision. It is only unbuffered here when nothing had wanted it, in
	// which case r.Body is still untouched and can be read now.
	if !isPluginRoute && strings.HasPrefix(rc.Path, PluginAPIPrefix) {
		if !buffered {
			body, err := h.readBody(w, r)
			if err != nil {
				rc.ResponseStatus = http.StatusRequestEntityTooLarge
				return
			}
			rc.RequestBody = body
		}
		isPluginRoute = true
	}

	if !isPluginRoute {
		// Not a plugin API call. Filters still ran, and still get their log
		// phase, but routing belongs to the rest of the gateway.
		//
		// Release what the filters reserved before handing off. Nothing beyond
		// this point calls a plugin again — the log phase takes its own
		// admission — and the handler below may hold the connection open
		// indefinitely: the console's event stream does exactly that. Keeping
		// the reservation would pin every plugin with a pre_route filter for as
		// long as a console is connected, so a disable or an upgrade would sit
		// out the full drain timeout before killing the process anyway.
		rc.ReleaseAdmissions()

		// The response is only wrapped when a post_handler filter actually
		// exists. Wrapping unconditionally would be worse than wasteful: any
		// wrapper hides interfaces the underlying writer implements, and
		// losing http.Flusher breaks every streaming response — server-sent
		// events included, which is how this console learns a plugin was
		// disabled.
		if !chain.HasPhase(pb.Phase_PHASE_POST_HANDLER) && !chain.HasPhase(pb.Phase_PHASE_LOG) {
			next.ServeHTTP(w, r)
			return
		}
		// Capture when somebody asked for the body, not when a post_handler
		// filter happens to exist. Those came apart for the log phase: a filter
		// declaring needs_response_body got one on plugin routes and an empty
		// one here, so auditing was blind to the responses of Core's own API.
		rec := newRecorder(w, chain.NeedsResponseBody())
		next.ServeHTTP(rec, r)
		rc.ResponseStatus = rec.status
		rc.ResponseHeader = rec.Header()
		rc.ResponseBody = rec.body()
		return
	}

	h.servePlugin(w, r, snap, chain, rc)
}

func (h *PluginHandler) servePlugin(w http.ResponseWriter, r *http.Request, snap *pluginhost.Snapshot, chain *pipeline.Chain, rc *pipeline.RequestContext) {
	// Core resolves the session first, so a filter sees whatever Core already
	// knows and can enrich or replace it.
	//
	// A session that does not resolve leaves the request anonymous rather than
	// refusing it here. That refusal used to happen before the authenticate
	// phase ran, which made the phase unreachable: a caller holding an API
	// key, a JWT, a signature — anything that is not one of Core's own session
	// cookies — was answered 401 by Core before the plugin that understands
	// their credential was ever asked. The whole filter:authenticate mechanism
	// existed and could not be used.
	if h.Auth != nil {
		if user, ok := h.Auth.Resolve(SessionToken(r)); ok {
			rc.Identity = &pb.Identity{
				UserId:   strconv.Itoa(int(user.ID)),
				Username: user.Username,
				Roles:    []string{user.Role},
			}
		}
	}

	for _, phase := range []pb.Phase{
		pb.Phase_PHASE_AUTHENTICATE,
		pb.Phase_PHASE_AUTHORIZE,
	} {
		if out := h.Runner.Run(r.Context(), chain, h.Registry, phase, rc); out.Stopped() {
			h.writeResponse(w, rc, out.ShortCircuit)
			return
		}
	}

	// The refusal, moved to after the phase whose job is to prevent it.
	//
	// Still unconditional: a plugin route reached by a caller nobody could
	// identify is refused exactly as before. What changed is only *when* the
	// question is asked, so that a plugin holding filter:authenticate gets to
	// answer it first. A deployment with no authenticate filter installed
	// behaves identically to before.
	//
	// The consequence worth stating: a plugin route cannot be public. If that
	// is ever wanted it belongs in the manifest, as a declaration an operator
	// approves, and not as an authorize filter quietly returning Continue.
	if h.Auth != nil && rc.Identity == nil {
		rc.ResponseStatus = http.StatusUnauthorized
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	if out := h.Runner.Run(r.Context(), chain, h.Registry, pb.Phase_PHASE_PRE_HANDLER, rc); out.Stopped() {
		h.writeResponse(w, rc, out.ShortCircuit)
		return
	}

	// Route on the possibly-rewritten path, so a pre_handler filter can send a
	// request to a different plugin or sub-path.
	key, sub, ok := splitPluginPath(rc.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	resp, err := h.callBackend(r.Context(), snap, key, sub, rc)
	if err != nil {
		// 502 only for a plugin that was there and failed. A plugin that is
		// not enabled, or is between states, is a different thing to be told.
		status := http.StatusBadGateway
		var unavailable *pluginUnavailableError
		if errors.As(err, &unavailable) {
			status = unavailable.status()
		}
		rc.ResponseStatus = status
		if out := h.Runner.Run(r.Context(), chain, h.Registry, pb.Phase_PHASE_ON_ERROR, rc); out.Stopped() {
			h.writeResponse(w, rc, out.ShortCircuit)
			return
		}
		http.Error(w, err.Error(), status)
		return
	}

	rc.ResponseStatus = int(resp.GetStatusCode())
	rc.ResponseHeader = pipeline.FromProtoHeaders(resp.GetHeaders())
	rc.ResponseBody = resp.GetBody()

	if rc.ResponseStatus >= 500 {
		if out := h.Runner.Run(r.Context(), chain, h.Registry, pb.Phase_PHASE_ON_ERROR, rc); out.Stopped() {
			h.writeResponse(w, rc, out.ShortCircuit)
			return
		}
	}

	if out := h.Runner.Run(r.Context(), chain, h.Registry, pb.Phase_PHASE_POST_HANDLER, rc); out.Stopped() {
		h.writeResponse(w, rc, out.ShortCircuit)
		return
	}

	writeFinal(w, rc)
}

// callBackend dispatches to the plugin that owns the route.
//
// This replaces the reverse tunnel's hand-rolled stream multiplexing: with
// go-plugin each call is an independent unary RPC over an HTTP/2 connection,
// so concurrent requests need neither stream ids nor a send mutex.
func (h *PluginHandler) callBackend(ctx context.Context, snap *pluginhost.Snapshot, key, sub string, rc *pipeline.RequestContext) (*pb.HttpResponse, error) {
	// Through the request's own admission rather than a fresh one: a filter on
	// this plugin has usually already reserved capacity, and asking again would
	// fail if the instance began draining in between — refusing a request that
	// the system had already accepted and run.
	inst, ok := snap.Pick(key)
	if ok && rc.Admit(key, inst.BeginRequest) {
		return h.dispatch(ctx, inst, sub, rc)
	}

	// The snapshot this request has been carrying can no longer serve the
	// plugin: an upgrade committed while the request was in flight, and the
	// instance it was routed to is draining or already gone. That reservation
	// only carries through for a plugin that ran a filter on this path; a
	// plugin reached by a plain route arrives here holding nothing, which is
	// the ordinary case and the one that used to answer 404 mid-upgrade.
	//
	// So resolve the instance once more against the live registry. The routing
	// decision is unchanged — this is still the same plugin serving the same
	// path — only the process behind it has been replaced, which is exactly
	// what a zero-downtime upgrade is supposed to be invisible about.
	live := false
	if cur := h.Registry.Current(); cur != nil {
		next, ok2 := cur.Pick(key)
		live = ok2
		if ok2 && next != inst && rc.Admit(key, next.BeginRequest) {
			return h.dispatch(ctx, next, sub, rc)
		}
	}

	// Gone or merely unavailable is a question about now, not about the
	// snapshot this request arrived with. Reading it from that snapshot said
	// 503 — try again shortly — for a plugin an operator had switched off and
	// which the registry no longer knows about at all, so the retry is against
	// something that is not coming back until somebody enables it. Disable
	// removes before it drains for exactly this reason; the answer has to
	// follow.
	return nil, &pluginUnavailableError{key: key, gone: !live}
}

// dispatch performs the backend call against an admitted instance.
func (h *PluginHandler) dispatch(ctx context.Context, inst *pluginhost.Instance, sub string, rc *pipeline.RequestContext) (*pb.HttpResponse, error) {
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
func (h *PluginHandler) logPhase(chain *pipeline.Chain, rc *pipeline.RequestContext) {
	// ForAsync because this outlives the response: the request has already
	// released its reservations by the time this phase runs, so it needs its
	// own to release in turn.
	h.Runner.RunAsync(context.Background(), chain, h.Registry, pb.Phase_PHASE_LOG, rc.ForAsync())
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

// writeResponse sends a short circuit and records what it sent.
//
// Recording here rather than at each of the six call sites, for the reason the
// log phase became a defer: a status written in one place and remembered in
// another drift apart, and the drift is silent. Measured before this took the
// rc: every short-circuited request — a filter refusing, Core refusing an
// unauthenticated caller — reached the audit trail as status 0, which is the
// same thing it says when it does not know. An audit trail's most interesting
// rows are the refusals.
func (h *PluginHandler) writeResponse(w http.ResponseWriter, rc *pipeline.RequestContext, resp *pb.HttpResponse) {
	for k, hv := range resp.GetHeaders() {
		for _, v := range hv.GetValues() {
			w.Header().Add(k, v)
		}
	}
	status := normalizeStatus(int(resp.GetStatusCode()))
	rc.ResponseStatus = status
	rc.ResponseHeader = pipeline.FromProtoHeaders(resp.GetHeaders())
	rc.ResponseBody = resp.GetBody()
	w.WriteHeader(status)
	_, _ = w.Write(resp.GetBody())
}

func writeFinal(w http.ResponseWriter, rc *pipeline.RequestContext) {
	for k, vs := range rc.ResponseHeader {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(normalizeStatus(rc.ResponseStatus))
	_, _ = w.Write(rc.ResponseBody)
}

// normalizeStatus keeps a plugin's status code inside what net/http accepts.
//
// WriteHeader panics on a code outside 100–999, so passing one straight
// through turns a plugin's bug into a panic in Core's request goroutine and a
// broken connection for the caller. Plugins are trusted code, but trusted code
// still has bugs, and this one is a one-line typo away.
//
// Zero means the plugin never set a status, which is the ordinary "just write
// the body" case and becomes 200. Anything else out of range is reported as a
// bad gateway, because the plugin genuinely did answer — it just answered with
// something that is not an HTTP status.
func normalizeStatus(code int) int {
	switch {
	case code == 0:
		return http.StatusOK
	case code < 100 || code > 599:
		return http.StatusBadGateway
	default:
		return code
	}
}

// pluginUnavailableError is a backend that could not be called, and why.
//
// The why decides the status code, and getting it wrong is not cosmetic:
// everything used to be 502, so a plugin an operator deliberately switched off
// answered every caller with "the upstream is broken". Measured during a
// disable under load: 5801 requests, all 502. A client cannot tell that from a
// crash, so it retries; an operator watching for 502 gets paged by a change
// they made on purpose.
type pluginUnavailableError struct {
	key string
	// gone means the plugin is not in the snapshot at all — disabled, never
	// installed, or quarantined. Its routes do not exist, the same way its
	// menu no longer does.
	gone bool
}

func (e *pluginUnavailableError) Error() string {
	if e.gone {
		return "plugin " + e.key + " is not enabled"
	}
	return "plugin " + e.key + " is not accepting requests"
}

// status maps the reason to what a caller is told.
//
//	404  the plugin is not enabled: this route does not exist. Do not retry.
//	503  it is enabled but between states — starting, draining, at capacity.
//	     Transient, so retrying is the right response.
func (e *pluginUnavailableError) status() int {
	if e.gone {
		return http.StatusNotFound
	}
	return http.StatusServiceUnavailable
}

// splitPluginPath turns /api/plugins/<key>/<sub> into its parts. A request for
// the bare prefix, or for a key with no sub-path, is not routable.
//
// The sub-path is cleaned before it is handed over, so a plugin can never
// receive one containing "..". Plugins are trusted code, but a plugin that
// joins this onto a directory — to serve a template, a report, an asset —
// would be walking out of it, and the encoded form (%2e%2e%2f) arrives here
// already decoded by net/http. Cleaning once, centrally, means no plugin
// author has to remember to.
func splitPluginPath(p string) (key, sub string, ok bool) {
	rest := strings.TrimPrefix(p, PluginAPIPrefix)
	if rest == p || rest == "" {
		return "", "", false
	}
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return rest, "/", true
	}
	return rest[:i], cleanSubPath(rest[i:]), true
}

// cleanSubPath resolves "." and ".." and collapses repeated slashes, keeping a
// trailing slash because routers commonly distinguish /items from /items/.
func cleanSubPath(sub string) string {
	cleaned := path.Clean(sub)
	if strings.HasSuffix(sub, "/") && !strings.HasSuffix(cleaned, "/") {
		cleaned += "/"
	}
	return cleaned
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

// Core's own handlers reach the trace id through the response.
//
// This used to be threaded into the downstream request's context, with an
// exported accessor for handlers to read it back. Nothing ever called it — the
// audit middleware, the one place that wanted the id, sits outside this
// handler and never saw that context at all. It reads X-Request-Id off the
// response instead, which is set before anything downstream runs and needs no
// plumbing.
//
// So the context value and its accessor are gone, along with the two
// allocations every non-plugin request was paying for a value nobody read.

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

// Flush forwards to the underlying writer.
//
// Wrapping a ResponseWriter hides whatever optional interfaces it implements,
// and losing http.Flusher silently converts a streaming response into one that
// buffers until the handler returns — which for an event stream means it never
// arrives at all. Forwarding it keeps SSE and any other streaming handler
// working when a filter chain is installed.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer for
// deadlines and any other capability this wrapper does not forward itself.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
