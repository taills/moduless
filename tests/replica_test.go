package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// Multi-replica and failure-isolation behaviour, against real processes.
//
// The weighted round-robin has a unit test over the pure function. These check
// the parts a pure function cannot: that the weights reach the scheduler from a
// real launch, that a replica dying mid-flight costs nobody a request, and that
// a plugin which keeps crashing is eventually left alone rather than restarted
// forever.

// launchWeighted starts one replica with an explicit load-balancing weight.
func launchWeighted(t *testing.T, key, instanceID string, weight int) *pluginhost.Instance {
	t.Helper()
	return launchReplica(t, key, instanceID, "1.0.0", weight, nil)
}

// instanceCounts drives traffic and reports which replica answered each time.
func instanceCounts(t *testing.T, url string, requests int) map[string]int {
	t.Helper()

	client := warmClientPlain()
	counts := map[string]int{}
	for range requests {
		resp, err := client.Get(url + "/api/plugins/hello/items")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("status = %d", resp.StatusCode)
		}
		counts[resp.Header.Get("X-Instance")]++
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return counts
}

func warmClientPlain() *http.Client {
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{MaxIdleConnsPerHost: 32},
	}
}

// Equal weights must produce an equal split. This is the ordinary case: a
// manifest asking for replicas gives them all the same weight.
func TestReplicasShareTrafficEqually(t *testing.T) {
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello",
		Instances: []*pluginhost.Instance{
			launchWeighted(t, "hello", "hello-0", 1),
			launchWeighted(t, "hello", "hello-1", 1),
			launchWeighted(t, "hello", "hello-2", 1),
		},
	})

	srv := newGateway(reg)
	defer srv.Close()

	const requests = 300
	counts := instanceCounts(t, srv.URL, requests)
	t.Logf("distribution over %d requests: %v", requests, counts)

	if len(counts) != 3 {
		t.Fatalf("%d replicas received traffic, want all 3: %v", len(counts), counts)
	}
	want := requests / 3
	for id, got := range counts {
		if got != want {
			t.Errorf("%s served %d of %d, want an exact %d; "+
				"smooth weighted round-robin is deterministic, so any drift is a real defect",
				id, got, requests, want)
		}
	}
}

// Unequal weights must split traffic in proportion. This is what makes a
// canary possible: send a tenth of the traffic to the new version.
func TestReplicasHonourWeights(t *testing.T) {
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello",
		Instances: []*pluginhost.Instance{
			launchWeighted(t, "hello", "light", 1),
			launchWeighted(t, "hello", "heavy", 4),
		},
	})

	srv := newGateway(reg)
	defer srv.Close()

	const requests = 250 // divisible by the weight total
	counts := instanceCounts(t, srv.URL, requests)
	t.Logf("distribution over %d requests: %v", requests, counts)

	// 1:4 over 250 requests is exactly 50 and 200.
	if counts["light"] != 50 || counts["heavy"] != 200 {
		t.Errorf("split was %v, want light=50 heavy=200 for weights 1:4", counts)
	}
}

// A replica dying while traffic flows must not take the plugin with it: the
// scheduler skips anything not ready and the survivors absorb the load.
//
// The requests already inside the dying process at the moment it is killed are
// lost, and no architecture can save them — the work was happening in memory
// that just went away. What must hold is that the loss is confined to that
// instant. Failures continuing after the death are the real defect: they mean
// the scheduler is still handing traffic to a corpse.
func TestReplicaDeathIsAbsorbed(t *testing.T) {
	victim := launchWeighted(t, "hello", "victim", 1)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello",
		Instances: []*pluginhost.Instance{
			launchWeighted(t, "hello", "survivor-a", 1),
			victim,
			launchWeighted(t, "hello", "survivor-b", 1),
		},
	})

	srv := newGateway(reg)
	defer srv.Close()

	var (
		stop     atomic.Bool
		settled  atomic.Bool // true once the death is far enough in the past
		attempts atomic.Int64
		atDeath  atomic.Int64 // failures during the kill itself
		after    atomic.Int64 // failures once things should have settled
		firstErr atomic.Pointer[string]
		wg       sync.WaitGroup
	)
	countFailure := func(msg string) {
		firstErr.CompareAndSwap(nil, &msg)
		if settled.Load() {
			after.Add(1)
		} else {
			atDeath.Add(1)
		}
	}

	client := warmClientPlain()
	for range 4 {
		wg.Go(func() {
			for !stop.Load() {
				attempts.Add(1)
				resp, err := client.Get(srv.URL + "/api/plugins/hello/items")
				if err != nil {
					countFailure(err.Error())
					continue
				}
				if resp.StatusCode != 200 {
					countFailure(fmt.Sprintf("status %d", resp.StatusCode))
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}

	time.Sleep(100 * time.Millisecond)
	victim.Kill()
	time.Sleep(200 * time.Millisecond)
	settled.Store(true)
	time.Sleep(400 * time.Millisecond)

	stop.Store(true)
	wg.Wait()

	detail := ""
	if p := firstErr.Load(); p != nil {
		detail = " first failure: " + *p
	}
	t.Logf("%d requests: %d failed at the moment of death, %d failed afterwards%s",
		attempts.Load(), atDeath.Load(), after.Load(), detail)

	if attempts.Load() < 50 {
		t.Fatalf("only %d requests; the test did not exercise the death", attempts.Load())
	}
	if after.Load() > 0 {
		t.Errorf("%d requests failed after the replica had been dead for 200ms; "+
			"the scheduler is still routing to it.%s", after.Load(), detail)
	}
	// A death should cost a handful of in-flight requests, not a wave of them.
	// A large number here means traffic kept arriving at the dead replica for
	// a while before the state change took effect.
	if limit := attempts.Load() / 20; atDeath.Load() > limit {
		t.Errorf("%d of %d requests failed at the moment of death, more than the %d "+
			"that could plausibly have been in flight", atDeath.Load(), attempts.Load(), limit)
	}
}

// A plugin that crashes repeatedly must eventually be left alone. Restarting
// forever turns one broken plugin into a permanent fork bomb against Core, and
// hides the failure from whoever needs to fix it.
func TestCrashLoopEndsInQuarantine(t *testing.T) {
	reg := pluginhost.NewRegistry()
	first := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{first}})

	srv := newGateway(reg)
	defer srv.Close()

	const threshold = 3
	var (
		relaunches atomic.Int32
		latest     atomic.Pointer[pluginhost.Instance]
	)
	latest.Store(first)

	sup := pluginhost.NewSupervisor(reg, func(context.Context, string) (*pluginhost.Instance, error) {
		relaunches.Add(1)
		inst := launchPlugin(t, "hello", "1.0.0", nil)
		latest.Store(inst)
		return inst, nil
	}, pluginhost.SupervisorConfig{
		PollInterval:   10 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     30 * time.Millisecond,
		CrashThreshold: threshold,
		CrashWindow:    time.Minute,
	})
	defer sup.Stop()
	sup.Watch(context.Background(), first)

	// Kill it as fast as it comes back.
	crash := func(inst *pluginhost.Instance) {
		_, _ = inst.Client.HandleHTTP(context.Background(),
			&pb.HttpRequest{Method: "GET", Path: "/crash"})
	}

	deadline := time.Now().Add(20 * time.Second)
	quarantined := false
	crash(first)
	for time.Now().Before(deadline) {
		inst := latest.Load()
		if inst.State() == pluginhost.StateQuarantined {
			quarantined = true
			break
		}
		if inst.State() == pluginhost.StateReady {
			crash(inst)
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Logf("%d relaunch attempts before quarantine", relaunches.Load())
	if !quarantined {
		t.Fatalf("the plugin crashed repeatedly but was never quarantined (state %v after %d relaunches)",
			latest.Load().State(), relaunches.Load())
	}
	if relaunches.Load() > threshold+2 {
		t.Errorf("%d relaunches for a threshold of %d; the crash counter is not bounding retries",
			relaunches.Load(), threshold)
	}

	// A quarantined plugin must be off the routing table, not merely marked.
	// Being marked but still routable would send traffic into a process
	// nothing is supervising.
	code := getStatus(t, warmClientPlain(), srv.URL+"/api/plugins/hello/items")
	if code == 200 {
		t.Error("a quarantined plugin is still serving traffic")
	}
	t.Logf("requests to a quarantined plugin answer %d", code)
}

// Once a plugin is quarantined the supervisor must stop working on it, so an
// operator sees a stable failure rather than a machine that keeps restarting
// something broken.
func TestQuarantineStopsRelaunching(t *testing.T) {
	reg := pluginhost.NewRegistry()
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{inst}})

	var relaunches atomic.Int32
	sup := pluginhost.NewSupervisor(reg, func(context.Context, string) (*pluginhost.Instance, error) {
		relaunches.Add(1)
		return launchPlugin(t, "hello", "1.0.0", nil), nil
	}, pluginhost.SupervisorConfig{
		PollInterval:   10 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     20 * time.Millisecond,
		CrashThreshold: 5,
		CrashWindow:    time.Minute,
	})
	defer sup.Stop()

	inst.MarkQuarantined()
	sup.Watch(context.Background(), inst)
	inst.Kill()

	time.Sleep(500 * time.Millisecond)
	if n := relaunches.Load(); n != 0 {
		t.Errorf("a quarantined plugin was relaunched %d time(s); quarantine must be terminal until an admin clears it", n)
	}
}

// A quarantine an operator cannot see is barely better than none: the console
// would show an enabled plugin with zero replicas, which is also what a plugin
// mid-restart looks like. One of those recovers on its own and the other never
// does.
func TestQuarantineIsVisibleInStatus(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	reg := pluginhost.NewRegistry()
	mgr := pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         root,
		DataDirRoot: filepath.Join(root, "data"),
		Supervisor: pluginhost.SupervisorConfig{
			PollInterval:   20 * time.Millisecond,
			InitialBackoff: 10 * time.Millisecond,
			MaxBackoff:     50 * time.Millisecond,
			CrashThreshold: 3,
			CrashWindow:    time.Minute,
		},
	}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{})
	})
	drainOnCleanup(t, reg, mgr)

	mgr.Scan()
	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	statusOf := func() pluginhost.Status {
		t.Helper()
		for _, st := range mgr.List() {
			if st.Key == "echo" {
				return st
			}
		}
		t.Fatal("echo missing from the status list")
		return pluginhost.Status{}
	}

	if st := statusOf(); st.Quarantined {
		t.Fatal("a freshly enabled plugin reports as quarantined")
	}

	// Make it die abruptly, over and over. Kill() would not do: that records a
	// deliberate stop, and the supervisor rightly does not undo those. Only an
	// unannounced death is a crash.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if statusOf().Quarantined {
			break
		}
		for _, inst := range reg.Current().Replicas("echo") {
			if inst.State() == pluginhost.StateReady {
				_, _ = inst.Client.HandleHTTP(context.Background(),
					&pb.HttpRequest{Method: "GET", Path: "/crash"})
			}
		}
		time.Sleep(30 * time.Millisecond)
	}

	st := statusOf()
	t.Logf("status after the crash loop: enabled=%v replicas=%d quarantined=%v",
		st.Enabled, st.Replicas, st.Quarantined)

	if !st.Quarantined {
		t.Fatal("the plugin was never reported as quarantined, so an operator cannot tell it apart from one that is restarting")
	}
	if st.QuarantinedAt == nil {
		t.Error("quarantine has no timestamp, so there is no way to tell how long it has been isolated")
	}
	if st.Replicas != 0 {
		t.Errorf("a quarantined plugin still has %d replica(s) in the routing table", st.Replicas)
	}

	// Re-enabling is the admin saying "try again", and must clear it.
	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if st := statusOf(); st.Quarantined {
		t.Error("re-enabling a quarantined plugin left it reported as quarantined while it was running")
	}
}

// installFixturePackage lays the echo fixture out as a distributable plugin
// package, so tests can drive it through the manager: scan, enable, crash,
// quarantine, re-enable.
func installFixturePackage(t *testing.T, root string) {
	t.Helper()

	dir := filepath.Join(root, "echo")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// TestMain already built the fixture; copy rather than rebuild.
	binary, err := os.ReadFile(pluginBinary)
	if err != nil {
		t.Fatalf("reading the fixture binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "echo"), binary, 0o755); err != nil {
		t.Fatalf("writing the fixture binary: %v", err)
	}

	manifestSrc, err := os.ReadFile(filepath.Join("fixtures", "echoplugin", "manifest.yaml"))
	if err != nil {
		t.Fatalf("reading the fixture manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), manifestSrc, 0o644); err != nil {
		t.Fatalf("writing the fixture manifest: %v", err)
	}
}

// A slow request in flight when its plugin is swapped out must finish, not be
// cut off.
//
// The zero-downtime upgrade test does not establish this. Its requests take
// microseconds, so by the time the drain looks there is nothing in flight and
// a drain that waited for nothing would pass. What zero downtime rests on is
// two separate mechanisms — the snapshot swaps atomically so no new request
// reaches the old instance, and the drain waits for the ones already inside it
// — and only a request slow enough to still be running distinguishes them.
func TestSlowRequestSurvivesAnUpgrade(t *testing.T) {
	reg := pluginhost.NewRegistry()
	v1 := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v1}})

	srv := newGateway(reg)
	defer srv.Close()

	// The fixture's /slow takes 150ms, comfortably longer than the swap.
	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Get(srv.URL + "/api/plugins/hello/slow")
		if err != nil {
			done <- result{err: err}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done <- result{status: resp.StatusCode}
	}()

	// Let the request reach the plugin, then swap underneath it.
	time.Sleep(40 * time.Millisecond)

	v2 := launchPlugin(t, "hello", "2.0.0", nil)
	swapStart := time.Now()
	reg.Swap(context.Background(),
		pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v2}},
		5*time.Second)

	got := <-done
	t.Logf("in-flight request finished %s after the swap began: status=%d err=%v",
		time.Since(swapStart).Round(time.Millisecond), got.status, got.err)

	if got.err != nil {
		t.Fatalf("a request in flight during an upgrade failed at the transport layer: %v; "+
			"the old instance was killed before it finished", got.err)
	}
	if got.status != 200 {
		t.Errorf("a request in flight during an upgrade returned %d; the drain did not wait for it", got.status)
	}

	// The old instance is gone once its work is done.
	deadline := time.Now().Add(10 * time.Second)
	for !v1.ProcessExited() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !v1.ProcessExited() {
		t.Error("the old instance was never stopped after draining")
	}
}

// The log phase runs after the response has been written, on its own
// goroutine. Its admission must be its own too.
//
// Admissions are held for the life of a request and released when the response
// is written — but the log phase starts after that point, so it admits itself
// into a request that has already let go. Nothing then releases it, every
// request carrying a log filter leaks one in-flight count, and a drain waits
// for a request that finished long ago. It surfaces as "drain timed out with N
// request(s) in flight" on a perfectly idle plugin.
func TestLogPhaseDoesNotLeakInFlight(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "hello", manifest.FilterDecl{
			Name:  "access-log",
			Phase: manifest.PhaseLog,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})

	srv := newGateway(reg)
	defer srv.Close()

	client := warmClientPlain()
	for range 20 {
		if code := getStatus(t, client, srv.URL+"/api/plugins/hello/items"); code != 200 {
			t.Fatalf("status = %d", code)
		}
	}

	// The log phase is asynchronous, so give it time to finish before looking.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if inst.InFlight() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Errorf("in-flight = %d after 20 completed requests; a drain would wait 30s "+
		"for requests that already finished", inst.InFlight())
}
