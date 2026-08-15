package tests

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// A request that is still inside a filter when its plugin is upgraded.
//
// This is the gap the zero-downtime upgrade test cannot see. Its requests take
// microseconds, so they are always entirely before or entirely after the swap.
// A request that spans one is a different thing: it has already been admitted,
// it has already run, and it then has to reach a backend on an instance that
// has meanwhile been taken out of rotation.
//
// It used to get a 502.
func TestRequestInFilterDuringUpgrade(t *testing.T) {
	reg := pluginhost.NewRegistry()
	v1 := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{v1},
		Filters: compileFilters(t, "hello", manifest.FilterDecl{
			Name:  "slow",
			Phase: manifest.PhasePreRoute,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
			// Core's default filter timeout is 50ms, which would cut the
			// fixture's deliberate delay short and end the request before the
			// swap — leaving the scenario silently not happening.
			TimeoutMS: 5000,
		}),
	})

	srv := newGateway(reg)
	defer srv.Close()

	type result struct {
		status   int
		instance string
		err      error
	}
	done := make(chan result, 1)
	requestStart := time.Now()
	go func() {
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Get(srv.URL + "/api/plugins/hello/slow-filter")
		if err != nil {
			done <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- result{status: resp.StatusCode, instance: resp.Header.Get("X-Instance")}
	}()

	// Launch the replacement first: starting a process takes tens of
	// milliseconds, which would otherwise be spent inside the filter's window
	// and put the swap after it rather than during it.
	v2 := launchPlugin(t, "hello", "2.0.0", nil)
	time.Sleep(60 * time.Millisecond)
	reg.Swap(context.Background(),
		pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v2}},
		5*time.Second)

	got := <-done
	elapsed := time.Since(requestStart)

	// Without this the test is theatre. Three earlier versions of it passed
	// while exercising nothing: once because the filter matched no path, once
	// because the 50ms default timeout cut the delay short, and once because
	// launching the replacement used up the window before the swap happened.
	// Each time the request simply finished before the swap and the assertion
	// below was met for the wrong reason.
	if elapsed < 150*time.Millisecond {
		t.Fatalf("the request took only %s, so it did not span the swap; the scenario did not happen", elapsed)
	}
	t.Logf("request spanned the swap (%s): status=%d instance=%q",
		elapsed.Round(time.Millisecond), got.status, got.instance)

	if got.err != nil {
		t.Fatalf("transport error: %v", got.err)
	}
	if got.status != 200 {
		t.Errorf("a request that was inside a filter when its plugin was upgraded returned %d; "+
			"it had already been admitted and should have been served", got.status)
	}
	if got.instance != "hello-1.0.0" {
		t.Errorf("the request was served by %q; one admitted before the swap should finish on "+
			"the instance it started on", got.instance)
	}

	// And the old instance still goes away once its work is done — draining
	// waits, it does not give up.
	deadline := time.Now().Add(10 * time.Second)
	for !v1.ProcessExited() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !v1.ProcessExited() {
		t.Error("the old instance was never stopped after its last request finished")
	}
}

// New traffic must not reach a draining instance. Serving requests that were
// admitted before a swap is the point of the change above; continuing to
// accept new ones would mean the upgrade never actually took effect.
func TestNewRequestsGoToTheNewInstance(t *testing.T) {
	reg := pluginhost.NewRegistry()
	v1 := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v1}})

	srv := newGateway(reg)
	defer srv.Close()

	client := warmClientPlain()
	if got := instanceServing(t, client, srv.URL); got != "hello-1.0.0" {
		t.Fatalf("before the swap the request was served by %q", got)
	}

	v2 := launchPlugin(t, "hello", "2.0.0", nil)
	reg.Swap(context.Background(),
		pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v2}},
		5*time.Second)

	// Every request after the swap belongs to the new instance.
	for i := range 20 {
		if got := instanceServing(t, client, srv.URL); got != "hello-2.0.0" {
			t.Fatalf("request %d after the swap was served by %q, want the new instance", i, got)
		}
	}
}

func instanceServing(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	resp, err := client.Get(baseURL + "/api/plugins/hello/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	return resp.Header.Get("X-Instance")
}
