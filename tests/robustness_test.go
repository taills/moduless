package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
)

// Robustness tests: what happens when a plugin behaves badly.
//
// Plugins are trusted code, but trusted code has bugs. A plugin that hangs,
// panics, returns a nonsense status or floods the connection must not be able
// to take Core down with it — that is the whole reason plugins are separate
// processes rather than linked in.

func robustGateway(t *testing.T) (url string, reg *pluginhost.Registry, cleanup func()) {
	t.Helper()
	return robustGatewayTimeout(t, 0)
}

// robustGatewayTimeout builds a gateway whose backend calls give up after the
// given duration. Zero uses the production default.
func robustGatewayTimeout(t *testing.T, backendTimeout time.Duration) (string, *pluginhost.Registry, func()) {
	t.Helper()

	reg := pluginhost.NewRegistry()
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{inst}})

	srv := newGatewayWithTimeout(reg, backendTimeout)
	return srv.URL, reg, srv.Close
}

// A wedged plugin must time out rather than holding the connection open until
// the client gives up, and Core must keep serving everything else.
func TestRobustHungPluginTimesOut(t *testing.T) {
	// Core gives up on the backend after 2s, well inside the client's budget,
	// so the test observes Core's timeout rather than the client's.
	url, _, cleanup := robustGatewayTimeout(t, 2*time.Second)
	defer cleanup()

	client := &http.Client{Timeout: 20 * time.Second}

	start := time.Now()
	resp, err := client.Get(url + "/api/plugins/hello/hang")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("the request failed at the transport layer rather than timing out cleanly: %v", err)
	}
	defer resp.Body.Close()

	t.Logf("hung plugin answered %d after %s", resp.StatusCode, elapsed.Round(time.Millisecond))
	if resp.StatusCode == 200 {
		t.Error("a hung plugin produced a 200")
	}
	if elapsed > 10*time.Second {
		t.Errorf("took %s to give up on a hung plugin; the backend timeout did not fire", elapsed)
	}

	// Core must still be serving.
	if !coreHealthy(t, url) {
		t.Error("Core stopped serving after a hung plugin")
	}
}

// A plugin returning a status code outside the valid range must not reach
// net/http, which panics on one.
func TestRobustInvalidStatusCode(t *testing.T) {
	url, _, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(url + "/api/plugins/hello/badstatus")
	if err != nil {
		t.Fatalf("GET: %v — an invalid status probably panicked the handler", err)
	}
	defer resp.Body.Close()

	t.Logf("plugin returned status 9999, gateway answered %d", resp.StatusCode)
	if resp.StatusCode < 100 || resp.StatusCode > 599 {
		t.Errorf("gateway passed through an invalid status code: %d", resp.StatusCode)
	}

	if !coreHealthy(t, url) {
		t.Error("Core stopped serving after an invalid status code")
	}
}

// A plugin that never set a status should be treated as 200 rather than
// producing a zero status.
func TestRobustZeroStatusBecomesOK(t *testing.T) {
	url, _, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url + "/api/plugins/hello/zerostatus")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want an unset status to become 200", resp.StatusCode)
	}
}

// A plugin returning a response larger than the transport ceiling must fail
// that one request rather than killing the connection for everyone.
func TestRobustOversizedResponse(t *testing.T) {
	url, _, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{Timeout: 30 * time.Second}

	resp, err := client.Get(url + "/api/plugins/hello/huge")
	if err != nil {
		t.Logf("oversized response failed at the transport layer: %v", err)
	} else {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("oversized response: status %d, %d bytes returned", resp.StatusCode, len(body))
	}

	// Whatever happened to that request, the connection and Core must survive
	// and the next request must work.
	if code := getStatus(t, client, url+"/api/plugins/hello/items"); code != 200 {
		t.Errorf("a normal request after an oversized one returned %d", code)
	}
}

// A panic inside the plugin kills that process. Core must notice, keep
// serving, and report the plugin as unavailable rather than hanging.
func TestRobustPluginPanicIsContained(t *testing.T) {
	url, _, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Get(url + "/api/plugins/hello/panic")
	if err != nil {
		t.Logf("panicking plugin: transport error %v", err)
	} else {
		t.Logf("panicking plugin answered %d", resp.StatusCode)
		resp.Body.Close()
	}

	// Core stays up.
	if !coreHealthy(t, url) {
		t.Fatal("Core went down with the plugin")
	}

	// Subsequent requests to the dead plugin fail cleanly rather than hanging.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		code := getStatus(t, client, url+"/api/plugins/hello/items")
		if code == http.StatusBadGateway {
			return // reported unavailable, which is correct
		}
		if code == 200 {
			t.Log("plugin still answering; the panic did not kill the process")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("requests to the dead plugin neither succeeded nor reported unavailable")
}

// Disabling a plugin while requests are in flight must not lose those
// requests: they were admitted before the swap, so they finish.
func TestRobustDisableUnderLoad(t *testing.T) {
	url, reg, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{Timeout: 15 * time.Second}

	var (
		stop      atomic.Bool
		attempts  atomic.Int64
		succeeded atomic.Int64
		wg        sync.WaitGroup
	)
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				attempts.Add(1)
				resp, err := client.Get(url + "/api/plugins/hello/items")
				if err != nil {
					continue
				}
				if resp.StatusCode == 200 {
					succeeded.Add(1)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}

	time.Sleep(150 * time.Millisecond)
	displaced := reg.Remove("hello")
	for _, inst := range displaced {
		if err := inst.Drain(context.Background(), 5*time.Second); err != nil {
			t.Errorf("drain reported %v; in-flight requests may have been cut off", err)
		}
	}
	time.Sleep(100 * time.Millisecond)

	stop.Store(true)
	wg.Wait()

	t.Logf("%d/%d requests succeeded across a disable", succeeded.Load(), attempts.Load())
	if succeeded.Load() == 0 {
		t.Error("no request succeeded; the load generator never reached the plugin")
	}
	// Core is still healthy after the plugin went away.
	if !coreHealthy(t, url) {
		t.Error("Core is unhealthy after a disable under load")
	}
}

// Requests arriving for a plugin that was never installed must 502 promptly
// rather than hanging or panicking.
func TestRobustUnknownPluginFailsFast(t *testing.T) {
	url, _, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	start := time.Now()
	code := getStatus(t, client, url+"/api/plugins/does-not-exist/anything")
	elapsed := time.Since(start)

	// A plugin that was never installed has no route, so 404 — the same
	// answer any other unrouted path gets. It was 502 until a disable under
	// load showed what conflating "not here" with "broken" costs.
	if code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a plugin that does not exist", code)
	}
	if elapsed > time.Second {
		t.Errorf("took %s to reject an unknown plugin; it should be immediate", elapsed)
	}
}

// Malformed request paths must not confuse routing.
func TestRobustMalformedPaths(t *testing.T) {
	url, _, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{Timeout: 5 * time.Second}

	paths := []string{
		"/api/plugins/",
		"/api/plugins//items",
		"/api/plugins/hello",
		"/api/plugins/../../etc/passwd",
		"/api/plugins/hello/%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"/api/plugins/" + strings.Repeat("x", 2000),
	}

	for _, p := range paths {
		t.Run(p[:min(len(p), 40)], func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, url+p, nil)
			if err != nil {
				t.Skipf("client refused to build the request: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Logf("transport rejected it: %v", err)
				return
			}
			defer resp.Body.Close()
			t.Logf("answered %d, plugin saw path %q", resp.StatusCode, resp.Header.Get("X-Echo-Path"))
			if resp.StatusCode >= 500 && resp.StatusCode != http.StatusBadGateway {
				t.Errorf("a malformed path produced %d; it should be a 4xx or a clean 502", resp.StatusCode)
			}
			// Whatever the plugin was handed must not contain a traversal.
			// Plugins are trusted, but a plugin that joins this onto a
			// directory would be walking out of it.
			if seen := resp.Header.Get("X-Echo-Path"); strings.Contains(seen, "..") {
				t.Errorf("plugin was handed a traversal path %q; Core should have cleaned it", seen)
			}
		})
	}

	// Core survived all of it.
	if !coreHealthy(t, url) {
		t.Error("Core is unhealthy after malformed paths")
	}
}

// Many concurrent requests against one plugin process must not deadlock or
// interleave responses. Each response carries the path it was for, so a
// mismatch would show cross-talk between concurrent calls.
func TestRobustConcurrentRequestsDoNotCrossTalk(t *testing.T) {
	url, _, cleanup := robustGateway(t)
	defer cleanup()

	client := &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 64, MaxConnsPerHost: 64},
	}

	var (
		wg        sync.WaitGroup
		mismatch  atomic.Int64
		completed atomic.Int64
	)
	for i := range 200 {
		wg.Go(func() {
			path := fmt.Sprintf("/api/plugins/hello/item-%d", i)
			resp, err := client.Get(url + path)
			if err != nil {
				return
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)

			// The fixture echoes the path it saw.
			want := fmt.Sprintf("/item-%d", i)
			if got := resp.Header.Get("X-Echo-Path"); got != want {
				mismatch.Add(1)
				t.Errorf("response for %s carried path %q", want, got)
			}
			completed.Add(1)
		})
	}
	wg.Wait()

	t.Logf("%d/200 concurrent requests completed", completed.Load())
	if mismatch.Load() > 0 {
		t.Errorf("%d responses were matched to the wrong request", mismatch.Load())
	}
	if completed.Load() < 200 {
		t.Errorf("only %d of 200 concurrent requests completed", completed.Load())
	}
}

func getStatus(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// coreHealthy checks that Core is still accepting and answering HTTP.
//
// It asks over a brand-new connection: reusing the load generator's client
// would conflate two different failures — Core being down, and the pooled
// connection having been broken by whatever the test just did to it. Only the
// first one is interesting here.
//
// Any HTTP response counts as alive. The test stack's own handler answers 404
// to everything, so requiring a 200 would report a perfectly healthy Core as
// dead; what matters is that the process is still serving rather than panicked
// or wedged.
func coreHealthy(t *testing.T, url string) bool {
	t.Helper()
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	resp, err := client.Get(url + "/is-core-alive")
	if err != nil {
		t.Logf("health check failed: %v", err)
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return true
}
