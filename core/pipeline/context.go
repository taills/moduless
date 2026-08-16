package pipeline

import (
	"net/http"
	"sync"
	"time"

	pb "github.com/taills/moduless/proto/plugin"
)

// RequestContext is the mutable state one request carries through the
// pipeline. Filters read it and, via mutations, write to it; the gateway reads
// the final result back out.
//
// It belongs to a single request goroutine and is never shared, so nothing
// here needs synchronising.
type RequestContext struct {
	// TraceID follows the request across every process it touches: filters,
	// the backend plugin, HostServices calls those make, and any queue message
	// they enqueue. It is what makes a slow query attributable to the request
	// that caused it.
	TraceID string

	Method   string
	Path     string // filters may rewrite this before routing
	Query    string
	ClientIP string

	Header   http.Header
	Identity *pb.Identity

	// Values carries data between filters within one request, e.g. a rate
	// limiter recording the bucket it charged so a later log filter can report
	// it. It is never sent to the browser.
	Values map[string]string

	// RequestBody is populated only when some filter in the chain declared
	// needs_request_body.
	RequestBody []byte

	// Response state, populated after the backend runs.
	ResponseStatus int
	ResponseHeader http.Header
	ResponseBody   []byte

	startedAt time.Time

	// protoHeaders caches the wire form of Header.
	//
	// Every filter in a chain is sent the same headers, and converting them
	// allocates a map plus one value per header — repeated per filter, for a
	// result that cannot differ unless a filter changes the headers. The cache
	// is dropped when one does.
	protoHeaders map[string]*pb.HeaderValues

	// admissions records, per plugin, the capacity this request already holds.
	//
	// A request calls one plugin more than once — a filter, then the backend,
	// then a later phase — and each call used to admit itself separately. That
	// made admission a property of the call rather than of the request, so an
	// upgrade landing between two of them refused the second: the request had
	// been accepted, had run, and was then told the plugin was draining. It
	// came back 502 despite nothing having gone wrong with it.
	//
	// Admitting once and holding for the life of the request also gives the
	// drain something accurate to wait for. It is the same principle as loading
	// the routing snapshot once: decide at the start, then stay consistent.
	admissionsMu sync.Mutex
	admissions   map[string]func()
}

// Admit reserves capacity on a plugin for this request, or reports that the
// plugin is not accepting work.
//
// The second and later calls for the same plugin reuse the first admission
// rather than asking again, so a request that was accepted stays accepted even
// if the plugin starts draining while it is still running.
func (rc *RequestContext) Admit(pluginKey string, begin func() (func(), bool)) bool {
	rc.admissionsMu.Lock()
	defer rc.admissionsMu.Unlock()

	if _, held := rc.admissions[pluginKey]; held {
		return true
	}
	release, ok := begin()
	if !ok {
		return false
	}
	if rc.admissions == nil {
		rc.admissions = make(map[string]func(), 2)
	}
	rc.admissions[pluginKey] = release
	return true
}

// ForAsync returns a copy of the request for work that outlives the response.
//
// The log phase runs on its own goroutine after the response has been written,
// which is after the request released everything it held. Admitting into the
// original from there would add a reservation nothing ever frees: every
// request carrying a log filter would leak one in-flight count, and a drain
// would then wait its full timeout for requests that finished long ago.
//
// The copy shares the request's data — which is no longer being written by the
// time the log phase starts — and keeps its own admissions, released when that
// phase completes.
func (rc *RequestContext) ForAsync() *RequestContext {
	// Fields are copied by name rather than by copying the struct: it carries
	// the admissions mutex, and copying a lock is both a vet error and a real
	// one — the copy would guard nothing.
	return &RequestContext{
		TraceID:        rc.TraceID,
		Method:         rc.Method,
		Path:           rc.Path,
		Query:          rc.Query,
		ClientIP:       rc.ClientIP,
		Header:         rc.Header,
		Identity:       rc.Identity,
		Values:         rc.Values,
		RequestBody:    rc.RequestBody,
		ResponseStatus: rc.ResponseStatus,
		ResponseHeader: rc.ResponseHeader,
		ResponseBody:   rc.ResponseBody,
		startedAt:      rc.startedAt,
	}
}

// ReleaseAdmissions frees every reservation this request holds. The gateway
// calls it once the response has been written, which is the point at which a
// draining instance may finally stop.
func (rc *RequestContext) ReleaseAdmissions() {
	rc.admissionsMu.Lock()
	defer rc.admissionsMu.Unlock()

	for key, release := range rc.admissions {
		release()
		delete(rc.admissions, key)
	}
}

// NewRequestContext seeds a context from an incoming HTTP request. The body is
// supplied separately because the gateway only reads it when the chain
// actually needs it.
func NewRequestContext(traceID string, r *http.Request, clientIP string) *RequestContext {
	return &RequestContext{
		TraceID:   traceID,
		Method:    r.Method,
		Path:      r.URL.Path,
		Query:     r.URL.RawQuery,
		ClientIP:  clientIP,
		Header:    r.Header.Clone(),
		startedAt: time.Now(),
	}
}

// Elapsed reports how long the request has been in the pipeline.
func (rc *RequestContext) Elapsed() time.Duration { return time.Since(rc.startedAt) }

// SetValue records a value for later filters.
func (rc *RequestContext) SetValue(k, v string) {
	if rc.Values == nil {
		rc.Values = map[string]string{}
	}
	rc.Values[k] = v
}

// buildFilterRequest assembles the message sent to one filter.
//
// Bodies are attached only when that specific filter asked for them: a 64KB
// body roughly quadruples the cost of the call, and most filters only need
// method, path, headers and identity.
// wireHeaders returns the request headers in wire form, converting once per
// request rather than once per filter.
//
// The result is shared across every filter in the chain. That is safe because
// nothing mutates it: a filter changing headers goes through applyMutation,
// which edits rc.Header and drops this cache, and the protobuf marshaller only
// reads.
func (rc *RequestContext) wireHeaders() map[string]*pb.HeaderValues {
	if rc.protoHeaders == nil {
		rc.protoHeaders = ToProtoHeaders(rc.Header)
	}
	return rc.protoHeaders
}

func (rc *RequestContext) buildFilterRequest(phase pb.Phase, f *Filter) *pb.FilterRequest {
	req := &pb.FilterRequest{
		TraceId:       rc.TraceID,
		Phase:         phase,
		Method:        rc.Method,
		Path:          rc.Path,
		Query:         rc.Query,
		Headers:       rc.wireHeaders(),
		ClientIp:      rc.ClientIP,
		Identity:      rc.Identity,
		Context:       rc.Values,
		ElapsedMicros: rc.Elapsed().Microseconds(),
		// Which declaration matched. A plugin may declare several filters in
		// one phase and the SDK dispatches by phase, so without this the two
		// calls are indistinguishable at the far end.
		FilterName: f.Decl.Name,
	}
	if f.Decl.NeedsRequestBody {
		req.Body = rc.RequestBody
	}
	if isResponsePhase(phase) {
		req.UpstreamStatus = int32(rc.ResponseStatus)
		req.UpstreamHeaders = ToProtoHeaders(rc.ResponseHeader)
		if f.Decl.NeedsResponseBody {
			req.UpstreamBody = rc.ResponseBody
		}
	}
	return req
}

// isResponsePhase reports whether the backend has already run by this phase,
// so response fields are meaningful.
func isResponsePhase(phase pb.Phase) bool {
	switch phase {
	case pb.Phase_PHASE_POST_HANDLER, pb.Phase_PHASE_ON_ERROR, pb.Phase_PHASE_LOG:
		return true
	default:
		return false
	}
}

// applyMutation folds a filter's decision back into the request state.
//
// allowIdentity gates the one mutation that changes an authorization outcome.
// Core passes true only when the plugin holds the filter:authenticate
// permission and the phase is one where identity is still being established;
// otherwise a plugin with a log-phase filter could rewrite the caller's
// identity after authorization had already run.
func (rc *RequestContext) applyMutation(m *pb.RequestMutation, phase pb.Phase, allowIdentity bool) {
	if m == nil {
		return
	}

	if rc.Header == nil {
		rc.Header = http.Header{}
	}
	applyHeaderMutation(rc.Header, m.GetSetRequestHeaders(), m.GetRemoveRequestHeaders())
	// The headers just changed, so the cached wire form no longer describes
	// them. Later filters must see the edit — that is what mutation is for.
	rc.protoHeaders = nil

	if isResponsePhase(phase) {
		if rc.ResponseHeader == nil {
			rc.ResponseHeader = http.Header{}
		}
		applyHeaderMutation(rc.ResponseHeader, m.GetSetResponseHeaders(), m.GetRemoveResponseHeaders())
	}

	// A rewritten path only means anything before routing has happened.
	if p := m.GetRewritePath(); p != "" && !isResponsePhase(phase) {
		rc.Path = p
	}

	if id := m.GetSetIdentity(); id != nil && allowIdentity {
		rc.Identity = id
	}

	for k, v := range m.GetSetContext() {
		rc.SetValue(k, v)
	}
}

// identityMutationAllowed reports whether a filter in this phase may change
// the caller's identity. Restricting it to the authentication phases means a
// late-running filter cannot retroactively alter who Core thought the caller
// was.
func identityMutationAllowed(phase pb.Phase) bool {
	switch phase {
	case pb.Phase_PHASE_AUTHENTICATE, pb.Phase_PHASE_AUTHORIZE:
		return true
	default:
		return false
	}
}
