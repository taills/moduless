package tests

import (
	"net/http"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// The same window, on the filter side, where it is wider.
//
// callBackend resolved its instance from the snapshot the request loaded on
// arrival, and an upgrade landing in between answered 404 (fixed in
// TestRequestInFlightAcrossASwapIsNotDropped). Filters resolve the same way:
// runFilter calls res.Target(f.PluginKey) against that same snapshot.
//
// The window is not the same size, though. A pre_route filter runs immediately
// after the snapshot is loaded, so the gap is microseconds. A post_handler
// filter runs only once the backend has answered — so for a request that takes
// 200ms, any upgrade of that plugin during those 200ms lands inside the window.
//
// For a fail-open filter that costs a skipped filter. For a fail_closed one —
// which is what authentication and rate limiting are declared as, on the
// argument that failing open would be a security hole — it costs the request.
func TestFailClosedFilterSurvivesItsPluginBeingUpgraded(t *testing.T) {
	const parkFor = 400 * time.Millisecond

	backend := launchPlugin(t, "hello", "1.0.0", nil)

	// Holds the request open between the snapshot load and the post_handler
	// phase, standing in for a backend that simply takes a while.
	slow, err := launchWithEnv(t, "slow", "ECHO_FILTER_DELAY="+parkFor.String())
	if err != nil {
		t.Fatalf("launching the slow filter: %v", err)
	}
	t.Cleanup(slow.Kill)

	// The plugin that gets upgraded mid-request, guarding every request in a
	// way that refuses rather than waves through when it cannot decide.
	authV1 := launchPlugin(t, "auth", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "slow",
		Instances: []*pluginhost.Instance{slow},
		Filters: compileFilters(t, "slow", manifest.FilterDecl{
			Name:      "park",
			Phase:     manifest.PhasePreRoute,
			TimeoutMS: 2000,
			Match:     manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "auth",
		Instances: []*pluginhost.Instance{authV1},
		Filters: compileFilters(t, "auth", manifest.FilterDecl{
			Name:       "guard",
			Phase:      manifest.PhasePostHandler,
			TimeoutMS:  2000,
			FailClosed: true,
			Match:      manifest.FilterMatch{Paths: []string{"/**"}},
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

	// Upgrade the guarding plugin while the request is parked before its
	// post_handler filter has run.
	time.Sleep(parkFor / 3)
	authV2 := launchPlugin(t, "auth", "2.0.0", nil)
	reg.Swap(t.Context(),
		pluginhost.Registration{
			Key:       "auth",
			Instances: []*pluginhost.Instance{authV2},
			Filters: compileFilters(t, "auth", manifest.FilterDecl{
				Name:       "guard",
				Phase:      manifest.PhasePostHandler,
				TimeoutMS:  2000,
				FailClosed: true,
				Match:      manifest.FilterMatch{Paths: []string{"/**"}},
			}),
		},
		2*time.Second)

	select {
	case got := <-done:
		if got.took < parkFor {
			t.Fatalf("the request completed in %s, faster than the %s it was meant to be "+
				"parked for: it never spanned the upgrade",
				got.took.Round(time.Millisecond), parkFor)
		}
		if got.err != nil {
			t.Fatalf("transport error: %v", got.err)
		}
		t.Logf("in flight for %s, spanning the upgrade of the fail-closed filter's plugin",
			got.took.Round(time.Millisecond))
		if got.status != http.StatusOK {
			t.Errorf("got %d: upgrading a fail-closed filter's plugin rejected a request "+
				"that was already in flight. Declaring a filter fail_closed is a statement "+
				"about what to do when the plugin cannot decide, not consent to dropping "+
				"traffic every time that plugin is deployed", got.status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the request never completed")
	}
}
