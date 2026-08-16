package tests

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
)

// What Core does while a plugin is slow and traffic keeps arriving.
//
// A backend call is bounded at 30 seconds by default, and nothing bounds how
// many of them may be outstanding to one plugin at once — BeginRequest counts
// but does not cap, unlike the transaction ceiling next door. So the in-flight
// count is arrival rate times latency, and each one holds a goroutine in Core,
// a gRPC call, and a goroutine in the plugin.
//
// Whether that is a problem is an empirical question and it had never been
// asked. The failure it would produce is the classic one: a slow dependency
// consuming the caller's capacity until the caller falls over too, which is
// exactly what a process-per-plugin architecture is supposed to prevent.
//
//	MEASURE=1 go test ./tests/ -run TestSlowBackendUnderLoad -v

// slowBackendStack serves a plugin whose handler sleeps 150ms.
func slowBackendStack(t *testing.T) (url string, inst *pluginhost.Instance) {
	t.Helper()

	inst = launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
	})
	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL, inst
}

func TestSlowBackendUnderLoad(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE is not set")
	}

	url, inst := slowBackendStack(t)

	var (
		stop      atomic.Bool
		completed atomic.Int64
		failed    atomic.Int64
		wg        sync.WaitGroup
	)
	// Far more concurrent callers than the plugin can serve at 150ms each, so
	// the backlog is real rather than theoretical.
	const workers = 200
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 40 * time.Second}
			for !stop.Load() {
				resp, err := client.Get(url + "/api/plugins/hello/slow")
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					completed.Add(1)
				} else {
					failed.Add(1)
				}
			}
		}()
	}

	t.Logf("%-8s %-12s %-14s %-12s %s", "at", "in-flight", "goroutines", "heap", "done/s")
	start := time.Now()
	for range 6 {
		time.Sleep(2 * time.Second)
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		elapsed := time.Since(start).Seconds()
		t.Logf("%-8s %-12d %-14d %-12s %.0f",
			time.Since(start).Round(time.Second),
			inst.InFlight(),
			runtime.NumGoroutine(),
			fmt.Sprintf("%.1f MB", float64(ms.HeapAlloc)/(1<<20)),
			float64(completed.Load())/elapsed,
		)
	}

	stop.Store(true)
	wg.Wait()

	t.Logf("%d completed, %d failed over %s with %d concurrent callers",
		completed.Load(), failed.Load(), time.Since(start).Round(time.Second), workers)
	if failed.Load() > 0 {
		t.Errorf("%d requests failed against a plugin that is merely slow", failed.Load())
	}
}

// The bound that does exist: a plugin that never answers is abandoned after
// the backend timeout, so an in-flight request cannot accumulate forever.
//
// Without this the in-flight count is arrival rate times infinity, and no
// amount of measurement above would be reassuring.
func TestHungBackendIsAbandonedAfterItsTimeout(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
	})
	// A short budget so the test does not wait out the 30s default.
	srv := newGatewayWithTimeout(reg, 300*time.Millisecond)
	defer srv.Close()

	start := time.Now()
	resp, err := http.Get(srv.URL + "/api/plugins/hello/hang")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	t.Logf("a request to a hung plugin returned %d after %s", resp.StatusCode, took.Round(time.Millisecond))
	if took > 3*time.Second {
		t.Errorf("the request took %s against a 300ms budget; a hung plugin holds a "+
			"goroutine in Core for as long as it likes", took)
	}

	// And the slot is given back, or the drain would never finish.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if inst.InFlight() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("in-flight = %d after an abandoned request; the slot was never released, "+
		"so a disable or an upgrade would wait out its full drain every time", inst.InFlight())
}

// The open-loop case, which the measurement above does not cover.
//
// Those 200 callers each wait for their response before sending the next, so
// the in-flight count is the number of callers by construction — it cannot
// grow no matter how slow the plugin is. That is the shape of most clients and
// it is why Core stayed flat, but it is not the shape that hurts.
//
// The one that hurts is arrivals that do not wait. Then in-flight is arrival
// rate times latency, bounded only by the backend timeout, and the default is
// 30 seconds: a plugin that stops answering under 1000 req/s accumulates
// thirty thousand outstanding calls before the first is abandoned. This checks
// the arithmetic holds and that the slots do come back.
func TestOpenLoopArrivalsAgainstAHungPlugin(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
	})
	const budget = 800 * time.Millisecond
	srv := newGatewayWithTimeout(reg, budget)
	defer srv.Close()

	const arrivals = 150
	var wg sync.WaitGroup
	for range arrivals {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get(srv.URL + "/api/plugins/hello/hang")
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}()
	}

	// While they are all outstanding: in-flight should be the arrivals, which
	// is the growth the closed-loop measurement cannot show.
	time.Sleep(budget / 2)
	peak := inst.InFlight()
	t.Logf("in-flight with %d simultaneous arrivals at a hung plugin: %d", arrivals, peak)
	if peak < arrivals/2 {
		t.Errorf("in-flight peaked at %d for %d arrivals; this test is not producing the "+
			"backlog it claims to measure", peak, arrivals)
	}

	wg.Wait()

	// And they are all released once abandoned. If they were not, a plugin
	// that hangs once would never be drainable again.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inst.InFlight() == 0 {
			t.Log("every slot released after the timeout")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("in-flight = %d after every request was abandoned", inst.InFlight())
}
