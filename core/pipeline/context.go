package pipeline

import (
	"net/http"
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
func (rc *RequestContext) buildFilterRequest(phase pb.Phase, f *Filter) *pb.FilterRequest {
	req := &pb.FilterRequest{
		TraceId:       rc.TraceID,
		Phase:         phase,
		Method:        rc.Method,
		Path:          rc.Path,
		Query:         rc.Query,
		Headers:       ToProtoHeaders(rc.Header),
		ClientIp:      rc.ClientIP,
		Identity:      rc.Identity,
		Context:       rc.Values,
		ElapsedMicros: rc.Elapsed().Microseconds(),
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
