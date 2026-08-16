package tests

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// What one slow plugin costs everybody else.
//
// Isolation is the reason this architecture pays for a process per plugin, and
// it holds for crashes: tests elsewhere show one plugin dying while the others
// serve. Slowness is the harder case and the more common one — a plugin does
// not usually die, it starts taking longer than anyone budgeted — and a filter
// subscribed to /** sits on the critical path of every request in the system,
// including requests belonging to plugins that have nothing to do with it.
//
// The declared defence is timeout_ms per filter. This measures what it is
// worth: how much a slow filter adds to unrelated traffic, and whether the
// timeout actually bounds it.
//
//	MEASURE=1 go test ./tests/ -run TestSlowFilterCost -v

// slowFilterStack serves plugin "hello" behind a global pre_route filter
// belonging to a separate, slow plugin.
func slowFilterStack(t *testing.T, delay string, timeoutMS int) string {
	t.Helper()

	backend := launchPlugin(t, "hello", "1.0.0", nil)

	var url string
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	if delay != "" {
		slow, err := launchWithEnv(t, "slowfilter", "ECHO_FILTER_DELAY="+delay)
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
		reg.InstallPlugin(pluginhost.Registration{
			Key:       "slowfilter",
			Instances: []*pluginhost.Instance{slow},
			Filters: compileFilters(t, "slowfilter", manifest.FilterDecl{
				Name:      "global",
				Phase:     manifest.PhasePreRoute,
				TimeoutMS: timeoutMS,
				Match:     manifest.FilterMatch{Paths: []string{"/**"}},
			}),
		})
	}

	h := &gateway.PluginHandler{Registry: reg, Runner: &pipeline.Runner{}}
	srv := httptest.NewServer(h.Middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })))
	t.Cleanup(srv.Close)
	url = srv.URL
	return url
}

// latencies samples n requests and returns p50 and p99.
func latencies(t *testing.T, url string, n int) (p50, p99 time.Duration, failures int) {
	t.Helper()

	client := warmClientPlain()
	got := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		resp, err := client.Get(url + "/api/plugins/hello/items")
		if err != nil {
			failures++
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			failures++
			continue
		}
		got = append(got, time.Since(start))
	}
	if len(got) == 0 {
		return 0, 0, failures
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	at := func(q float64) time.Duration { return got[int(float64(len(got)-1)*q)] }
	return at(0.50), at(0.99), failures
}

func TestSlowFilterCost(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE is not set")
	}

	const samples = 200

	t.Logf("%-34s %-12s %-12s %s", "scenario", "p50", "p99", "failed")
	for _, tc := range []struct {
		name      string
		delay     string
		timeoutMS int
	}{
		{"no filter at all", "", 0},
		{"fast filter", "0s", 100},
		{"filter sleeping 50ms, 200ms budget", "50ms", 200},
		{"filter sleeping 200ms, 500ms budget", "200ms", 500},
		{"filter sleeping 200ms, 20ms budget", "200ms", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := slowFilterStack(t, tc.delay, tc.timeoutMS)
			// Warm, so the first sample does not pay for connection setup.
			latencies(t, url, 10)

			p50, p99, failed := latencies(t, url, samples)
			t.Logf("%-34s %-12s %-12s %d",
				tc.name, p50.Round(time.Microsecond), p99.Round(time.Microsecond), failed)
		})
	}
}

// The bound, asserted rather than measured: a filter that sleeps far past its
// budget must not hold a request for longer than the budget.
//
// This is what makes a global filter survivable at all. Without it one plugin
// having a bad afternoon sets the latency of every request in the system, and
// the isolation this architecture pays for stops at crashes.
func TestFilterTimeoutBoundsAnUnrelatedRequest(t *testing.T) {
	const budget = 50 * time.Millisecond
	url := slowFilterStack(t, "2s", int(budget/time.Millisecond))

	client := warmClientPlain()
	start := time.Now()
	resp, err := client.Get(url + "/api/plugins/hello/items")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	t.Logf("request through a filter sleeping 2s with a %s budget took %s",
		budget, took.Round(time.Millisecond))

	// Generously above the budget and far below the filter's sleep: what is
	// being ruled out is "waited two seconds".
	if took > 500*time.Millisecond {
		t.Errorf("the request took %s; the filter's %s budget did not bound it, so one "+
			"slow plugin sets the latency of every request in the system", took, budget)
	}
	// And it is a fail-open filter, so the request still succeeds.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; a fail-open filter timing out must not fail the request",
			resp.StatusCode)
	}
}

// The other direction: a filter that answers inside its budget is waited for.
//
// A gateway that abandoned every filter immediately would pass the test above
// and make filters useless — the request would never see their verdict.
func TestFilterInsideItsBudgetIsWaitedFor(t *testing.T) {
	url := slowFilterStack(t, "80ms", 1000)

	client := warmClientPlain()
	start := time.Now()
	resp, err := client.Get(url + "/api/plugins/hello/items")
	took := time.Since(start)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	if took < 80*time.Millisecond {
		t.Errorf("the request took %s through a filter that sleeps 80ms; the filter's "+
			"verdict was not waited for", took)
	}
	t.Logf("a filter sleeping 80ms inside a 1s budget adds %s", took.Round(time.Millisecond))
}

// Why a tight budget costs less than the budget.
//
// Measured, a filter sleeping 200ms with a 20ms budget adds 146µs per request
// — less than the budget it was given, which cannot be explained by the
// timeout alone. The hypothesis is the circuit breaker: enough timeouts in a
// row and Core stops calling the plugin at all, so the cost falls from one
// budget per request to nothing.
//
// A hypothesis in a comment is how a wrong mechanism gets documented, so this
// checks it: the early requests should pay the budget and the later ones
// should not.
func TestABudgetIsPaidUntilTheBreakerOpens(t *testing.T) {
	const budget = 60 * time.Millisecond
	url := slowFilterStack(t, "2s", int(budget/time.Millisecond))

	client := warmClientPlain()
	var took []time.Duration
	for range 25 {
		start := time.Now()
		resp, err := client.Get(url + "/api/plugins/hello/items")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
		took = append(took, time.Since(start))
	}

	first, last := took[0], took[len(took)-1]
	t.Logf("first request %s, last %s", first.Round(time.Millisecond), last.Round(time.Millisecond))

	// The first one waits out the budget, because nothing yet knows the plugin
	// is slow.
	if first < budget/2 {
		t.Errorf("the first request took %s against a %s budget; the filter was not "+
			"called at all, so this measures something else", first, budget)
	}
	// A later one does not, because by then the breaker has given up on it.
	if last > budget/2 {
		t.Errorf("request 25 still took %s; the breaker never opened, so every request "+
			"pays a full budget for as long as the plugin stays slow", last)
	}

	// Stated as the count, since that is the number an operator cares about:
	// how many requests wear the cost before it stops.
	paid := 0
	for _, d := range took {
		if d > budget/2 {
			paid++
		}
	}
	t.Logf("%d of 25 requests paid the budget before the breaker opened", paid)
	if paid == 0 || paid == len(took) {
		t.Errorf("%d of %d paid; expected some then none", paid, len(took))
	}
}
