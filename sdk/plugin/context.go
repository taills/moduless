package sdk

import (
	"context"
	"net/http"

	"google.golang.org/grpc/metadata"

	"github.com/taills/moduless/pluginapi"
	pb "github.com/taills/moduless/proto/plugin"
)

// UserContext is the authenticated caller, as Core resolved them.
type UserContext struct {
	UserID   string
	Username string
	Roles    []string
}

// Name is the username, or "" when there is no authenticated caller.
//
// Nil-safe on purpose. User returns nil for an unauthenticated request, so
// reaching straight for .Username panics — and a panic inside a plugin kills
// the process, turning one anonymous request into an outage. Every accessor
// here tolerates nil so that the obvious code is also the correct code.
func (u *UserContext) Name() string {
	if u == nil {
		return ""
	}
	return u.Username
}

// ID is the caller's user id, or "" when there is none.
func (u *UserContext) ID() string {
	if u == nil {
		return ""
	}
	return u.UserID
}

// Authenticated reports whether Core resolved a caller for this request.
func (u *UserContext) Authenticated() bool { return u != nil && u.UserID != "" }

// HasRole reports whether the caller holds a role.
func (u *UserContext) HasRole(role string) bool {
	if u == nil {
		return false
	}
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}

type contextKey int

const (
	userKey contextKey = iota
	traceKey
	valuesKey
)

// User returns the caller Core authenticated, or nil for an unauthenticated
// request. It is safe to call on any context.
func User(ctx context.Context) *UserContext {
	u, _ := ctx.Value(userKey).(*UserContext)
	return u
}

// TraceID returns the id that follows this request across every process it
// touches. Include it in logs and it lines up with Core's own.
func TraceID(ctx context.Context) string {
	id, _ := ctx.Value(traceKey).(string)
	return id
}

// Values returns data earlier filters attached to this request.
func Values(ctx context.Context) map[string]string {
	v, _ := ctx.Value(valuesKey).(map[string]string)
	return v
}

func withTrace(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceKey, traceID)
}

func withRequestValues(ctx context.Context, req *pb.HttpRequest) context.Context {
	ctx = withTrace(ctx, req.GetTraceId())
	if id := req.GetIdentity(); id != nil {
		ctx = context.WithValue(ctx, userKey, identityFrom(id))
	}
	return ctx
}

func withFilterValues(ctx context.Context, req *pb.FilterRequest) context.Context {
	ctx = withTrace(ctx, req.GetTraceId())
	if id := req.GetIdentity(); id != nil {
		ctx = context.WithValue(ctx, userKey, identityFrom(id))
	}
	if len(req.GetContext()) > 0 {
		ctx = context.WithValue(ctx, valuesKey, req.GetContext())
	}
	return ctx
}

func identityFrom(id *pb.Identity) *UserContext {
	return &UserContext{
		UserID:   id.GetUserId(),
		Username: id.GetUsername(),
		Roles:    id.GetRoles(),
	}
}

// outgoing attaches the trace id to a call the plugin makes back into Core, so
// a slow query or a queue write can be attributed to the request that caused
// it. Plugin authors never have to thread this by hand.
func outgoing(ctx context.Context) context.Context {
	if id := TraceID(ctx); id != "" {
		return metadata.AppendToOutgoingContext(ctx, pluginapi.TraceMetadataKey, id)
	}
	return ctx
}

// FilterRequest is what a filter sees.
//
// Body and the response fields are populated only when the manifest declared
// needs_request_body / needs_response_body, because moving a body across the
// process boundary roughly quadruples the cost of the call.
type FilterRequest struct {
	Phase    Phase
	TraceID  string
	Method   string
	Path     string
	Query    string
	ClientIP string
	Header   http.Header
	Identity *UserContext
	Body     []byte

	// Populated from the post_handler phase onwards.
	ResponseStatus int
	ResponseHeader http.Header
	ResponseBody   []byte

	// Values carries data set by earlier filters on this request.
	Values map[string]string
}

func filterRequestFrom(req *pb.FilterRequest) *FilterRequest {
	out := &FilterRequest{
		Phase:          Phase(req.GetPhase() - 1),
		TraceID:        req.GetTraceId(),
		Method:         req.GetMethod(),
		Path:           req.GetPath(),
		Query:          req.GetQuery(),
		ClientIP:       req.GetClientIp(),
		Header:         headersFrom(req.GetHeaders()),
		Body:           req.GetBody(),
		ResponseStatus: int(req.GetUpstreamStatus()),
		ResponseHeader: headersFrom(req.GetUpstreamHeaders()),
		ResponseBody:   req.GetUpstreamBody(),
		Values:         req.GetContext(),
	}
	if id := req.GetIdentity(); id != nil {
		out.Identity = identityFrom(id)
	}
	return out
}

// FilterResult is a filter's decision. Build one with Continue, Stop or Mutate.
type FilterResult struct {
	action pb.FilterResponse_Action

	status  int
	body    []byte
	stopHdr http.Header

	setReqHeaders    http.Header
	removeReqHeaders []string
	setRespHeaders   http.Header
	removeRespHdrs   []string
	rewritePath      string
	identity         *UserContext
	values           map[string]string
}

// Continue lets the request proceed unchanged.
func Continue() *FilterResult {
	return &FilterResult{action: pb.FilterResponse_ACTION_CONTINUE}
}

// Stop answers the request immediately, without reaching its backend.
func Stop(status int, body []byte) *FilterResult {
	return &FilterResult{
		action:  pb.FilterResponse_ACTION_SHORT_CIRCUIT,
		status:  status,
		body:    body,
		stopHdr: http.Header{},
	}
}

// Mutate changes the request and lets it proceed.
func Mutate() *FilterResult {
	return &FilterResult{
		action:         pb.FilterResponse_ACTION_MUTATE,
		setReqHeaders:  http.Header{},
		setRespHeaders: http.Header{},
		values:         map[string]string{},
	}
}

// WithHeader adds a header to a Stop response.
func (r *FilterResult) WithHeader(key, value string) *FilterResult {
	if r.stopHdr == nil {
		r.stopHdr = http.Header{}
	}
	r.stopHdr.Add(key, value)
	return r
}

// SetRequestHeader sets a header the backend will see.
func (r *FilterResult) SetRequestHeader(key, value string) *FilterResult {
	r.setReqHeaders.Set(key, value)
	return r
}

// RemoveRequestHeader strips a header before the backend sees it.
func (r *FilterResult) RemoveRequestHeader(key string) *FilterResult {
	r.removeReqHeaders = append(r.removeReqHeaders, key)
	return r
}

// SetResponseHeader sets a header on the response. Only meaningful from the
// post_handler phase onwards.
func (r *FilterResult) SetResponseHeader(key, value string) *FilterResult {
	r.setRespHeaders.Set(key, value)
	return r
}

// RemoveResponseHeader strips a response header.
func (r *FilterResult) RemoveResponseHeader(key string) *FilterResult {
	r.removeRespHdrs = append(r.removeRespHdrs, key)
	return r
}

// RewritePath changes where the request is routed. Only meaningful before the
// backend runs.
func (r *FilterResult) RewritePath(path string) *FilterResult {
	r.rewritePath = path
	return r
}

// SetIdentity replaces the authenticated caller.
//
// Core honours this only for plugins holding the "filter:authenticate"
// permission, and only during the authenticate and authorize phases. Anywhere
// else it is ignored and logged, because a filter changing identity after
// authorization has run would be an escalation.
func (r *FilterResult) SetIdentity(u *UserContext) *FilterResult {
	r.identity = u
	return r
}

// SetValue passes data to later filters on the same request.
func (r *FilterResult) SetValue(key, value string) *FilterResult {
	if r.values == nil {
		r.values = map[string]string{}
	}
	r.values[key] = value
	return r
}

func (r *FilterResult) toProto() *pb.FilterResponse {
	switch r.action {
	case pb.FilterResponse_ACTION_SHORT_CIRCUIT:
		return &pb.FilterResponse{
			Action: r.action,
			ShortCircuitResponse: &pb.HttpResponse{
				StatusCode: int32(r.status),
				Headers:    headersTo(r.stopHdr),
				Body:       r.body,
			},
		}

	case pb.FilterResponse_ACTION_MUTATE:
		m := &pb.RequestMutation{
			SetRequestHeaders:     headersTo(r.setReqHeaders),
			RemoveRequestHeaders:  r.removeReqHeaders,
			SetResponseHeaders:    headersTo(r.setRespHeaders),
			RemoveResponseHeaders: r.removeRespHdrs,
			RewritePath:           r.rewritePath,
			SetContext:            r.values,
		}
		if r.identity != nil {
			m.SetIdentity = &pb.Identity{
				UserId:   r.identity.UserID,
				Username: r.identity.Username,
				Roles:    r.identity.Roles,
			}
		}
		return &pb.FilterResponse{Action: r.action, Mutation: m}

	default:
		return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}
	}
}

// --- testing ----------------------------------------------------------------

// Action is what a filter decided.
type Action string

const (
	ActionContinue     Action = "continue"
	ActionStop         Action = "stop"
	ActionMutate       Action = "mutate"
	ActionUnrecognised Action = "unrecognised"
)

// FilterDecision is a FilterResult opened up, for a test to assert on.
//
// A filter is an ordinary function — a plugin author can call theirs directly —
// but until this they could not check what it returned: every field of
// FilterResult is unexported and there were no accessors, so the one thing a
// filter test needs to do was the one thing it could not. Reading the fields
// back through a struct rather than through eight accessors keeps the builder's
// method names free: RewritePath is already a setter.
type FilterDecision struct {
	Action Action

	// Set when the filter stopped the request.
	Status int
	Body   []byte
	Header http.Header

	// Set when the filter mutated it.
	Identity       *UserContext
	Path           string
	SetRequest     http.Header
	RemoveRequest  []string
	SetResponse    http.Header
	RemoveResponse []string
	Values         map[string]string
}

// Inspect reports what this result says, for tests.
func (r *FilterResult) Inspect() FilterDecision {
	if r == nil {
		return FilterDecision{Action: ActionUnrecognised}
	}
	d := FilterDecision{
		Status:         r.status,
		Body:           r.body,
		Header:         r.stopHdr,
		Identity:       r.identity,
		Path:           r.rewritePath,
		SetRequest:     r.setReqHeaders,
		RemoveRequest:  r.removeReqHeaders,
		SetResponse:    r.setRespHeaders,
		RemoveResponse: r.removeRespHdrs,
		Values:         r.values,
	}
	switch r.action {
	case pb.FilterResponse_ACTION_CONTINUE:
		d.Action = ActionContinue
	case pb.FilterResponse_ACTION_SHORT_CIRCUIT:
		d.Action = ActionStop
	case pb.FilterResponse_ACTION_MUTATE:
		d.Action = ActionMutate
	default:
		d.Action = ActionUnrecognised
	}
	return d
}

// WithUser returns a context carrying an authenticated caller, so a handler
// that reads sdk.User(ctx) can be tested without a live Core.
//
// Core builds this context itself from the identity it passes with each
// request. A plugin author writing a test for the authorization pattern this
// SDK documents — `if !sdk.User(r.Context()).HasRole("admin")` — had no way to
// produce a request that satisfies it, so the check most worth testing was
// untestable.
func WithUser(ctx context.Context, u *UserContext) context.Context {
	return context.WithValue(ctx, userKey, u)
}
