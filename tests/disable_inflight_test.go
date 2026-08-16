package tests

import (
	"net/http"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// A request already in flight when a plugin is disabled.
//
// Disable removes the plugin from the snapshot before draining, precisely so
// that no new request can reach it — so a request arriving afterwards gets a
// clean 404. A request that arrived *before* still carries the old snapshot,
// and what it is told is decided in callBackend, from a flag computed against
// that stale snapshot rather than against the registry as it stands now.
//
// The difference matters to the caller. 404 says the route is not there and
// will not be; 503 says try again shortly. For a plugin an operator switched
// off, "try again" is false — it is not coming back until somebody enables it.
// TestDisableUnderLoad asserts exactly this and catches it only under load,
// because the window is the width of one request.
func TestARequestSpanningADisableIsToldItIsGone(t *testing.T) {
	const parkFor = 400 * time.Millisecond

	backend := launchPlugin(t, "hello", "1.0.0", nil)
	slow, err := launchWithEnv(t, "slow", "ECHO_FILTER_DELAY="+parkFor.String())
	if err != nil {
		t.Fatalf("launching the slow filter: %v", err)
	}
	t.Cleanup(slow.Kill)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "slow",
		Instances: []*pluginhost.Instance{slow},
		Filters: compileFilters(t, "slow", manifest.FilterDecl{
			Name: "park", Phase: manifest.PhasePreRoute, TimeoutMS: 2000,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})

	srv := newGateway(reg)
	defer srv.Close()

	type result struct {
		status int
		took   time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		resp, err := http.Get(srv.URL + "/api/plugins/hello/items")
		if err != nil {
			done <- result{status: -1, took: time.Since(start)}
			return
		}
		defer resp.Body.Close()
		done <- result{status: resp.StatusCode, took: time.Since(start)}
	}()

	// Hold the drain open. Core drains by waiting for in-flight requests, so
	// without one the instance goes from Ready straight to Stopped and the
	// parked request simply finds nothing — a 404 for the wrong reason. The
	// window this is about is the one where the instance is still Draining,
	// and it exists exactly as long as somebody is inside it.
	holding := make(chan struct{})
	go func() {
		defer close(holding)
		resp, err := http.Get(srv.URL + "/api/plugins/hello/slow")
		if err == nil {
			resp.Body.Close()
		}
	}()
	time.Sleep(parkFor / 3)

	// Disable while one request is parked in the filter and another is inside
	// the backend.
	for _, inst := range reg.Remove("hello") {
		go inst.Drain(t.Context(), 2*time.Second)
	}

	got := <-done
	<-holding
	if got.took < parkFor {
		t.Fatalf("the request finished in %s, faster than the %s it was meant to be "+
			"parked for; it never spanned the disable", got.took, parkFor)
	}
	t.Logf("in flight for %s across a disable, answered %d",
		got.took.Round(10*time.Millisecond), got.status)

	if got.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404. The registry has nothing for this plugin any "+
			"more, so whether it is 'gone' or 'draining' is a question about now — and "+
			"answering 503 tells a caller to retry something that is not coming back "+
			"until an operator enables it again", got.status)
	}
}
