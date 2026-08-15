package tests

import (
	"io"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// Sustained load.
//
// Every performance number in this repository comes from a benchmark lasting a
// few seconds. That measures throughput; it does not measure whether the thing
// still works after a while. Token buckets accumulate entries, connection pools
// churn, goroutines are started per request, and a leak of any of those looks
// exactly like healthy behaviour until it does not.
//
// Off by default — it takes minutes:
//
//	SOAK=2m go test ./tests/ -run TestSoak -v

func soakDuration(t *testing.T) time.Duration {
	t.Helper()

	raw := os.Getenv("SOAK")
	if raw == "" {
		t.Skip("SOAK is not set (try SOAK=2m)")
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("SOAK=%q: %v", raw, err)
	}
	return d
}

// sample is one observation of the things that should not grow.
type sample struct {
	at         time.Duration
	goroutines int
	heapMB     float64
	inFlight   int64
}

func TestSoakUnderSustainedLoad(t *testing.T) {
	duration := soakDuration(t)

	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "hello", manifest.FilterDecl{
			Name:  "guard",
			Phase: manifest.PhasePreRoute,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}, manifest.FilterDecl{
			Name:  "access-log",
			Phase: manifest.PhaseLog,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})

	srv := newGateway(reg)
	defer srv.Close()

	var (
		stop      atomic.Bool
		completed atomic.Int64
		failed    atomic.Int64
		wg        sync.WaitGroup

		latMu     sync.Mutex
		latencies []timedLatency
	)

	const workers = 4
	for range workers {
		wg.Go(func() {
			client := warmClientPlain()
			// Every hundredth request, not every one. At fifteen thousand
			// requests a second a full record is megabytes of Durations, and
			// the first version of this test measured its own slice growing
			// and called it a heap leak.
			const sampleEvery = 100
			local := make([]timedLatency, 0, 8192)
			var n int
			for !stop.Load() {
				start := time.Now()
				resp, err := client.Get(srv.URL + "/api/plugins/hello/items")
				if err != nil {
					failed.Add(1)
					continue
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					failed.Add(1)
					continue
				}
				if n++; n%sampleEvery == 0 {
					local = append(local, timedLatency{at: start, took: time.Since(start)})
				}
				completed.Add(1)
			}
			latMu.Lock()
			latencies = append(latencies, local...)
			latMu.Unlock()
		})
	}

	// Let the process reach steady state before the first sample: the initial
	// allocations of a warming connection pool are not a leak.
	time.Sleep(3 * time.Second)
	runStart := time.Now()

	var samples []sample
	start := time.Now()
	for time.Since(start) < duration {
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		samples = append(samples, sample{
			at:         time.Since(start),
			goroutines: runtime.NumGoroutine(),
			heapMB:     float64(ms.HeapAlloc) / (1 << 20),
			inFlight:   inst.InFlight(),
		})
		time.Sleep(5 * time.Second)
	}

	stop.Store(true)
	wg.Wait()

	// Waited for rather than sampled once. Two things make an immediate read
	// wrong: while workers run, a non-zero count is the requests in flight,
	// which is the system working; and the log phase outlives the response it
	// belongs to, so the last few requests are still finishing after the
	// workers have stopped. What must be true is that it reaches zero.
	drained := false
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if inst.InFlight() == 0 {
			drained = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !drained {
		t.Errorf("in-flight = %d ten seconds after the load stopped; a drain would wait "+
			"for requests that finished long ago", inst.InFlight())
	}

	// One more heap reading with nothing running, which is the only one that
	// can distinguish retained memory from memory in use.
	runtime.GC()
	var idle runtime.MemStats
	runtime.ReadMemStats(&idle)
	idleHeapMB := float64(idle.HeapAlloc) / (1 << 20)

	if len(samples) < 3 {
		t.Fatalf("only %d samples; run for longer than %s", len(samples), duration)
	}

	rate := float64(completed.Load()) / duration.Seconds()
	t.Logf("%d requests in %s (%.0f req/s), %d failed",
		completed.Load(), duration.Round(time.Second), rate, failed.Load())

	for _, s := range samples {
		t.Logf("  +%3ds  goroutines=%d heap=%.1fMB in-flight=%d",
			int(s.at.Seconds()), s.goroutines, s.heapMB, s.inFlight)
	}

	if failed.Load() > 0 {
		t.Errorf("%d requests failed during sustained load", failed.Load())
	}

	// Compare the first third against the last: a leak shows as a trend, and a
	// trend is what a single before/after pair cannot distinguish from noise.
	first, last := samples[0], samples[len(samples)-1]

	if last.goroutines > first.goroutines+20 {
		t.Errorf("goroutines went from %d to %d over %s; something started per request is not stopping",
			first.goroutines, last.goroutines, duration)
	}
	t.Logf("heap once idle: %.1fMB (first sample under load %.1fMB)", idleHeapMB, first.heapMB)

	// The idle reading is what matters. Under load the heap holds live
	// request state, which grows with concurrency rather than with time;
	// retained memory is what is still there once nothing is running.
	if first.heapMB > 0 && idleHeapMB > first.heapMB*2 {
		t.Errorf("heap is %.1fMB with nothing running, against %.1fMB early in the run; "+
			"something is retained per request", idleHeapMB, first.heapMB)
	}

	// Latency must not degrade as the run goes on.
	latMu.Lock()
	all := slices.Clone(latencies)
	latMu.Unlock()
	if len(all) < 100 {
		t.Fatalf("only %d latency samples", len(all))
	}
	reportLatency(t, all, runStart)
}

// timedLatency is one request's duration and when it began, so the run can be
// split into periods.
type timedLatency struct {
	at   time.Time
	took time.Duration
}

// reportLatency prints the distribution overall and per third of the run.
//
// The split is the point. A single p50 over a long run cannot tell a system
// that degrades from a machine that thermally throttles — both show up as one
// worse number. Comparing the run's own thirds does not separate those either,
// but it does say whether the change happened progressively, and it is what a
// human needs to decide whether to look further.
func reportLatency(t *testing.T, all []timedLatency, start time.Time) {
	t.Helper()

	quantiles := func(in []timedLatency) (p50, p99, max time.Duration) {
		if len(in) == 0 {
			return 0, 0, 0
		}
		d := make([]time.Duration, len(in))
		for i, l := range in {
			d[i] = l.took
		}
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		at := func(q float64) time.Duration { return d[int(float64(len(d)-1)*q)] }
		return at(0.50), at(0.99), d[len(d)-1]
	}

	p50, p99, max := quantiles(all)
	t.Logf("latency overall: p50=%s p99=%s max=%s over %d sampled requests",
		p50.Round(time.Microsecond), p99.Round(time.Microsecond),
		max.Round(time.Microsecond), len(all))

	total := all[len(all)-1].at.Sub(start)
	third := total / 3
	var periods [3][]timedLatency
	for _, l := range all {
		i := int(l.at.Sub(start) / third)
		periods[min(i, 2)] = append(periods[min(i, 2)], l)
	}

	var firstP50, lastP50 time.Duration
	for i, period := range periods {
		p50, p99, _ := quantiles(period)
		t.Logf("  third %d: p50=%s p99=%s (%d samples)",
			i+1, p50.Round(time.Microsecond), p99.Round(time.Microsecond), len(period))
		if i == 0 {
			firstP50 = p50
		}
		if i == 2 {
			lastP50 = p50
		}
	}

	if p50 > 0 && p99 > p50*50 {
		t.Errorf("p99 (%s) is more than 50x p50 (%s); requests are stalling rather than merely queueing",
			p99, p50)
	}
	// Deliberately generous. This is a laptop, and the point is to catch a
	// system that gets progressively worse — a growing structure walked per
	// request, a lock held longer as state accumulates — not to police the
	// machine's clock speed.
	if firstP50 > 0 && lastP50 > firstP50*4 {
		t.Errorf("p50 went from %s in the first third to %s in the last; "+
			"this looks like degradation rather than variance", firstP50, lastP50)
	}
}
