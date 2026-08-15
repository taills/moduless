package pluginhost

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastSupervisorConfig keeps the restart machinery on a millisecond timescale
// so these tests stay fast without faking the scheduler.
func fastSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		PollInterval:   5 * time.Millisecond,
		InitialBackoff: 5 * time.Millisecond,
		MaxBackoff:     40 * time.Millisecond,
		CrashThreshold: 3,
		CrashWindow:    time.Minute,
	}
}

func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

func TestSupervisorRestartsCrashedPlugin(t *testing.T) {
	reg := NewRegistry()
	inst, _, proc := readyInstance("p", 1)
	reg.Install("p", inst)

	var relaunches atomic.Int32
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		relaunches.Add(1)
		fresh, _, _ := readyInstance("p", 1)
		return fresh, nil
	}, fastSupervisorConfig())
	defer sup.Stop()

	sup.Watch(context.Background(), inst)

	// Simulate the process dying on its own.
	proc.exited.Store(true)

	waitFor(t, 2*time.Second, "a restart", func() bool { return relaunches.Load() == 1 })
	waitFor(t, 2*time.Second, "the new instance to be routable", func() bool {
		got, ok := reg.Current().Pick("p")
		return ok && got != inst
	})
}

// Disabling a plugin must not fight the supervisor: an instance that exits
// because it was drained is not a crash.
func TestSupervisorIgnoresDeliberateStop(t *testing.T) {
	reg := NewRegistry()
	inst, _, proc := readyInstance("p", 1)
	reg.Install("p", inst)

	var relaunches atomic.Int32
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		relaunches.Add(1)
		fresh, _, _ := readyInstance("p", 1)
		return fresh, nil
	}, fastSupervisorConfig())
	defer sup.Stop()

	sup.Watch(context.Background(), inst)

	// Admin disables the plugin: removed from the registry, then drained.
	reg.Remove("p")
	if err := inst.Drain(context.Background(), time.Second); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !proc.Exited() {
		t.Fatal("drain did not stop the process")
	}

	time.Sleep(60 * time.Millisecond)
	if got := relaunches.Load(); got != 0 {
		t.Errorf("supervisor restarted a deliberately stopped plugin %d time(s)", got)
	}
	if reg.Current().Has("p") {
		t.Error("a disabled plugin came back into the registry")
	}
}

// After a blue-green swap the old instance is expected to exit. Restarting it
// would resurrect the previous version alongside the new one.
func TestSupervisorIgnoresSupersededInstance(t *testing.T) {
	reg := NewRegistry()
	oldInst, _, oldProc := readyInstance("p", 1)
	reg.Install("p", oldInst)

	var relaunches atomic.Int32
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		relaunches.Add(1)
		fresh, _, _ := readyInstance("p", 1)
		return fresh, nil
	}, fastSupervisorConfig())
	defer sup.Stop()

	sup.Watch(context.Background(), oldInst)

	newInst, _, _ := readyInstance("p", 1)
	reg.Swap(context.Background(), "p", 100*time.Millisecond, newInst)

	waitFor(t, 2*time.Second, "the old process to be killed", func() bool { return oldProc.Exited() })
	time.Sleep(60 * time.Millisecond)

	if got := relaunches.Load(); got != 0 {
		t.Errorf("supervisor restarted a superseded instance %d time(s)", got)
	}
	if got, _ := reg.Current().Pick("p"); got != newInst {
		t.Error("registry no longer points at the new instance")
	}
}

func TestSupervisorQuarantinesAfterRepeatedCrashes(t *testing.T) {
	reg := NewRegistry()
	inst, _, proc := readyInstance("p", 1)
	reg.Install("p", inst)

	var relaunches atomic.Int32
	// Every replacement is born already dead, so each restart crashes again.
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		relaunches.Add(1)
		fresh, _, freshProc := readyInstance("p", 1)
		freshProc.exited.Store(true)
		return fresh, nil
	}, fastSupervisorConfig())
	defer sup.Stop()

	sup.Watch(context.Background(), inst)
	proc.exited.Store(true)

	waitFor(t, 3*time.Second, "the plugin to be quarantined", func() bool {
		return !reg.Current().Has("p")
	})

	// Threshold is 3, so it must stop retrying rather than loop forever.
	settled := relaunches.Load()
	time.Sleep(100 * time.Millisecond)
	if got := relaunches.Load(); got != settled {
		t.Errorf("supervisor kept restarting after quarantine: %d -> %d", settled, got)
	}
	if settled > 3 {
		t.Errorf("restarted %d times, want at most the crash threshold (3)", settled)
	}
}

// If an admin disables a plugin while the supervisor is backing off, the
// restart must be abandoned rather than silently overriding them.
func TestSupervisorAbandonsRestartWhenPluginDisabled(t *testing.T) {
	reg := NewRegistry()
	inst, _, proc := readyInstance("p", 1)
	reg.Install("p", inst)

	cfg := fastSupervisorConfig()
	cfg.InitialBackoff = 150 * time.Millisecond

	var relaunches atomic.Int32
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		relaunches.Add(1)
		fresh, _, _ := readyInstance("p", 1)
		return fresh, nil
	}, cfg)
	defer sup.Stop()

	sup.Watch(context.Background(), inst)
	proc.exited.Store(true)

	// Disable during the backoff window.
	time.Sleep(40 * time.Millisecond)
	reg.Remove("p")

	time.Sleep(300 * time.Millisecond)
	if got := relaunches.Load(); got != 0 {
		t.Errorf("supervisor restarted a plugin that was disabled mid-backoff (%d times)", got)
	}
}

func TestSupervisorBackoffGrowsAndCaps(t *testing.T) {
	cfg := SupervisorConfig{
		InitialBackoff: time.Second,
		MaxBackoff:     8 * time.Second,
		CrashThreshold: 100,
		CrashWindow:    time.Hour,
		PollInterval:   time.Second,
	}
	sup := NewSupervisor(NewRegistry(), nil, cfg)

	// Mirror recover()'s real order: the crash is recorded first, then the
	// backoff for that crash count is read.
	want := []time.Duration{
		time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 8 * time.Second, 8 * time.Second,
	}
	for i, expected := range want {
		sup.tooManyCrashes("p")
		if got := sup.backoffFor("p"); got != expected {
			t.Errorf("crash %d: backoff = %s, want %s", i+1, got, expected)
		}
	}
}

// A crash long after the previous one starts a fresh window, so a plugin that
// fails once a month is never quarantined.
func TestSupervisorCrashWindowExpires(t *testing.T) {
	cfg := fastSupervisorConfig()
	cfg.CrashWindow = time.Minute
	sup := NewSupervisor(NewRegistry(), nil, cfg)

	clock := newFakeClock()
	sup.SetClock(clock.Now)

	for range cfg.CrashThreshold - 1 {
		if sup.tooManyCrashes("p") {
			t.Fatal("quarantined before reaching the threshold")
		}
	}

	clock.Advance(2 * time.Minute)

	if sup.tooManyCrashes("p") {
		t.Error("crash counter did not reset after the window expired")
	}
}

func TestSupervisorClearCrashesResetsHistory(t *testing.T) {
	cfg := fastSupervisorConfig()
	sup := NewSupervisor(NewRegistry(), nil, cfg)

	for range cfg.CrashThreshold - 1 {
		sup.tooManyCrashes("p")
	}
	sup.ClearCrashes("p")

	if sup.tooManyCrashes("p") {
		t.Error("crash history survived ClearCrashes")
	}
}

func TestSupervisorStopIsIdempotent(t *testing.T) {
	sup := NewSupervisor(NewRegistry(), nil, fastSupervisorConfig())

	var wg sync.WaitGroup
	for range 4 {
		wg.Go(sup.Stop)
	}
	wg.Wait()
}

func TestSupervisorRetriesFailedRelaunch(t *testing.T) {
	reg := NewRegistry()
	inst, _, proc := readyInstance("p", 1)
	reg.Install("p", inst)

	var attempts atomic.Int32
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("port still in use")
		}
		fresh, _, _ := readyInstance("p", 1)
		return fresh, nil
	}, fastSupervisorConfig())
	defer sup.Stop()

	sup.Watch(context.Background(), inst)
	proc.exited.Store(true)

	waitFor(t, 3*time.Second, "a successful restart after a failed attempt", func() bool {
		got, ok := reg.Current().Pick("p")
		return ok && got != inst
	})
	if got := attempts.Load(); got < 2 {
		t.Errorf("relaunch attempted %d time(s), want at least 2", got)
	}
}
