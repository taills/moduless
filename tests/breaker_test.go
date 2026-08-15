package tests

import (
	"net/http"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// Failure policy, end to end.
//
// Note: these observe the breaker with Breaker.Open(), never with Allow().
// Allow consumes a half-open probe slot, so using it to check state is not a
// check — it changes the thing being checked, and a test that polls with it
// starves the probe it is waiting for.
//
// Two of the framework's promises only matter when something is broken: a
// filter that fails must either let traffic through or stop it, according to
// what its manifest declared, and a filter that keeps failing must stop being
// called at all. Both are easy to believe and easy to get wrong, because
// nothing exercises them until the day they matter.

// failingFilterGateway installs one filter that errors on a known path.
func failingFilterGateway(t *testing.T, failClosed bool) (url string, inst *pluginhost.Instance, cleanup func()) {
	t.Helper()

	inst = launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "hello", manifest.FilterDecl{
			Name:       "guard",
			Phase:      manifest.PhasePreRoute,
			Match:      manifest.FilterMatch{Paths: []string{"/**"}},
			FailClosed: failClosed,
		}),
	})

	srv := newGateway(reg)
	return srv.URL, inst, srv.Close
}

// The default. A filter that fails must not take the site down with it: a
// broken rate limiter or audit logger is a degraded system, not an outage.
func TestFilterFailOpenLetsTrafficThrough(t *testing.T) {
	url, _, cleanup := failingFilterGateway(t, false)
	defer cleanup()

	// The fixture returns an error from Filter for exactly this path.
	status, _, _ := get(t, url+"/boom")
	t.Logf("a failing fail-open filter answered %d", status)

	// 404 is Core's own handler: the request got past the filter and was
	// routed normally.
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want the request to proceed to Core's handler (404); "+
			"a fail-open filter blocked traffic when it errored", status)
	}
}

// The opt-in. A filter enforcing a security decision must fail closed, and
// accept that its own outage becomes the site's outage — otherwise an outage
// silently becomes a bypass.
func TestFilterFailClosedRejects(t *testing.T) {
	url, _, cleanup := failingFilterGateway(t, true)
	defer cleanup()

	status, body, _ := get(t, url+"/boom")
	t.Logf("a failing fail-closed filter answered %d: %s", status, body)

	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; a fail_closed filter that errored let the request through, "+
			"which turns an outage into a bypass", status)
	}
}

// A filter that keeps failing must stop being consulted. Every call to a
// broken plugin costs a cross-process round trip and adds latency to a request
// that is going to proceed anyway.
func TestFilterCircuitBreakerOpens(t *testing.T) {
	url, inst, cleanup := failingFilterGateway(t, false)
	defer cleanup()

	if inst.Breaker.Open() {
		t.Fatal("the breaker was open before anything failed")
	}

	// Drive failures until the breaker trips.
	var calls int
	deadline := time.Now().Add(10 * time.Second)
	for !inst.Breaker.Open() && time.Now().Before(deadline) {
		calls++
		if status, _, _ := get(t, url+"/boom"); status != http.StatusNotFound {
			t.Fatalf("request %d answered %d; fail-open should have let it through", calls, status)
		}
	}

	t.Logf("the breaker opened after %d failing requests", calls)
	if !inst.Breaker.Open() {
		t.Fatal("the breaker never opened; a broken filter is consulted forever")
	}

	// With the breaker open, traffic must still flow — the filter is skipped,
	// not the request.
	for range 5 {
		if status, _, _ := get(t, url+"/anything"); status != http.StatusNotFound {
			t.Errorf("status = %d with the breaker open; opening the breaker must skip the filter, not the request", status)
		}
	}
}

// An open breaker on a fail-closed filter must reject rather than allow. This
// is the combination that would be easiest to get backwards, and getting it
// backwards means a security filter stops being enforced precisely when it is
// failing.
func TestFailClosedBreakerRejects(t *testing.T) {
	url, inst, cleanup := failingFilterGateway(t, true)
	defer cleanup()

	deadline := time.Now().Add(10 * time.Second)
	for !inst.Breaker.Open() && time.Now().Before(deadline) {
		get(t, url+"/boom")
	}
	if !inst.Breaker.Open() {
		t.Fatal("the breaker never opened")
	}

	// Any path now, not just the failing one: the plugin is considered
	// unhealthy, and this filter matches everything.
	status, _, _ := get(t, url+"/some-other-path")
	t.Logf("fail-closed filter with an open breaker answered %d", status)
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; an unhealthy fail-closed filter must reject, "+
			"otherwise a security filter stops being enforced exactly when it is broken", status)
	}
}

// A plugin's breaker must not affect a different plugin. They fail
// independently, and one broken plugin taking out another's filters would make
// the blast radius of any single bug the whole system.
func TestBreakerIsPerPlugin(t *testing.T) {
	broken := launchPlugin(t, "broken", "1.0.0", nil)
	healthy := launchPlugin(t, "healthy", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	// The two plugins subscribe to different paths. Both run the same fixture
	// binary, so a shared "/**" subscription would send the failing path to
	// both and break both — which would prove nothing about isolation.
	for _, p := range []struct {
		key   string
		inst  *pluginhost.Instance
		paths []string
	}{
		{"broken", broken, []string{"/boom"}},
		{"healthy", healthy, []string{"/deny"}},
	} {
		reg.InstallPlugin(pluginhost.Registration{
			Key:       p.key,
			Instances: []*pluginhost.Instance{p.inst},
			Filters: compileFilters(t, p.key, manifest.FilterDecl{
				Name:  "guard",
				Phase: manifest.PhasePreRoute,
				Match: manifest.FilterMatch{Paths: p.paths},
			}),
		})
	}

	srv := newGateway(reg)
	defer srv.Close()

	deadline := time.Now().Add(10 * time.Second)
	for !broken.Breaker.Open() && time.Now().Before(deadline) {
		get(t, srv.URL+"/boom")
	}
	if !broken.Breaker.Open() {
		t.Fatal("the breaker never opened")
	}

	if healthy.Breaker.Open() {
		t.Error("one plugin's breaker opened another plugin's; a single bug should not take out unrelated filters")
	}

	// The healthy plugin's filter still works: it short-circuits /deny.
	status, body, _ := get(t, srv.URL+"/deny")
	if status != http.StatusForbidden {
		t.Errorf("the healthy plugin's filter answered %d, want its 403: %s", status, body)
	}
}

// The half of the breaker that matters after the incident: a plugin that
// recovers must get its traffic back.
//
// Opening is the easy direction and the one already covered. If closing never
// happened, a plugin that hiccupped five times would have its filters skipped
// for the rest of the process's life — a momentary fault turned permanent, and
// silently, because a fail-open filter that is being skipped looks exactly
// like one that is passing everything.
//
// The observable is the filter's own effect, not the breaker's state: the
// fixture short-circuits /deny with a 403, so a 403 means the filter ran and a
// 404 means it was skipped and the request fell through to Core.
func TestBreakerClosesAndTheFilterWorksAgain(t *testing.T) {
	url, inst, cleanup := failingFilterGateway(t, false)
	defer cleanup()

	// Healthy to begin with: the filter runs and stops /deny.
	if status, _, _ := get(t, url+"/deny"); status != http.StatusForbidden {
		t.Fatalf("status = %d before any failure; the filter was not running", status)
	}

	// Drive it to failure.
	deadline := time.Now().Add(10 * time.Second)
	for !inst.Breaker.Open() && time.Now().Before(deadline) {
		get(t, url+"/boom")
	}
	if !inst.Breaker.Open() {
		t.Fatal("the breaker never opened")
	}

	// While open, the filter is skipped — /deny is no longer stopped.
	if status, _, _ := get(t, url+"/deny"); status != http.StatusNotFound {
		t.Errorf("status = %d with the breaker open, want the filter to be skipped (404)", status)
	}

	// Wait out the open period. The real five seconds rather than a fake
	// clock, because what is under test is a plugin coming back on its own
	// with nothing intervening.
	//
	// Open() going false means only that the breaker has stopped refusing
	// outright — it cannot distinguish half-open from closed, and from a
	// caller's side that distinction is invisible anyway: a probe that
	// succeeds runs the filter just like a closed breaker would. So this is
	// the wait, not the assertion.
	deadline = time.Now().Add(30 * time.Second)
	for inst.Breaker.Open() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if inst.Breaker.Open() {
		t.Fatal("the breaker was still refusing every call well past its open period")
	}

	// The assertion is the filter's behaviour: it stops /deny again. That is
	// what a recovered plugin means to everything outside it, and it is what a
	// breaker that never let go would prevent.
	recovered := false
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if status, _, _ := get(t, url+"/deny"); status == http.StatusForbidden {
			recovered = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !recovered {
		t.Error("the breaker closed but the filter is still not stopping /deny; " +
			"traffic is being allowed past a filter that is once again healthy")
	}
}

// A plugin that stays broken must not be let back in just because time passed.
// The probe is an opportunity to prove recovery, not a reset.
func TestBreakerReopensIfTheProbeFails(t *testing.T) {
	url, inst, cleanup := failingFilterGateway(t, false)
	defer cleanup()

	deadline := time.Now().Add(10 * time.Second)
	for !inst.Breaker.Open() && time.Now().Before(deadline) {
		get(t, url+"/boom")
	}
	if !inst.Breaker.Open() {
		t.Fatal("the breaker never opened")
	}

	// Keep failing across the open period, so every probe fails too.
	end := time.Now().Add(8 * time.Second)
	for time.Now().Before(end) {
		get(t, url+"/boom")
		time.Sleep(100 * time.Millisecond)
	}

	if !inst.Breaker.Open() {
		t.Error("the breaker closed for a plugin that never stopped failing; " +
			"the half-open probe is resetting on time rather than on recovery")
	}
}
