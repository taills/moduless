package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	pb "github.com/taills/moduless/proto/plugin"
)

// Target is one plugin instance as the pipeline needs to see it.
//
// The pipeline talks to plugins through this interface rather than depending
// on the plugin host directly. That keeps the dependency one-way — the host's
// snapshot can carry a Chain without an import cycle — and lets the whole
// pipeline be tested against fakes, with no subprocess in sight.
type Target interface {
	Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error)

	// Allow reports whether the circuit breaker currently permits a call.
	Allow() bool
	RecordSuccess()
	RecordFailure()

	// Begin reserves capacity on the instance so a drain waits for this call.
	// It returns false when the instance is no longer accepting work.
	Begin() (release func(), ok bool)
}

// Resolver maps a plugin key to a callable target for the current request.
//
// Resolution happens per call rather than being baked into the chain, so a hot
// upgrade takes effect immediately: the next filter call lands on the new
// process without the chain being rebuilt.
type Resolver interface {
	Target(pluginKey string) (Target, bool)
}

// Outcome is the result of running one phase.
type Outcome struct {
	// ShortCircuit, when set, is the response to send instead of continuing.
	ShortCircuit *pb.HttpResponse
}

// Stopped reports whether the pipeline should stop and write the response.
func (o Outcome) Stopped() bool { return o.ShortCircuit != nil }

// Runner executes phases. It is stateless and safe for concurrent use.
type Runner struct {
	// OnFilterError, when set, is called for every filter failure. Core wires
	// it to the log so a fail-open filter that is quietly broken is still
	// visible, rather than disappearing precisely because it failed open.
	OnFilterError func(f *Filter, err error)
}

// Run executes every filter subscribed to phase, in order.
//
// A phase nobody subscribed to returns immediately after one array-length
// check, which is what keeps unfiltered traffic free of pipeline cost.
func (r *Runner) Run(ctx context.Context, chain *Chain, res Resolver, phase pb.Phase, rc *RequestContext) Outcome {
	if chain == nil || !chain.HasPhase(phase) {
		return Outcome{}
	}

	for _, f := range chain.Filters(phase) {
		if !f.Matches(rc.Method, rc.Path) {
			continue
		}
		if resp := r.runOne(ctx, res, phase, f, rc); resp != nil {
			return Outcome{ShortCircuit: resp}
		}
	}
	return Outcome{}
}

// RunAsync dispatches a phase without blocking the caller, for PHASE_LOG,
// where the response has already been flushed and a filter's result cannot
// affect it. Errors are reported through OnFilterError; short-circuit
// decisions are meaningless here and ignored.
//
// The caller must pass a context that outlives the request (the request's own
// context is cancelled as soon as the handler returns) and a RequestContext
// that nothing else will mutate.
func (r *Runner) RunAsync(ctx context.Context, chain *Chain, res Resolver, phase pb.Phase, rc *RequestContext) {
	if chain == nil || !chain.HasPhase(phase) {
		return
	}
	go r.Run(ctx, chain, res, phase, rc)
}

// runOne invokes a single filter and applies its decision. It returns a
// non-nil response when the request must be short-circuited.
func (r *Runner) runOne(ctx context.Context, res Resolver, phase pb.Phase, f *Filter, rc *RequestContext) *pb.HttpResponse {
	// A body larger than this filter is prepared to handle must not be
	// silently truncated: a guard filter judging a partial body could reach a
	// different conclusion than the backend handling the whole one. Fail-closed
	// filters therefore reject the request outright; fail-open ones are skipped.
	if f.Decl.NeedsRequestBody && len(rc.RequestBody) > f.MaxBody {
		err := fmt.Errorf("request body %d bytes exceeds the filter's limit of %d",
			len(rc.RequestBody), f.MaxBody)
		r.report(f, err)
		return r.failure(f, http.StatusRequestEntityTooLarge, "request body too large for filter "+f.Label())
	}

	target, ok := res.Target(f.PluginKey)
	if !ok {
		r.report(f, errors.New("plugin is not running"))
		return r.failure(f, http.StatusServiceUnavailable, "plugin "+f.PluginKey+" is unavailable")
	}

	if !target.Allow() {
		// The breaker is open: the plugin has been failing, so skip it without
		// paying for another call.
		return r.failure(f, http.StatusServiceUnavailable, "plugin "+f.PluginKey+" is unhealthy")
	}

	// Held for the life of the request, not just this call: see
	// RequestContext.Admit.
	if !rc.Admit(f.PluginKey, target.Begin) {
		r.report(f, errors.New("plugin is draining"))
		return r.failure(f, http.StatusServiceUnavailable, "plugin "+f.PluginKey+" is draining")
	}

	callCtx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()

	resp, err := target.Filter(callCtx, rc.buildFilterRequest(phase, f))
	if err != nil {
		target.RecordFailure()
		r.report(f, err)
		// 503 rather than 502, and deliberately the same code the unavailable,
		// unhealthy and draining paths above return. To a caller they are one
		// situation — this filter could not reach a decision, so a fail-closed
		// filter is refusing the request — and a client that retries on one
		// should retry on all of them. Splitting them across status codes only
		// invites handling the same condition two different ways.
		return r.failure(f, http.StatusServiceUnavailable, "filter "+f.Label()+" failed")
	}
	target.RecordSuccess()

	switch resp.GetAction() {
	case pb.FilterResponse_ACTION_SHORT_CIRCUIT:
		sc := resp.GetShortCircuitResponse()
		if sc == nil {
			// A filter that says "stop" without saying what to send is a bug
			// in the plugin. Failing closed here would be worse: it would turn
			// a plugin bug into a blanket outage.
			r.report(f, errors.New("returned SHORT_CIRCUIT with no response"))
			return nil
		}
		return sc

	case pb.FilterResponse_ACTION_MUTATE:
		allowIdentity := f.AllowIdentityMutation && identityMutationAllowed(phase)
		if resp.GetMutation().GetSetIdentity() != nil && !allowIdentity {
			r.report(f, errors.New("attempted to set identity without the filter:authenticate permission or outside an authentication phase"))
		}
		rc.applyMutation(resp.GetMutation(), phase, allowIdentity)
		return nil

	default:
		return nil
	}
}

// failure turns a filter problem into either a short-circuit response
// (fail-closed) or a decision to carry on (fail-open).
//
// Fail-open is the default because most filters observe rather than guard, and
// a broken observer should not take the site down. Anything enforcing a
// security decision has to opt in to fail-closed — otherwise an outage in that
// plugin would silently become an authorisation bypass.
func (r *Runner) failure(f *Filter, status int, message string) *pb.HttpResponse {
	if !f.Decl.FailClosed {
		return nil
	}
	return &pb.HttpResponse{
		StatusCode: int32(status),
		Headers: map[string]*pb.HeaderValues{
			"Content-Type": {Values: []string{"text/plain; charset=utf-8"}},
		},
		Body: []byte(message),
	}
}

func (r *Runner) report(f *Filter, err error) {
	if r.OnFilterError != nil {
		r.OnFilterError(f, err)
	}
}
