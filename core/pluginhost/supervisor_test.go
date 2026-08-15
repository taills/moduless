package pluginhost

import (
	"context"
	"errors"
	"fmt"
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
	reg.Swap(context.Background(), Registration{Key: "p", Instances: []*Instance{newInst}}, 100*time.Millisecond)

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

// A plugin an admin disabled mid-restart must not end up quarantined.
//
// Disable clears `enabled` before it removes the plugin from the registry, so
// a recovery that wakes from its backoff inside that window still sees the
// plugin registered and tries to relaunch it. relaunch refuses — the plugin is
// no longer enabled — and the supervisor used to treat that refusal as another
// failed restart: it retried with a longer backoff, and each attempt counted
// toward the crash threshold. Enough of them and a plugin somebody switched
// off deliberately is reported as one Core gave up on after repeated crashes,
// which is both wrong and only clearable by an explicit re-enable.
func TestSupervisorDoesNotQuarantineADisabledPlugin(t *testing.T) {
	reg := NewRegistry()
	inst, _, proc := readyInstance("p", 1)
	reg.Install("p", inst)

	var attempts atomic.Int32
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		attempts.Add(1)
		// What Manager.relaunch returns once an admin has disabled the plugin.
		return nil, fmt.Errorf("%w: p", ErrPluginDisabled)
	}, fastSupervisorConfig())
	defer sup.Stop()

	sup.Watch(context.Background(), inst)
	proc.Kill()

	waitFor(t, 2*time.Second, "the supervisor to attempt a restart",
		func() bool { return attempts.Load() >= 1 })

	// Long enough for several more backoff rounds, had it been retrying.
	time.Sleep(200 * time.Millisecond)

	if got := attempts.Load(); got != 1 {
		t.Errorf("the supervisor tried %d times to restart a plugin that was disabled; "+
			"each retry counts toward the crash threshold", got)
	}
	if quarantined(sup, "p") {
		t.Error("a plugin an admin disabled was quarantined as if it had been crashing")
	}
}

// The other direction: a restart that fails for a real reason still escalates.
// A supervisor that treated every failure as "disabled" would give up on a
// plugin that is genuinely broken, and it would pass the test above.
func TestSupervisorStillQuarantinesOnRealRestartFailures(t *testing.T) {
	reg := NewRegistry()
	inst, _, proc := readyInstance("p", 1)
	reg.Install("p", inst)

	var attempts atomic.Int32
	sup := NewSupervisor(reg, func(context.Context, string) (*Instance, error) {
		attempts.Add(1)
		return nil, errors.New("exec format error")
	}, fastSupervisorConfig())
	defer sup.Stop()

	sup.Watch(context.Background(), inst)
	proc.Kill()

	waitFor(t, 3*time.Second, "the plugin to be quarantined",
		func() bool { return quarantined(sup, "p") })

	if got := attempts.Load(); got < 2 {
		t.Errorf("only %d restart attempt(s) before quarantine; a real failure should be retried", got)
	}
}

func quarantined(s *Supervisor, key string) bool {
	_, ok := s.QuarantinedSince(key)
	return ok
}
