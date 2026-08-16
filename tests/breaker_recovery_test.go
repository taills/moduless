package tests

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// How long after a plugin recovers does its traffic come back.
//
// The breaker's own comment says five seconds is "short enough that a
// recovered plugin returns to service quickly". That is a claim about
// wall-clock recovery and it had never been measured: the unit tests cover the
// state machine, and the state machine does not obviously add up to five
// seconds. One probe is admitted per open window, and closing takes two
// consecutive probe successes — so if the second probe also has to wait out a
// window, recovery is ten seconds rather than five, and an operator watching a
// plugin they have just fixed would see twice the outage they were promised.
//
// This measures it against a real plugin, with the breaker's timings scaled
// down so the test does not take a minute.

// breakerStack serves a plugin behind a fail-open filter, with a breaker
// tuned for measurement rather than production.
func breakerStack(t *testing.T, cfg pluginhost.BreakerConfig) (url string, inst *pluginhost.Instance) {
	t.Helper()

	inst = launchPlugin(t, "flaky", "1.0.0", nil)
	inst.Breaker = pluginhost.NewBreaker(cfg)

	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "flaky",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "flaky", manifest.FilterDecl{
			Name:  "guard",
			Phase: manifest.PhasePreRoute,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL, inst
}

// Recovery measured by what the plugin sees, not by what the breaker says.
//
// The first version of this asked Breaker.Open(), which reports only whether
// the open window has expired — it knows nothing about whether the probes
// succeeded. So it measured "400ms window took 414ms", which is trivially true
// and says nothing about the thing in question. The signal that traffic is
// really back is the plugin being called again, so that is what this counts.
func TestTimeFromRecoveryToNormalTraffic(t *testing.T) {
	const openFor = 400 * time.Millisecond

	url, inst := breakerStack(t, pluginhost.BreakerConfig{
		FailureThreshold:  3,
		OpenDuration:      openFor,
		HalfOpenSuccesses: 2,
	})

	// Break it: the fixture's /boom path makes its filter return an error.
	for range 5 {
		resp, err := http.Get(url + "/boom")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
	}
	if !inst.Breaker.Open() {
		t.Fatal("the breaker did not open after five failing calls; this test would " +
			"measure a recovery that never had to happen")
	}
	recovered := time.Now()

	// From here the plugin is healthy: every path but /boom succeeds. Traffic
	// is back when consecutive requests are all reaching the filter again —
	// one call is a probe, several in a row is service.
	const inARow = 5
	var (
		streak   int
		restored time.Duration
	)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && streak < inARow {
		trace := fmt.Sprintf("recov-%d", time.Since(recovered).Nanoseconds())
		resp := requestWithTrace(t, url, "/api/plugins/hello/items", trace)
		resp.Body.Close()

		if len(phasesSeen(t, inst, trace)) > 0 {
			if streak++; streak == 1 {
				restored = time.Since(recovered)
			}
		} else {
			streak = 0
		}
		time.Sleep(10 * time.Millisecond)
	}

	if streak < inARow {
		t.Fatalf("the filter was not called %d times in a row within 10s of the plugin "+
			"recovering; traffic never came back", inARow)
	}
	t.Logf("open for %s; the filter was called again %s after the plugin was healthy, "+
		"and served %d consecutive requests from there",
		openFor, restored.Round(time.Millisecond), inARow)

	// One window, not two. Two consecutive probe successes are required and
	// one probe is admitted per window, so a breaker that made the second
	// probe wait out another window would land near 2x. Compared against 1.5x
	// to leave room for scheduling.
	if restored > openFor*3/2 {
		t.Errorf("the filter was first called again %s after recovery, against a %s "+
			"window — so closing costs more than one window and 'returns to service "+
			"quickly' understates the outage by the number of probes required",
			restored, openFor)
	}
}

// While it is open, the filter is not called at all.
//
// This is the other half of what the breaker is for and it is what makes the
// measurement above meaningful: if the filter were still being called, there
// would be no outage to recover from.
func TestAnOpenBreakerSkipsTheFilterEntirely(t *testing.T) {
	url, inst := breakerStack(t, pluginhost.BreakerConfig{
		FailureThreshold:  3,
		OpenDuration:      5 * time.Second,
		HalfOpenSuccesses: 2,
	})

	for range 5 {
		resp, err := http.Get(url + "/boom")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		resp.Body.Close()
	}
	if !inst.Breaker.Open() {
		t.Fatal("the breaker did not open")
	}

	// A request now: the plugin is healthy, but an open breaker should not ask
	// it. Checked by whether the plugin was called, not by how long it took —
	// a fast request is consistent with a fast filter, and the claim is that
	// there is no filter call at all.
	const trace = "breaker-open"
	start := time.Now()
	resp := requestWithTrace(t, url, "/api/plugins/hello/items", trace)
	resp.Body.Close()
	took := time.Since(start)

	t.Logf("a request through an open breaker took %s", took.Round(time.Microsecond))
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; a fail-open filter behind an open breaker must not fail "+
			"the request", resp.StatusCode)
	}
	if got := phasesSeen(t, inst, trace); len(got) != 0 {
		t.Errorf("the plugin was called %v while its breaker was open; the breaker saves "+
			"nothing if the call still happens", got)
	}
}
