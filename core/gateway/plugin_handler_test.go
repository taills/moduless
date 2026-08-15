package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// --- fakes ------------------------------------------------------------------

type fakePluginClient struct {
	httpResp   *pb.HttpResponse
	httpErr    error
	filterResp *pb.FilterResponse
	// hold blocks HandleHTTP until closed, so a test can keep a request in
	// flight and observe a plugin that is genuinely draining rather than one
	// that has already finished draining and stopped.
	hold chan struct{}

	httpCalls   atomic.Int32
	filterCalls atomic.Int32
	lastHTTPReq atomic.Pointer[pb.HttpRequest]
}

func (f *fakePluginClient) HandleHTTP(_ context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
	f.httpCalls.Add(1)
	f.lastHTTPReq.Store(req)
	if f.hold != nil {
		<-f.hold
	}
	if f.httpErr != nil {
		return nil, f.httpErr
	}
	return f.httpResp, nil
}

func (f *fakePluginClient) Filter(_ context.Context, _ *pb.FilterRequest) (*pb.FilterResponse, error) {
	f.filterCalls.Add(1)
	if f.filterResp != nil {
		return f.filterResp, nil
	}
	return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}, nil
}

func (f *fakePluginClient) RunJob(context.Context, *pb.JobRequest) (*pb.JobResponse, error) {
	return &pb.JobResponse{Success: true}, nil
}
func (f *fakePluginClient) OnConfigChanged(context.Context, *pb.ConfigChangeEvent) error { return nil }
func (f *fakePluginClient) Shutdown(context.Context, *pb.ShutdownRequest) error          { return nil }

// liveProc satisfies the process handle a running instance needs.
type liveProc struct{}

func (liveProc) Kill()        {}
func (liveProc) Exited() bool { return false }

type fakeAuth struct{ user auth.User }

func (f fakeAuth) Resolve(string) (auth.User, bool) { return f.user, true }

func okResponse(body string) *pb.HttpResponse {
	return &pb.HttpResponse{
		StatusCode: 200,
		Headers: map[string]*pb.HeaderValues{
			"Content-Type": {Values: []string{"text/plain"}},
			// Two values on one header, to prove repeated headers survive.
			"Set-Cookie": {Values: []string{"a=1", "b=2"}},
		},
		Body: []byte(body),
	}
}

// newHarness wires a registry, one plugin, and the handler under test.
func newHarness(t *testing.T, client *fakePluginClient, filters []manifest.FilterDecl) (*PluginHandler, *pluginhost.Registry) {
	t.Helper()

	reg := pluginhost.NewRegistry()
	inst := pluginhost.NewInstance("hello", "hello-1", "1.0.0", 1, client, liveProc{})
	inst.MarkReady()

	registration := pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{inst}}
	if len(filters) > 0 {
		m := &manifest.Manifest{Key: "hello", Version: "1.0.0", Filters: filters}
		compiled, err := m.CompileFilters()
		if err != nil {
			t.Fatalf("CompileFilters: %v", err)
		}
		registration.Filters = compiled
		registration.AllowIdentityMutation = true
	}
	reg.InstallPlugin(registration)

	h := &PluginHandler{
		Registry: reg,
		Runner:   &pipeline.Runner{},
		Auth:     fakeAuth{user: auth.User{ID: 42, Username: "alice", Role: "admin"}},
	}
	return h, reg
}

func passthrough() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("core handled this"))
	})
}

// --- tests ------------------------------------------------------------------

func TestPluginRouteReachesBackend(t *testing.T) {
	client := &fakePluginClient{httpResp: okResponse("hi from plugin")}
	h, _ := newHarness(t, client, nil)

	req := httptest.NewRequest("POST", "/api/plugins/hello/items?page=2", strings.NewReader("payload"))
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hi from plugin" {
		t.Errorf("body = %q", got)
	}

	sent := client.lastHTTPReq.Load()
	if sent.GetPath() != "/items" {
		t.Errorf("plugin saw path %q, want /items (the key prefix must be stripped)", sent.GetPath())
	}
	if sent.GetQuery() != "page=2" {
		t.Errorf("plugin saw query %q", sent.GetQuery())
	}
	if string(sent.GetBody()) != "payload" {
		t.Errorf("plugin saw body %q", sent.GetBody())
	}
	if sent.GetIdentity().GetUsername() != "alice" {
		t.Errorf("plugin saw identity %v, want alice", sent.GetIdentity())
	}
	if sent.GetTraceId() == "" {
		t.Error("plugin received no trace id")
	}

	// Repeated response headers must survive; the reverse tunnel dropped all
	// but the first value.
	if got := rec.Result().Header.Values("Set-Cookie"); len(got) != 2 {
		t.Errorf("Set-Cookie = %v, want 2 values", got)
	}
}

func TestNonPluginRoutePassesThrough(t *testing.T) {
	client := &fakePluginClient{httpResp: okResponse("x")}
	h, _ := newHarness(t, client, nil)

	req := httptest.NewRequest("GET", "/system/anything", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d, want the downstream handler's 418", rec.Code)
	}
	if client.httpCalls.Load() != 0 {
		t.Error("a non-plugin path reached a plugin backend")
	}
}

// Filters are global by design: a rate limiter or WAF plugin has to see
// requests Core itself serves, not only plugin API calls.
func TestPreRouteFilterRunsOnNonPluginPaths(t *testing.T) {
	client := &fakePluginClient{
		httpResp: okResponse("x"),
		filterResp: &pb.FilterResponse{
			Action: pb.FilterResponse_ACTION_SHORT_CIRCUIT,
			ShortCircuitResponse: &pb.HttpResponse{
				StatusCode: 429,
				Body:       []byte("slow down"),
			},
		},
	}
	h, _ := newHarness(t, client, []manifest.FilterDecl{{
		Name:  "limiter",
		Phase: manifest.PhasePreRoute,
		Match: manifest.FilterMatch{Paths: []string{"/**"}},
	}})

	req := httptest.NewRequest("GET", "/some/core/page", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != 429 {
		t.Errorf("status = %d, want 429 — the filter did not intercept a core route", rec.Code)
	}
	if client.filterCalls.Load() == 0 {
		t.Error("the pre_route filter never ran")
	}
}

func TestTraceIDPropagation(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    string // empty means "any non-empty generated id"
	}{
		{
			name:    "inherits W3C traceparent",
			headers: map[string]string{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
			want:    "4bf92f3577b34da6a3ce929d0e0e4736",
		},
		{
			name:    "falls back to X-Request-Id",
			headers: map[string]string{"X-Request-Id": "req-abc"},
			want:    "req-abc",
		},
		{
			name:    "generates one when absent",
			headers: nil,
		},
		{
			name:    "ignores an all-zero traceparent",
			headers: map[string]string{"traceparent": "00-00000000000000000000000000000000-00f067aa0ba902b7-01"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakePluginClient{httpResp: okResponse("x")}
			h, _ := newHarness(t, client, nil)

			req := httptest.NewRequest("GET", "/api/plugins/hello/x", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.Middleware(passthrough()).ServeHTTP(rec, req)

			got := client.lastHTTPReq.Load().GetTraceId()
			if tc.want != "" && got != tc.want {
				t.Errorf("trace id = %q, want %q", got, tc.want)
			}
			if got == "" {
				t.Error("no trace id reached the plugin")
			}
			// The same id must come back to the caller, so a user reporting a
			// problem can quote something that appears in the logs.
			if echoed := rec.Result().Header.Get("X-Request-Id"); echoed != got {
				t.Errorf("X-Request-Id = %q, want %q", echoed, got)
			}
		})
	}
}

func TestOfflinePluginIsNotFound(t *testing.T) {
	reg := pluginhost.NewRegistry()
	h := &PluginHandler{Registry: reg, Runner: &pipeline.Runner{}}

	req := httptest.NewRequest("GET", "/api/plugins/ghost/items", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a plugin with no running replica", rec.Code)
	}
}

// A draining instance must stop receiving new requests immediately, which is
// what makes a hot upgrade safe.
//
// Two states that used to be one answer. A plugin that has finished draining
// is gone — its route no longer exists, the same way its menu does not — and a
// plugin that is still draining is between states and will be back or will
// not, but either way retrying is the right response. Both were 502 until a
// disable under load showed what that costs: 5801 callers told the upstream
// was broken by an operator switching a plugin off on purpose.
//
// This test used to claim to cover draining and did not: with nothing in
// flight the drain completed immediately, so what it actually exercised was a
// stopped instance. Both mapped to 502, so nothing said so.
func TestStoppedPluginIsNotFound(t *testing.T) {
	client := &fakePluginClient{httpResp: okResponse("x")}
	h, reg := newHarness(t, client, nil)

	inst := reg.Current().Replicas("hello")[0]
	if err := inst.Drain(context.Background(), time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := inst.State(); got != pluginhost.StateStopped {
		t.Fatalf("state = %v; this test needs a stopped instance", got)
	}

	req := httptest.NewRequest("GET", "/api/plugins/hello/items", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a plugin that is no longer serving", rec.Code)
	}
}

// A plugin that is still draining answers 503: transient, retry.
func TestDrainingPluginIsUnavailableNotBroken(t *testing.T) {
	hold := make(chan struct{})
	client := &fakePluginClient{httpResp: okResponse("x"), hold: hold}
	h, reg := newHarness(t, client, nil)

	inst := reg.Current().Replicas("hello")[0]

	// One request held inside the plugin, so the drain cannot finish.
	inFlight := make(chan struct{})
	go func() {
		defer close(inFlight)
		req := httptest.NewRequest("GET", "/api/plugins/hello/items", nil)
		h.Middleware(passthrough()).ServeHTTP(httptest.NewRecorder(), req)
	}()
	waitFor(t, func() bool { return inst.InFlight() > 0 }, "the first request to reach the plugin")

	drained := make(chan struct{})
	go func() { defer close(drained); _ = inst.Drain(context.Background(), 5*time.Second) }()
	waitFor(t, func() bool { return inst.State() == pluginhost.StateDraining }, "the drain to start")

	// A second request, arriving while the first is still in flight.
	req := httptest.NewRequest("GET", "/api/plugins/hello/items", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	close(hold)
	<-inFlight
	<-drained

	if rec.Code == http.StatusBadGateway {
		t.Error("a draining plugin answered 502; nothing is broken, it is between states")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 for a plugin that is draining", rec.Code)
	}
}

// waitFor polls a condition with a deadline.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRequestBodyLimit(t *testing.T) {
	client := &fakePluginClient{httpResp: okResponse("x")}
	h, _ := newHarness(t, client, nil)
	h.MaxBodyBytes = 32

	req := httptest.NewRequest("POST", "/api/plugins/hello/items", strings.NewReader(strings.Repeat("x", 1024)))
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if client.httpCalls.Load() != 0 {
		t.Error("an oversized request still reached the plugin")
	}
}

// A pre_handler filter may rewrite the path, and routing must honour the
// rewritten value rather than the original.
func TestPreHandlerRewriteAffectsRouting(t *testing.T) {
	client := &fakePluginClient{
		httpResp: okResponse("x"),
		filterResp: &pb.FilterResponse{
			Action: pb.FilterResponse_ACTION_MUTATE,
			Mutation: &pb.RequestMutation{
				RewritePath: "/api/plugins/hello/v2/items",
				SetRequestHeaders: map[string]*pb.HeaderValues{
					"X-Rewritten": {Values: []string{"yes"}},
				},
			},
		},
	}
	h, _ := newHarness(t, client, []manifest.FilterDecl{{
		Name:  "rewriter",
		Phase: manifest.PhasePreHandler,
		Match: manifest.FilterMatch{Paths: []string{"/api/plugins/hello/**"}},
	}})

	req := httptest.NewRequest("GET", "/api/plugins/hello/items", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	sent := client.lastHTTPReq.Load()
	if sent.GetPath() != "/v2/items" {
		t.Errorf("plugin saw path %q, want /v2/items", sent.GetPath())
	}
	if sent.GetHeaders()["X-Rewritten"].GetValues()[0] != "yes" {
		t.Error("the header a filter added did not reach the plugin")
	}
}

func TestUnauthenticatedPluginCallIsRejected(t *testing.T) {
	client := &fakePluginClient{httpResp: okResponse("x")}
	h, _ := newHarness(t, client, nil)
	h.Auth = rejectingAuth{}

	req := httptest.NewRequest("GET", "/api/plugins/hello/items", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if client.httpCalls.Load() != 0 {
		t.Error("an unauthenticated request reached the plugin")
	}
}

type rejectingAuth struct{}

func (rejectingAuth) Resolve(string) (auth.User, bool) { return auth.User{}, false }

func TestSplitPluginPath(t *testing.T) {
	tests := []struct {
		path    string
		wantKey string
		wantSub string
		wantOK  bool
	}{
		{path: "/api/plugins/hello/items", wantKey: "hello", wantSub: "/items", wantOK: true},
		{path: "/api/plugins/hello/a/b/c", wantKey: "hello", wantSub: "/a/b/c", wantOK: true},
		{path: "/api/plugins/hello", wantKey: "hello", wantSub: "/", wantOK: true},
		{path: "/api/plugins/", wantOK: false},
		{path: "/api/extensions/hello/x", wantOK: false},
		{path: "/other", wantOK: false},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			key, sub, ok := splitPluginPath(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && (key != tc.wantKey || sub != tc.wantSub) {
				t.Errorf("= (%q, %q), want (%q, %q)", key, sub, tc.wantKey, tc.wantSub)
			}
		})
	}
}

func TestBackendErrorSurfacesAsBadGateway(t *testing.T) {
	client := &fakePluginClient{httpErr: io.ErrUnexpectedEOF}
	h, _ := newHarness(t, client, nil)

	req := httptest.NewRequest("GET", "/api/plugins/hello/items", nil)
	rec := httptest.NewRecorder()
	h.Middleware(passthrough()).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

// Installing the pipeline must not break streaming responses.
//
// Wrapping a ResponseWriter hides the optional interfaces it implements, and
// losing http.Flusher turns a streaming handler into one that buffers until it
// returns. For server-sent events that means the stream never arrives — which
// is exactly how the console finds out a plugin was disabled, so the failure
// would be silent and self-concealing.
func TestStreamingSurvivesTheFilterChain(t *testing.T) {
	// A post_handler filter is what forces the response to be wrapped.
	filters := []manifest.FilterDecl{{
		Name:  "observer",
		Phase: manifest.PhasePostHandler,
		Match: manifest.FilterMatch{Paths: []string{"/**"}},
	}}

	for _, tc := range []struct {
		name    string
		filters []manifest.FilterDecl
	}{
		{name: "without filters", filters: nil},
		{name: "with a post_handler filter", filters: filters},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakePluginClient{httpResp: okResponse("x")}
			h, _ := newHarness(t, client, tc.filters)

			flushed := false
			streamer := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				f, ok := w.(http.Flusher)
				if !ok {
					t.Error("the response writer no longer implements http.Flusher")
					return
				}
				_, _ = w.Write([]byte("event: ping\n\n"))
				f.Flush()
				flushed = true
			})

			req := httptest.NewRequest("GET", "/api/system/ui/events", nil)
			rec := httptest.NewRecorder()
			h.Middleware(streamer).ServeHTTP(rec, req)

			if !flushed {
				t.Error("the streaming handler could not flush")
			}
			if !strings.Contains(rec.Body.String(), "event: ping") {
				t.Errorf("body = %q", rec.Body.String())
			}
		})
	}
}
