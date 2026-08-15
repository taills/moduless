package pluginhost

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestBeginRequestRejectsUnlessReady(t *testing.T) {
	tests := []struct {
		name  string
		state State
		want  bool
	}{
		{name: "ready accepts", state: StateReady, want: true},
		{name: "starting rejects", state: StateStarting, want: false},
		{name: "draining rejects", state: StateDraining, want: false},
		{name: "stopped rejects", state: StateStopped, want: false},
		{name: "failed rejects", state: StateFailed, want: false},
		{name: "quarantined rejects", state: StateQuarantined, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inst, _, _ := readyInstance("p", 1)
			inst.setState(tc.state)

			end, ok := inst.BeginRequest()
			if ok != tc.want {
				t.Fatalf("BeginRequest ok = %v, want %v", ok, tc.want)
			}
			if ok {
				if got := inst.InFlight(); got != 1 {
					t.Errorf("in-flight = %d, want 1", got)
				}
				end()
				if got := inst.InFlight(); got != 0 {
					t.Errorf("in-flight after end = %d, want 0", got)
				}
			}
		})
	}
}

// The end function is handed to callers as a defer, so a double call must not
// drive the counter negative and make a drain wait forever.
func TestBeginRequestEndIsIdempotent(t *testing.T) {
	inst, _, _ := readyInstance("p", 1)

	end, ok := inst.BeginRequest()
	if !ok {
		t.Fatal("BeginRequest failed")
	}
	end()
	end()

	if got := inst.InFlight(); got != 0 {
		t.Errorf("in-flight = %d, want 0", got)
	}
}

func TestDrainWaitsForInFlight(t *testing.T) {
	inst, fc, fp := readyInstance("p", 1)
	fc.blockUntil = make(chan struct{})

	end, ok := inst.BeginRequest()
	if !ok {
		t.Fatal("BeginRequest failed")
	}

	drained := make(chan error, 1)
	go func() { drained <- inst.Drain(context.Background(), 2*time.Second) }()

	// Drain must not finish while the request is outstanding.
	select {
	case err := <-drained:
		t.Fatalf("drain returned early with %d in flight: %v", inst.InFlight(), err)
	case <-time.After(80 * time.Millisecond):
	}

	if got := inst.State(); got != StateDraining {
		t.Errorf("state = %v, want draining", got)
	}
	// A draining instance must refuse new work immediately, which is what
	// makes the swap safe: traffic moves to the new instance at once.
	if _, ok := inst.BeginRequest(); ok {
		t.Error("draining instance accepted a new request")
	}

	end()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not finish after in-flight work completed")
	}

	if fp.kills.Load() == 0 {
		t.Error("process was not killed after drain")
	}
	if fc.shutdownCalls.Load() == 0 {
		t.Error("plugin was not asked to shut down")
	}
	if got := inst.State(); got != StateStopped {
		t.Errorf("state = %v, want stopped", got)
	}
}

// A request that never finishes must not pin the process forever: the
// instance is already unreachable, so holding it open would leak.
func TestDrainTimesOutAndKillsAnyway(t *testing.T) {
	inst, _, fp := readyInstance("p", 1)

	if _, ok := inst.BeginRequest(); !ok {
		t.Fatal("BeginRequest failed")
	}

	err := inst.Drain(context.Background(), 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected a drain timeout error")
	}
	if fp.kills.Load() == 0 {
		t.Error("process must be killed even when the drain times out")
	}
	if got := inst.State(); got != StateStopped {
		t.Errorf("state = %v, want stopped", got)
	}
}

func TestRegistryInstallAndRemove(t *testing.T) {
	r := NewRegistry()

	if got := r.Current().Version(); got != 0 {
		t.Errorf("initial version = %d, want 0", got)
	}
	if _, ok := r.Current().Pick("a"); ok {
		t.Error("empty registry returned an instance")
	}

	instA, _, _ := readyInstance("a", 1)
	r.Install("a", instA)

	snap := r.Current()
	if snap.Version() == 0 {
		t.Error("version did not advance after install")
	}
	got, ok := snap.Pick("a")
	if !ok || got != instA {
		t.Fatalf("Pick = %v, %v; want the installed instance", got, ok)
	}

	displaced := r.Remove("a")
	if len(displaced) != 1 || displaced[0] != instA {
		t.Fatalf("Remove returned %v, want the installed instance", displaced)
	}
	if _, ok := r.Current().Pick("a"); ok {
		t.Error("removed plugin is still routable")
	}
}

// A request holds one snapshot for its whole lifetime. An admin action must
// not mutate what that request already observed, or a request could apply one
// plugin's filters and another plugin's routing.
func TestSnapshotIsIsolatedFromLaterChanges(t *testing.T) {
	r := NewRegistry()
	instA, _, _ := readyInstance("a", 1)
	r.Install("a", instA)

	held := r.Current()

	instB, _, _ := readyInstance("b", 1)
	r.Install("b", instB)
	r.Remove("a")

	if _, ok := held.Pick("a"); !ok {
		t.Error("held snapshot lost a plugin that was removed afterwards")
	}
	if held.Has("b") {
		t.Error("held snapshot gained a plugin installed afterwards")
	}
	if !r.Current().Has("b") || r.Current().Has("a") {
		t.Error("live snapshot does not reflect the latest changes")
	}
}

func TestSwapDrainsDisplacedInstance(t *testing.T) {
	r := NewRegistry()
	oldInst, oldClient, oldProc := readyInstance("a", 1)
	r.Install("a", oldInst)

	newInst, _, _ := readyInstance("a", 1)
	r.Swap(context.Background(), "a", time.Second, newInst)

	// Routing switches at the commit point, before the old instance finishes
	// draining.
	got, ok := r.Current().Pick("a")
	if !ok || got != newInst {
		t.Fatalf("Pick returned %v, want the new instance", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for oldProc.kills.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if oldProc.kills.Load() == 0 {
		t.Error("displaced instance was never terminated")
	}
	if oldClient.shutdownCalls.Load() == 0 {
		t.Error("displaced instance was never asked to shut down")
	}
}

func TestOnChangeFiresWithNewSnapshot(t *testing.T) {
	r := NewRegistry()

	var mu sync.Mutex
	var versions []uint64
	r.OnChange(func(s *Snapshot) {
		mu.Lock()
		defer mu.Unlock()
		versions = append(versions, s.Version())
	})

	instA, _, _ := readyInstance("a", 1)
	r.Install("a", instA)
	r.Remove("a")

	mu.Lock()
	defer mu.Unlock()
	if len(versions) != 2 {
		t.Fatalf("onChange fired %d times, want 2", len(versions))
	}
	if versions[0] >= versions[1] {
		t.Errorf("versions not increasing: %v", versions)
	}
}

// Carried over from the reverse tunnel's manager test: weights 1/2/3 over 600
// picks must land exactly 100/200/300.
func TestWeightedRoundRobinDistribution(t *testing.T) {
	r := NewRegistry()

	a, _, _ := readyInstance("p", 1)
	b, _, _ := readyInstance("p", 2)
	c, _, _ := readyInstance("p", 3)
	a.InstanceID, b.InstanceID, c.InstanceID = "a", "b", "c"
	r.Install("p", a, b, c)

	counts := map[string]int{}
	snap := r.Current()
	for range 600 {
		inst, ok := snap.Pick("p")
		if !ok {
			t.Fatal("Pick failed")
		}
		counts[inst.InstanceID]++
	}

	want := map[string]int{"a": 100, "b": 200, "c": 300}
	for id, expected := range want {
		if counts[id] != expected {
			t.Errorf("instance %s picked %d times, want %d (all: %v)", id, counts[id], expected, counts)
		}
	}
}

// An unhealthy replica must be skipped rather than handed traffic.
func TestPickSkipsNonReadyReplicas(t *testing.T) {
	r := NewRegistry()
	a, _, _ := readyInstance("p", 1)
	b, _, _ := readyInstance("p", 1)
	a.InstanceID, b.InstanceID = "a", "b"
	a.MarkFailed()
	r.Install("p", a, b)

	snap := r.Current()
	for range 20 {
		inst, ok := snap.Pick("p")
		if !ok {
			t.Fatal("Pick failed while a healthy replica existed")
		}
		if inst.InstanceID == "a" {
			t.Fatal("Pick returned the failed replica")
		}
	}

	b.MarkFailed()
	if _, ok := snap.Pick("p"); ok {
		t.Error("Pick succeeded with no healthy replica")
	}
}

func TestPickIsSafeUnderConcurrency(t *testing.T) {
	r := NewRegistry()
	a, _, _ := readyInstance("p", 1)
	b, _, _ := readyInstance("p", 2)
	r.Install("p", a, b)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 500 {
				if _, ok := r.Current().Pick("p"); !ok {
					t.Error("Pick failed")
					return
				}
			}
		})
	}
	wg.Wait()
}
