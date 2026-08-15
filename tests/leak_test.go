package tests

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// Leak checks.
//
// Core is a gateway: it runs for months and plugins are enabled, disabled and
// upgraded underneath it the whole time. Every one of those operations starts
// goroutines — a supervisor watcher, a gRPC connection, a health stream — and
// anything that fails to stop accumulates silently until the process dies of
// it. That failure is invisible in any test that performs an operation once.

// settledGoroutines waits until the count stops changing and returns it.
//
// Waiting for a threshold instead would defeat the purpose: it returns as soon
// as the number happens to be acceptable, which can be mid-teardown, and the
// test then measures the moment it sampled rather than what was left behind.
// Teardown here is asynchronous — gRPC connections, the plugin host and the
// supervisor's watch loop all end on their own schedule, the last of them only
// at its next poll — so the only honest measurement is one taken after
// everything has stopped moving.
func settledGoroutines(t *testing.T, within time.Duration) int {
	t.Helper()

	// Long enough to span several of the supervisor's poll intervals, or a
	// watcher that is about to notice its instance is gone would be counted as
	// a leak.
	const stableFor = 15 // consecutive samples with no change
	deadline := time.Now().Add(within)

	last, stable := -1, 0
	for time.Now().Before(deadline) {
		runtime.GC()
		n := runtime.NumGoroutine()
		if n == last {
			if stable++; stable >= stableFor {
				return n
			}
		} else {
			last, stable = n, 0
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("goroutine count never settled within %s (last %d)", within, last)
	return last
}

// Enabling and disabling a plugin repeatedly must not accumulate goroutines.
// An operator toggling a misbehaving plugin, or a deploy loop, does this dozens
// of times a day.
func TestNoGoroutineLeakAcrossEnableDisable(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, _ := newManagerOver(t, root, nil)
	mgr.Scan()

	ctx := context.Background()

	// One cycle first, so one-off initialisation is not counted as a leak.
	if err := mgr.Enable(ctx, "echo"); err != nil {
		t.Fatalf("warmup enable: %v", err)
	}
	if err := mgr.Disable(ctx, "echo"); err != nil {
		t.Fatalf("warmup disable: %v", err)
	}
	baseline := settledGoroutines(t, 15*time.Second)
	t.Logf("goroutines after one cycle: %d", baseline)

	const cycles = 10
	for i := range cycles {
		if err := mgr.Enable(ctx, "echo"); err != nil {
			t.Fatalf("enable %d: %v", i, err)
		}
		if err := mgr.Disable(ctx, "echo"); err != nil {
			t.Fatalf("disable %d: %v", i, err)
		}
	}

	// Slack for the runtime's own and for anything that finishes a moment
	// after the count settles. Deliberately small: the failure being looked for
	// is proportional to the cycle count, so a per-cycle leak of even one
	// goroutine clears this at ten cycles.
	const slack = 5
	after := settledGoroutines(t, 20*time.Second)
	t.Logf("goroutines after %d more cycles: %d (baseline %d)", cycles, after, baseline)

	if after > baseline+slack {
		t.Errorf("goroutines grew from %d to %d across %d enable/disable cycles "+
			"(~%.1f leaked per cycle); a gateway doing this daily would not survive",
			baseline, after, cycles, float64(after-baseline)/cycles)
	}
}

// Upgrades run continuously in a deploy pipeline, and each one starts a whole
// new process while draining the old.
func TestNoGoroutineLeakAcrossUpgrades(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, _ := newManagerOver(t, root, nil)
	mgr.Scan()

	ctx := context.Background()
	if err := mgr.Enable(ctx, "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := mgr.Upgrade(ctx, "echo"); err != nil {
		t.Fatalf("warmup upgrade: %v", err)
	}
	baseline := settledGoroutines(t, 15*time.Second)
	t.Logf("goroutines after the first upgrade: %d", baseline)

	const upgrades = 8
	for i := range upgrades {
		if err := mgr.Upgrade(ctx, "echo"); err != nil {
			t.Fatalf("upgrade %d: %v", i, err)
		}
	}

	const slack = 5
	after := settledGoroutines(t, 20*time.Second)
	t.Logf("goroutines after %d more upgrades: %d (baseline %d)", upgrades, after, baseline)

	if after > baseline+slack {
		t.Errorf("goroutines grew from %d to %d across %d upgrades (~%.1f per upgrade); "+
			"the old instances are not being fully torn down",
			baseline, after, upgrades, float64(after-baseline)/upgrades)
	}

	// And the plugin still works after all that.
	if err := mgr.Disable(ctx, "echo"); err != nil {
		t.Errorf("disable after repeated upgrades: %v", err)
	}
}

// Every plugin process started must eventually be gone. A leaked process is
// worse than a leaked goroutine: it holds memory Core cannot reclaim and keeps
// whatever the plugin opened open.
func TestDisabledPluginProcessesExit(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	reg := pluginhost.NewRegistry()
	mgr := pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         root,
		DataDirRoot: filepath.Join(root, "data"),
		DevMode:     true,
	}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{})
	})
	defer mgr.Close()
	mgr.Scan()

	ctx := context.Background()
	var started []*pluginhost.Instance
	for range 5 {
		if err := mgr.Enable(ctx, "echo"); err != nil {
			t.Fatalf("enable: %v", err)
		}
		started = append(started, reg.Current().Replicas("echo")...)
		if err := mgr.Disable(ctx, "echo"); err != nil {
			t.Fatalf("disable: %v", err)
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		alive := 0
		for _, inst := range started {
			if !inst.ProcessExited() {
				alive++
			}
		}
		if alive == 0 {
			t.Logf("all %d plugin processes exited", len(started))
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d of %d plugin processes are still running after being disabled",
				alive, len(started))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
