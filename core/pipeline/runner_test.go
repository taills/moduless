package pipeline

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// fakeTarget is a plugin instance as far as the pipeline is concerned.
type fakeTarget struct {
	resp  *pb.FilterResponse
	err   error
	delay time.Duration

	allow    bool
	draining bool

	calls     atomic.Int32
	successes atomic.Int32
	failures  atomic.Int32
	lastReq   atomic.Pointer[pb.FilterRequest]
}

func newFakeTarget(resp *pb.FilterResponse) *fakeTarget {
	return &fakeTarget{resp: resp, allow: true}
}

func (f *fakeTarget) Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error) {
	f.calls.Add(1)
	f.lastReq.Store(req)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeTarget) Allow() bool    { return f.allow }
func (f *fakeTarget) RecordSuccess() { f.successes.Add(1) }
func (f *fakeTarget) RecordFailure() { f.failures.Add(1) }
func (f *fakeTarget) Begin() (func(), bool) {
	if f.draining {
		return nil, false
	}
	return func() {}, true
}

// fakeResolver maps keys to targets; a missing key models an offline plugin.
type fakeResolver map[string]Target

func (r fakeResolver) Target(key string) (Target, bool) {
	t, ok := r[key]
	return t, ok
}

func continueResp() *pb.FilterResponse {
	return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}
}

// buildChain is a helper that compiles one plugin's filter declarations.
func buildChain(t *testing.T, key string, allowIdentity bool, decls ...manifest.FilterDecl) *Chain {
	t.Helper()
	m := &manifest.Manifest{Key: key, Version: "1.0.0", Filters: decls}
	compiled, err := m.CompileFilters()
	if err != nil {
		t.Fatalf("CompileFilters: %v", err)
	}
	chain, err := BuildChain([]PluginFilters{{
		Key:                   key,
		Filters:               compiled,
		AllowIdentityMutation: allowIdentity,
	}}, DefaultDefaults())
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
	return chain
}

func decl(phase string, paths []string, mutate func(*manifest.FilterDecl)) manifest.FilterDecl {
	d := manifest.FilterDecl{
		Phase: phase,
		Match: manifest.FilterMatch{Paths: paths},
	}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

func newCtx() *RequestContext {
	return &RequestContext{
		TraceID: "trace-1",
		Method:  "GET",
		Path:    "/api/plugins/hello/items",
		Header:  http.Header{},
	}
}

func TestRunSkipsPhaseWithNoFilters(t *testing.T) {
	var r Runner
	target := newFakeTarget(continueResp())

	chain := buildChain(t, "p", false, decl(manifest.PhaseLog, []string{"/**"}, nil))
	out := r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, newCtx())

	if out.Stopped() {
		t.Error("an unsubscribed phase short-circuited")
	}
	if got := target.calls.Load(); got != 0 {
		t.Errorf("plugin called %d times for an unsubscribed phase, want 0", got)
	}
}

func TestRunSkipsNonMatchingPath(t *testing.T) {
	var r Runner
	target := newFakeTarget(continueResp())

	chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/api/other/**"}, nil))
	rc := newCtx()
	out := r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, rc)

	if out.Stopped() {
		t.Error("a non-matching filter short-circuited")
	}
	if got := target.calls.Load(); got != 0 {
		t.Errorf("plugin called %d times for a non-matching path, want 0", got)
	}
}

func TestRunShortCircuits(t *testing.T) {
	var r Runner
	target := newFakeTarget(&pb.FilterResponse{
		Action: pb.FilterResponse_ACTION_SHORT_CIRCUIT,
		ShortCircuitResponse: &pb.HttpResponse{
			StatusCode: 429,
			Body:       []byte("rate limited"),
		},
	})

	chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"}, nil))
	out := r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, newCtx())

	if !out.Stopped() {
		t.Fatal("expected the pipeline to stop")
	}
	if got := out.ShortCircuit.GetStatusCode(); got != 429 {
		t.Errorf("status = %d, want 429", got)
	}
}

// A filter that stops the request without supplying a response is a plugin
// bug. Turning that into a 5xx would escalate one plugin's bug into an outage,
// so the request continues instead.
func TestShortCircuitWithoutResponseContinues(t *testing.T) {
	var reported int
	r := Runner{OnFilterError: func(*Filter, error) { reported++ }}
	target := newFakeTarget(&pb.FilterResponse{Action: pb.FilterResponse_ACTION_SHORT_CIRCUIT})

	chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"}, nil))
	out := r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, newCtx())

	if out.Stopped() {
		t.Error("a malformed short-circuit stopped the request")
	}
	if reported == 0 {
		t.Error("the malformed response was not reported")
	}
}

func TestMutateAppliesHeadersPathAndContext(t *testing.T) {
	var r Runner
	target := newFakeTarget(&pb.FilterResponse{
		Action: pb.FilterResponse_ACTION_MUTATE,
		Mutation: &pb.RequestMutation{
			SetRequestHeaders:    map[string]*pb.HeaderValues{"X-Tenant": {Values: []string{"acme"}}},
			RemoveRequestHeaders: []string{"X-Secret"},
			RewritePath:          "/api/plugins/hello/v2/items",
			SetContext:           map[string]string{"bucket": "b1"},
		},
	})

	chain := buildChain(t, "p", false, decl(manifest.PhasePreHandler, []string{"/**"}, nil))
	rc := newCtx()
	rc.Header.Set("X-Secret", "leak")

	r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_HANDLER, rc)

	if got := rc.Header.Get("X-Tenant"); got != "acme" {
		t.Errorf("X-Tenant = %q, want acme", got)
	}
	if got := rc.Header.Get("X-Secret"); got != "" {
		t.Errorf("X-Secret = %q, want it removed", got)
	}
	if rc.Path != "/api/plugins/hello/v2/items" {
		t.Errorf("path = %q, want the rewritten one", rc.Path)
	}
	if rc.Values["bucket"] != "b1" {
		t.Errorf("context values = %v", rc.Values)
	}
}

// Identity is the one mutation that changes an authorization outcome, so it is
// gated twice: by permission and by phase.
func TestSetIdentityIsGated(t *testing.T) {
	newIdentity := &pb.Identity{UserId: "999", Roles: []string{"admin"}}

	tests := []struct {
		name          string
		phase         string
		protoPhase    pb.Phase
		allowIdentity bool
		wantApplied   bool
	}{
		{
			name: "permitted plugin in authenticate phase", phase: manifest.PhaseAuthenticate,
			protoPhase: pb.Phase_PHASE_AUTHENTICATE, allowIdentity: true, wantApplied: true,
		},
		{
			name: "permitted plugin in authorize phase", phase: manifest.PhaseAuthorize,
			protoPhase: pb.Phase_PHASE_AUTHORIZE, allowIdentity: true, wantApplied: true,
		},
		{
			// Without the permission a plugin could otherwise promote itself
			// to admin from any filter it declares.
			name: "plugin without the permission", phase: manifest.PhaseAuthenticate,
			protoPhase: pb.Phase_PHASE_AUTHENTICATE, allowIdentity: false, wantApplied: false,
		},
		{
			// Even a permitted plugin must not rewrite identity after
			// authorization has already been decided.
			name: "permitted plugin in a late phase", phase: manifest.PhaseLog,
			protoPhase: pb.Phase_PHASE_LOG, allowIdentity: true, wantApplied: false,
		},
		{
			name: "permitted plugin in post_handler", phase: manifest.PhasePostHandler,
			protoPhase: pb.Phase_PHASE_POST_HANDLER, allowIdentity: true, wantApplied: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r Runner
			target := newFakeTarget(&pb.FilterResponse{
				Action:   pb.FilterResponse_ACTION_MUTATE,
				Mutation: &pb.RequestMutation{SetIdentity: newIdentity},
			})

			chain := buildChain(t, "p", tc.allowIdentity, decl(tc.phase, []string{"/**"}, nil))
			rc := newCtx()
			rc.Identity = &pb.Identity{UserId: "1", Roles: []string{"user"}}

			r.Run(context.Background(), chain, fakeResolver{"p": target}, tc.protoPhase, rc)

			applied := rc.Identity.GetUserId() == "999"
			if applied != tc.wantApplied {
				t.Errorf("identity applied = %v, want %v (identity is now %v)",
					applied, tc.wantApplied, rc.Identity)
			}
		})
	}
}

func TestFailOpenVersusFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		failClosed bool
		wantStop   bool
	}{
		{name: "fail-open continues", failClosed: false, wantStop: false},
		{name: "fail-closed stops", failClosed: true, wantStop: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r Runner
			target := newFakeTarget(nil)
			target.err = errors.New("plugin exploded")

			chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"},
				func(d *manifest.FilterDecl) { d.FailClosed = tc.failClosed }))

			out := r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, newCtx())

			if out.Stopped() != tc.wantStop {
				t.Errorf("stopped = %v, want %v", out.Stopped(), tc.wantStop)
			}
			if got := target.failures.Load(); got != 1 {
				t.Errorf("breaker recorded %d failures, want 1", got)
			}
		})
	}
}

// A fail-open filter that is broken must still be visible; otherwise it
// disappears from operations precisely because it is failing.
func TestFailOpenStillReportsTheError(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	r := Runner{OnFilterError: func(f *Filter, err error) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, f.Label()+": "+err.Error())
	}}

	target := newFakeTarget(nil)
	target.err = errors.New("boom")
	chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"},
		func(d *manifest.FilterDecl) { d.Name = "guard" }))

	r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, newCtx())

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("reported %d errors, want 1: %v", len(seen), seen)
	}
}

func TestOfflineAndUnhealthyPlugins(t *testing.T) {
	tests := []struct {
		name       string
		resolver   Resolver
		failClosed bool
		wantStop   bool
		wantStatus int32
	}{
		{
			name:     "offline plugin fails open",
			resolver: fakeResolver{},
		},
		{
			name:       "offline plugin fails closed",
			resolver:   fakeResolver{},
			failClosed: true,
			wantStop:   true,
			wantStatus: http.StatusServiceUnavailable,
		},
		{
			name: "tripped breaker fails open",
			resolver: func() Resolver {
				tgt := newFakeTarget(continueResp())
				tgt.allow = false
				return fakeResolver{"p": tgt}
			}(),
		},
		{
			name: "draining plugin fails closed",
			resolver: func() Resolver {
				tgt := newFakeTarget(continueResp())
				tgt.draining = true
				return fakeResolver{"p": tgt}
			}(),
			failClosed: true,
			wantStop:   true,
			wantStatus: http.StatusServiceUnavailable,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r Runner
			chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"},
				func(d *manifest.FilterDecl) { d.FailClosed = tc.failClosed }))

			out := r.Run(context.Background(), chain, tc.resolver, pb.Phase_PHASE_PRE_ROUTE, newCtx())

			if out.Stopped() != tc.wantStop {
				t.Fatalf("stopped = %v, want %v", out.Stopped(), tc.wantStop)
			}
			if tc.wantStop && out.ShortCircuit.GetStatusCode() != tc.wantStatus {
				t.Errorf("status = %d, want %d", out.ShortCircuit.GetStatusCode(), tc.wantStatus)
			}
		})
	}
}

// A tripped breaker must not cost a call: skipping the plugin is the point.
func TestTrippedBreakerSkipsTheCall(t *testing.T) {
	var r Runner
	target := newFakeTarget(continueResp())
	target.allow = false

	chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"}, nil))
	r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, newCtx())

	if got := target.calls.Load(); got != 0 {
		t.Errorf("plugin called %d times with the breaker open, want 0", got)
	}
}

func TestFilterTimeoutIsEnforced(t *testing.T) {
	var r Runner
	target := newFakeTarget(continueResp())
	target.delay = 500 * time.Millisecond

	chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"},
		func(d *manifest.FilterDecl) {
			d.TimeoutMS = 20
			d.FailClosed = true
		}))

	start := time.Now()
	out := r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, newCtx())
	elapsed := time.Since(start)

	if !out.Stopped() {
		t.Error("a timed-out fail-closed filter did not stop the request")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("the call took %s; the 20ms timeout was not enforced", elapsed)
	}
	if got := target.failures.Load(); got != 1 {
		t.Errorf("breaker recorded %d failures, want 1", got)
	}
}

// Truncating a body would let a guard filter judge different bytes than the
// backend processes, so an oversized body is refused rather than trimmed.
func TestOversizedBodyIsNotTruncated(t *testing.T) {
	tests := []struct {
		name       string
		failClosed bool
		wantStop   bool
		wantCalls  int32
	}{
		{name: "fail-open skips the filter", failClosed: false, wantStop: false, wantCalls: 0},
		{name: "fail-closed rejects the request", failClosed: true, wantStop: true, wantCalls: 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r Runner
			target := newFakeTarget(continueResp())

			chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"},
				func(d *manifest.FilterDecl) {
					d.NeedsRequestBody = true
					d.MaxBodyBytes = 16
					d.FailClosed = tc.failClosed
				}))

			rc := newCtx()
			rc.RequestBody = make([]byte, 1024)

			out := r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, rc)

			if out.Stopped() != tc.wantStop {
				t.Errorf("stopped = %v, want %v", out.Stopped(), tc.wantStop)
			}
			if tc.wantStop && out.ShortCircuit.GetStatusCode() != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413", out.ShortCircuit.GetStatusCode())
			}
			if got := target.calls.Load(); got != tc.wantCalls {
				t.Errorf("plugin called %d times, want %d", got, tc.wantCalls)
			}
		})
	}
}

// Bodies are withheld unless the filter asked, because they dominate the cost
// of the call.
func TestBodyOnlySentWhenDeclared(t *testing.T) {
	tests := []struct {
		name     string
		needs    bool
		wantBody int
	}{
		{name: "not declared", needs: false, wantBody: 0},
		{name: "declared", needs: true, wantBody: 4},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var r Runner
			target := newFakeTarget(continueResp())

			chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"},
				func(d *manifest.FilterDecl) { d.NeedsRequestBody = tc.needs }))

			rc := newCtx()
			rc.RequestBody = []byte("body")

			r.Run(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_PRE_ROUTE, rc)

			got := len(target.lastReq.Load().GetBody())
			if got != tc.wantBody {
				t.Errorf("filter received %d body bytes, want %d", got, tc.wantBody)
			}
		})
	}
}

func TestFiltersRunInDeclaredOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string

	mkTarget := func(name string) *fakeTarget {
		tgt := newFakeTarget(continueResp())
		orig := tgt.resp
		_ = orig
		return tgt
	}
	a, b, c := mkTarget("a"), mkTarget("b"), mkTarget("c")

	// Record call order through the error hook, which fires per call when the
	// target errors; simpler: wrap resolution.
	recording := recordingResolver{
		inner: fakeResolver{"pa": a, "pb": b, "pc": c},
		onGet: func(key string) {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, key)
		},
	}

	build := func(key string, ord int) PluginFilters {
		m := &manifest.Manifest{Key: key, Version: "1", Filters: []manifest.FilterDecl{
			decl(manifest.PhasePreRoute, []string{"/**"}, func(d *manifest.FilterDecl) { d.Order = ord }),
		}}
		compiled, err := m.CompileFilters()
		if err != nil {
			t.Fatalf("CompileFilters: %v", err)
		}
		return PluginFilters{Key: key, Filters: compiled}
	}

	// Declared out of order on purpose.
	chain, err := BuildChain([]PluginFilters{build("pc", 30), build("pa", 10), build("pb", 20)}, DefaultDefaults())
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}

	var r Runner
	r.Run(context.Background(), chain, recording, pb.Phase_PHASE_PRE_ROUTE, newCtx())

	mu.Lock()
	defer mu.Unlock()
	want := []string{"pa", "pb", "pc"}
	if len(order) != len(want) {
		t.Fatalf("call order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("call order = %v, want %v", order, want)
		}
	}
}

type recordingResolver struct {
	inner Resolver
	onGet func(key string)
}

func (r recordingResolver) Target(key string) (Target, bool) {
	r.onGet(key)
	return r.inner.Target(key)
}

// The cheapest possible outcome — nobody subscribed — must stay free of
// allocation, since it is the common case for most traffic.
func TestEmptyPhaseDoesNotAllocate(t *testing.T) {
	var r Runner
	chain := EmptyChain()
	res := fakeResolver{}
	rc := newCtx()
	ctx := context.Background()

	got := testing.AllocsPerRun(200, func() {
		r.Run(ctx, chain, res, pb.Phase_PHASE_PRE_ROUTE, rc)
	})
	if got != 0 {
		t.Errorf("running an empty phase allocated %.1f times, want 0", got)
	}
}

func TestChainReportsBodyRequirements(t *testing.T) {
	chain := buildChain(t, "p", false,
		decl(manifest.PhasePreRoute, []string{"/**"}, nil),
		decl(manifest.PhaseLog, []string{"/**"}, func(d *manifest.FilterDecl) {
			d.Name = "audit"
			d.NeedsRequestBody = true
			d.MaxBodyBytes = 4096
		}),
	)

	if !chain.NeedsRequestBody() {
		t.Error("chain does not report needing a request body")
	}
	if got := chain.MaxRequestBodyBytes(); got != 4096 {
		t.Errorf("max request body = %d, want 4096", got)
	}
	if got := chain.Len(); got != 2 {
		t.Errorf("chain length = %d, want 2", got)
	}
}

func TestChainWithoutBodyFiltersNeedsNoBuffering(t *testing.T) {
	chain := buildChain(t, "p", false, decl(manifest.PhasePreRoute, []string{"/**"}, nil))
	if chain.NeedsRequestBody() {
		t.Error("chain reports needing a body when no filter declared one")
	}
}

func TestBuildChainRejectsUnknownPhase(t *testing.T) {
	m := &manifest.Manifest{Key: "p", Version: "1", Filters: []manifest.FilterDecl{
		{Phase: "nonsense", Match: manifest.FilterMatch{Paths: []string{"/**"}}},
	}}
	compiled, err := m.CompileFilters()
	if err != nil {
		t.Fatalf("CompileFilters: %v", err)
	}
	if _, err := BuildChain([]PluginFilters{{Key: "p", Filters: compiled}}, DefaultDefaults()); err == nil {
		t.Error("BuildChain accepted an unknown phase")
	}
}

func TestRunAsyncDoesNotBlock(t *testing.T) {
	var r Runner
	target := newFakeTarget(continueResp())
	target.delay = 100 * time.Millisecond

	chain := buildChain(t, "p", false, decl(manifest.PhaseLog, []string{"/**"}, nil))

	start := time.Now()
	r.RunAsync(context.Background(), chain, fakeResolver{"p": target}, pb.Phase_PHASE_LOG, newCtx())
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Errorf("RunAsync blocked for %s", elapsed)
	}

	deadline := time.Now().Add(2 * time.Second)
	for target.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if target.calls.Load() == 0 {
		t.Error("the async phase never ran")
	}
}

func BenchmarkRunEmptyPhase(b *testing.B) {
	var r Runner
	chain := EmptyChain()
	res := fakeResolver{}
	rc := newCtx()
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		r.Run(ctx, chain, res, pb.Phase_PHASE_PRE_ROUTE, rc)
	}
}

func BenchmarkRunNonMatchingFilter(b *testing.B) {
	m := &manifest.Manifest{Key: "p", Version: "1", Filters: []manifest.FilterDecl{
		{Phase: manifest.PhasePreRoute, Match: manifest.FilterMatch{Paths: []string{"/api/other/**"}}},
	}}
	compiled, _ := m.CompileFilters()
	chain, _ := BuildChain([]PluginFilters{{Key: "p", Filters: compiled}}, DefaultDefaults())

	var r Runner
	res := fakeResolver{"p": newFakeTarget(continueResp())}
	rc := newCtx()
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		r.Run(ctx, chain, res, pb.Phase_PHASE_PRE_ROUTE, rc)
	}
}
