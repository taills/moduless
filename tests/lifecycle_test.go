package tests

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// The order phases actually run in.
//
// Each phase has been tested on its own, and the ordering has been tested at
// the pipeline level with fakes. What had never been checked is the sequence a
// real request produces through real plugin processes — which is where the
// last bug in this area lived: authenticate and the 401 were each correct and
// happened in the wrong order, so the phase was unreachable and no unit test
// could have said so.
//
// These read the sequence back out of the plugin rather than inferring it from
// side effects, so what is asserted is what happened rather than what should
// have.

// lifecycleGateway installs one plugin subscribed to every phase, plus a
// backend, and returns the server.
func lifecycleGateway(t *testing.T, extra ...manifest.FilterDecl) (string, *pluginhost.Instance) {
	t.Helper()

	watcher := launchPlugin(t, "watcher", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	decls := []manifest.FilterDecl{
		phaseDecl(manifest.PhasePreRoute),
		phaseDecl(manifest.PhaseAuthenticate),
		phaseDecl(manifest.PhaseAuthorize),
		phaseDecl(manifest.PhasePreHandler),
		phaseDecl(manifest.PhasePostHandler),
		phaseDecl(manifest.PhaseOnError),
		phaseDecl(manifest.PhaseLog),
	}
	decls = append(decls, extra...)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "watcher",
		Instances: []*pluginhost.Instance{watcher},
		Filters:   compileFilters(t, "watcher", decls...),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL, watcher
}

func phaseDecl(phase string) manifest.FilterDecl {
	return manifest.FilterDecl{
		Name:  phase,
		Phase: phase,
		Match: manifest.FilterMatch{Paths: []string{"/**"}},
	}
}

// phasesSeen asks the plugin which phases it was called in for a trace.
func phasesSeen(t *testing.T, inst *pluginhost.Instance, traceID string) []string {
	t.Helper()

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/phases", Query: traceID,
	})
	if err != nil {
		t.Fatalf("asking for phases: %v", err)
	}
	body := strings.TrimSpace(string(resp.GetBody()))
	if body == "" {
		return nil
	}
	return strings.Fields(body)
}

// requestWithTrace sends a request carrying a trace id we choose, so the
// answer can be looked up afterwards.
func requestWithTrace(t *testing.T, url, path, traceID string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url+path, nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("X-Request-Id", traceID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// waitForPhases polls until the expected count arrives. The log phase runs
// after the response is written, so reading immediately would race it.
//
// Bounded by a deadline rather than an iteration count. The first version
// looped a hundred times with no pause, which is about four milliseconds of
// polling — enough on an idle machine and not enough under the full suite,
// where this failed intermittently while passing every time it was run alone.
// A flaky test is worse than none: it teaches people to re-run.
func waitForPhases(t *testing.T, inst *pluginhost.Instance, traceID string, want int) []string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		if got = phasesSeen(t, inst, traceID); len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

// waitUntilPhase waits for a named phase to be recorded.
//
// Waiting for a count instead is what a test asserting on a specific phase
// must not do, and two tests here did. The log phase runs asynchronously after
// the response is written, so waitForPhases(…, 1) returns the moment any
// earlier phase lands — and an assertion that the last phase is PHASE_LOG then
// reads a slice the log phase has not reached yet. It passes on an idle
// machine and fails under a loaded full-suite run, which reads as flake and is
// really the test asserting on something it never waited for.
func waitUntilPhase(t *testing.T, inst *pluginhost.Instance, traceID, phase string) []string {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = phasesSeen(t, inst, traceID)
		if slices.Contains(got, phase) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return got
}

// An ordinary request runs every phase, in the documented order.
func TestPhasesRunInOrder(t *testing.T) {
	url, watcher := lifecycleGateway(t)

	const trace = "lifecycle-ordinary"
	resp := requestWithTrace(t, url, "/api/plugins/hello/items", trace)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := waitForPhases(t, watcher, trace, 6)
	want := []string{
		"PHASE_PRE_ROUTE",
		"PHASE_AUTHENTICATE",
		"PHASE_AUTHORIZE",
		"PHASE_PRE_HANDLER",
		"PHASE_POST_HANDLER",
		"PHASE_LOG",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("phases ran as\n  %v\nwant\n  %v", got, want)
	}
}

// on_error does not fire when nothing went wrong. A phase that ran on every
// request would make the error handler useless, and it is the one phase whose
// absence is the correct outcome.
func TestOnErrorDoesNotFireOnSuccess(t *testing.T) {
	url, watcher := lifecycleGateway(t)

	const trace = "lifecycle-no-error"
	resp := requestWithTrace(t, url, "/api/plugins/hello/items", trace)
	resp.Body.Close()

	for _, p := range waitForPhases(t, watcher, trace, 6) {
		if p == "PHASE_ON_ERROR" {
			t.Error("the error phase ran for a request that succeeded")
		}
	}
}

// The log phase runs for a request an earlier filter refused.
//
// Both example READMEs make this claim — "被更早的 filter 短路的请求同样会被记
// 下来" — and it is what makes an audit plugin worth having: the requests most
// worth recording are the ones that were turned away.
func TestLogPhaseRunsForAShortCircuitedRequest(t *testing.T) {
	// A second plugin whose pre_route filter refuses /deny.
	blocker := launchPlugin(t, "blocker", "1.0.0", nil)
	watcher := launchPlugin(t, "watcher", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "blocker",
		Instances: []*pluginhost.Instance{blocker},
		Filters: compileFilters(t, "blocker", manifest.FilterDecl{
			Name:  "guard",
			Phase: manifest.PhasePreRoute,
			Order: 1,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "watcher",
		Instances: []*pluginhost.Instance{watcher},
		Filters: compileFilters(t, "watcher",
			phaseDecl(manifest.PhaseAuthenticate),
			phaseDecl(manifest.PhaseLog),
		),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	defer srv.Close()

	const trace = "lifecycle-denied"
	resp := requestWithTrace(t, srv.URL, "/deny", trace)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; the blocking filter did not run", resp.StatusCode)
	}

	got := waitUntilPhase(t, watcher, trace, "PHASE_LOG")
	if len(got) == 0 {
		t.Fatal("the log phase did not run for a refused request; a rejected caller " +
			"leaves no trace, which is the traffic most worth recording")
	}
	for _, p := range got {
		if p == "PHASE_AUTHENTICATE" {
			t.Error("a later phase ran after the request was refused")
		}
	}
	if got[len(got)-1] != "PHASE_LOG" {
		t.Errorf("phases = %v; want the log phase", got)
	}
}

// Filters from different plugins run in declared order within a phase.
//
// Tested at the pipeline level with fakes, never with two real processes. The
// ordering is what lets an operator put a rate limiter in front of an
// authenticator rather than behind it.
func TestOrderAcrossPluginsInOnePhase(t *testing.T) {
	first := launchPlugin(t, "first", "1.0.0", nil)
	second := launchPlugin(t, "second", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	// Installed in the wrong order on purpose: the declared order is what must
	// decide, not the order they happened to be registered in.
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "second",
		Instances: []*pluginhost.Instance{second},
		Filters: compileFilters(t, "second", manifest.FilterDecl{
			Name: "late", Phase: manifest.PhasePreRoute, Order: 90,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "first",
		Instances: []*pluginhost.Instance{first},
		Filters: compileFilters(t, "first", manifest.FilterDecl{
			Name: "early", Phase: manifest.PhasePreRoute, Order: 10,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	defer srv.Close()

	// The low-order plugin refuses, so the high-order one must never be
	// reached. Observable from outside, which an ordering of two Continues is
	// not.
	const trace = "lifecycle-order"
	resp := requestWithTrace(t, srv.URL, "/deny", trace)
	resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	if got := waitForPhases(t, second, trace, 0); len(got) != 0 {
		t.Errorf("the order-90 filter ran (%v) although the order-10 filter refused the "+
			"request; declared order is not deciding", got)
	}
	if got := phasesSeen(t, first, trace); len(got) == 0 {
		t.Error("the order-10 filter did not run at all; this test is measuring nothing")
	}
}

// on_error fires when the backend fails.
//
// The other half of the pair. A phase that never runs would pass the
// success-path test above on its own, and an error handler that is never
// called is worse than none: it looks like coverage.
func TestOnErrorFiresWhenTheBackendFails(t *testing.T) {
	watcher := launchPlugin(t, "watcher", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "watcher",
		Instances: []*pluginhost.Instance{watcher},
		Filters: compileFilters(t, "watcher",
			phaseDecl(manifest.PhaseOnError),
			phaseDecl(manifest.PhaseLog),
		),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	defer srv.Close()

	// Establish that it works first, or a 502 proves only that the test broke
	// something before the request.
	ok := requestWithTrace(t, srv.URL, "/api/plugins/hello/items", "lifecycle-alive")
	ok.Body.Close()
	if ok.StatusCode != http.StatusOK {
		t.Fatalf("the backend was not serving before it was killed: %d", ok.StatusCode)
	}

	// The backend goes away mid-flight, which is what a crash looks like from
	// the gateway's side.
	backend.Kill()

	const trace = "lifecycle-error"
	resp := requestWithTrace(t, srv.URL, "/api/plugins/hello/items", trace)
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("the request succeeded against a dead backend")
	}
	t.Logf("status with a dead backend: %d", resp.StatusCode)

	got := waitUntilPhase(t, watcher, trace, "PHASE_LOG")
	var sawError bool
	for _, p := range got {
		if p == "PHASE_ON_ERROR" {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("phases = %v; the error phase did not run for a failed backend, so an "+
			"on_error filter is decoration", got)
	}
	if len(got) == 0 || got[len(got)-1] != "PHASE_LOG" {
		t.Errorf("phases = %v; the log phase must still run for a failed request", got)
	}
}

// Two filters in one phase are told apart.
//
// The manifest permits it — only names must be unique, nothing forbids two
// pre_route declarations with different path rules — and the SDK dispatches by
// phase, so both land on the same Go function. Until the request carried the
// declaration's name, that function could not tell which one matched. Deriving
// it from the path duplicates the matching Core already did, and is impossible
// outright when both declarations match the same path.
//
// Found by an author who could read only the plugin guide, and who correctly
// reported it as a blank rather than guessing.
func TestFilterKnowsWhichDeclarationMatched(t *testing.T) {
	watcher := launchPlugin(t, "watcher", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "watcher",
		Instances: []*pluginhost.Instance{watcher},
		Filters: compileFilters(t, "watcher",
			manifest.FilterDecl{
				Name: "broad", Phase: manifest.PhasePreRoute, Order: 10,
				Match: manifest.FilterMatch{Paths: []string{"/**"}},
			},
			// Both match the same request. That is the case no amount of
			// path inspection can disambiguate.
			manifest.FilterDecl{
				Name: "narrow", Phase: manifest.PhasePreRoute, Order: 20,
				Match: manifest.FilterMatch{Paths: []string{"/api/plugins/hello/**"}},
			},
		),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	defer srv.Close()

	resp := requestWithTrace(t, srv.URL, "/api/plugins/hello/items", "which-decl")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// Both declarations matched, so the plugin was called twice in this phase.
	got := waitForPhases(t, watcher, "which-decl", 2)
	if len(got) < 2 {
		t.Fatalf("the plugin was called %d time(s); both declarations match this path", len(got))
	}

	// And the names arrived. Read from the fixture, which records them.
	names := filterNamesSeen(t, watcher, "which-decl")
	if len(names) != 2 || names[0] != "broad" || names[1] != "narrow" {
		t.Errorf("filter names = %v; want [broad narrow] in declared order — without them "+
			"one function receives two calls it cannot tell apart", names)
	}
}

func filterNamesSeen(t *testing.T, inst *pluginhost.Instance, traceID string) []string {
	t.Helper()

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/filter-names", Query: traceID,
	})
	if err != nil {
		t.Fatalf("asking for filter names: %v", err)
	}
	body := strings.TrimSpace(string(resp.GetBody()))
	if body == "" {
		return nil
	}
	return strings.Fields(body)
}
