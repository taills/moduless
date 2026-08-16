package tests

import (
	"net/http"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// A request that is already in flight when an upgrade commits.
//
// TestE2EHotUpgradeServesContinuously failed once under a loaded full-suite run
// and passed every time it was run alone, which is the signature of a window
// too narrow to hit reliably rather than of a test that is merely flaky. The
// window is visible in callBackend: a request picks its instance from the
// snapshot it loaded at the start, and admits into that instance later. If the
// swap commits in between, the instance it picked is draining by the time it
// asks, and the request is refused.
//
// The comment there already anticipates this — it takes the request's own
// admission "rather than a fresh one" precisely so that a filter's earlier
// reservation carries through. But that only helps a plugin that ran a filter
// on this path. A plugin reached by a plain route, which is the ordinary case,
// has no earlier reservation and meets the race unguarded.
//
// Rather than hammer and hope, this holds a request open across the swap: a
// slow pre_route filter on a different plugin keeps it parked on the old
// snapshot while the upgrade commits underneath it.

func TestRequestInFlightAcrossASwapIsNotDropped(t *testing.T) {
	const filterDelay = 400 * time.Millisecond

	// The parked request's own plugin. It runs no filters, so it holds no
	// admission before callBackend asks for one.
	v1 := launchPlugin(t, "hello", "1.0.0", nil)

	// A filter belonging to someone else, slow enough to hold the request on
	// its snapshot while the swap lands.
	guard, err := launchWithEnv(t, "guard", "ECHO_FILTER_DELAY="+filterDelay.String())
	if err != nil {
		t.Fatalf("launching the slow filter: %v", err)
	}
	t.Cleanup(func() { guard.Kill() })

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{v1},
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "guard",
		Instances: []*pluginhost.Instance{guard},
		Filters: compileFilters(t, "guard", manifest.FilterDecl{
			Name: "slow",
			// Without a budget of its own the filter hits the default timeout
			// and fails open, and the request sails past in ~50ms having
			// parked for nothing.
			TimeoutMS: 2000,
			Phase:     manifest.PhasePreRoute,
			Match:     manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})

	srv := newGateway(reg)
	defer srv.Close()

	type result struct {
		status int
		took   time.Duration
		err    error
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		resp, err := http.Get(srv.URL + "/api/plugins/hello/items")
		if err != nil {
			done <- result{err: err, took: time.Since(start)}
			return
		}
		defer resp.Body.Close()
		done <- result{status: resp.StatusCode, took: time.Since(start)}
	}()

	// Let the request get past the snapshot load and into the slow filter,
	// then commit the upgrade while it is parked there.
	time.Sleep(filterDelay / 3)
	v2 := launchPlugin(t, "hello", "2.0.0", nil)
	reg.Swap(t.Context(),
		pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v2}},
		2*time.Second)

	select {
	case got := <-done:
		// The whole point is that the request spans the swap. If it finished
		// before the filter's own delay elapsed, it was never parked and a PASS
		// here would mean nothing.
		if got.took < filterDelay {
			t.Fatalf("the request completed in %s, faster than the %s filter delay: it "+
				"never spanned the swap, so this test proved nothing",
				got.took.Round(time.Millisecond), filterDelay)
		}
		if got.err != nil {
			t.Fatalf("the request failed at the transport layer during the swap: %v", got.err)
		}
		t.Logf("the request was in flight for %s, spanning the swap", got.took.Round(time.Millisecond))
		if got.status != http.StatusOK {
			t.Errorf("a request already in flight when the upgrade committed got %d; "+
				"zero-downtime has to cover the requests that span the swap, which are "+
				"the only ones an upgrade can possibly disturb", got.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parked request never completed")
	}
}
