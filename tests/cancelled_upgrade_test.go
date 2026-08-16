package tests

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// An admin who closes the tab mid-upgrade.
//
// The admin routes pass the request's context into Enable, Disable and Upgrade
// deliberately, so that navigating away cancels the operation rather than
// leaving it running unobserved. That makes cancellation an ordinary event —
// browsers close connections — and it can land anywhere in a blue-green swap.
//
// Three separate mechanisms keep that safe, and none of them had been checked
// together: Launch kills the child on any error path, launchAll kills whatever
// it had already started, and the commit runs on a context.WithoutCancel so a
// swap cannot be torn in half. What matters is the property they add up to —
// a cancelled upgrade leaves the old version serving and no process behind.
//
// The failure this rules out is not a memory leak. An orphaned plugin still
// holds its queue consumer and its locks, so it goes on claiming work that
// Core no longer knows anybody is doing.

// pluginProcesses counts live processes running a given binary.
//
// Through ps rather than pgrep: on darwin `pgrep -f` did not match the full
// path even while a replica was serving, and a detector that silently sees
// nothing turns this into a test that always skips — which looks like
// coverage and is not. Counted this way it can be checked against the number
// of replicas actually running, which is what the caller does before trusting
// it.
func pluginProcesses(t *testing.T, binary string) int {
	t.Helper()

	// -ww disables the width truncation. Without it ps clips each line to the
	// terminal width and a path under /var/folders never appears at all, which
	// is not "no such process" but reads exactly like it.
	out, err := exec.Command("ps", "-Awwo", "pid,args").Output()
	if err != nil {
		t.Skipf("cannot list processes on this machine: %v", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, binary) {
			continue
		}
		// The ps invocation itself, and anything else merely mentioning the
		// path, are not the plugin.
		if strings.Contains(line, "ps -Awwo") || strings.Contains(line, "grep") {
			continue
		}
		n++
	}
	return n
}

func TestACancelledUpgradeLeavesNothingBehind(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)
	// bin/echo, not bin/plugin: installFixturePackage names the binary after
	// the manifest's entrypoint. Guessing it wrong made this test skip itself,
	// which is worse than failing — it reads as coverage.
	binary := filepath.Join(root, "echo", "bin", "echo")

	reg := pluginhost.NewRegistry()
	mgr := pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         root,
		DataDirRoot: filepath.Join(root, "data"),
	}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{})
	})
	drainOnCleanup(t, reg, mgr)
	mgr.Scan()

	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	serving := len(reg.Current().Replicas("echo"))
	if serving == 0 {
		t.Fatal("nothing is running, so there is no upgrade to interrupt")
	}
	settled := pluginProcesses(t, binary)
	if settled == 0 {
		t.Skipf("cannot see plugin processes on this machine: ps found none for %s "+
			"while %d replica(s) are serving", binary, serving)
	}
	t.Logf("%d replica(s) serving, %d process(es) running", serving, settled)

	// Cancelled before it starts, which is the same thing the handler does when
	// a connection drops: the process is still forked and handshakes, and the
	// cancellation bites at Configure — the point where a child exists and
	// Core has not yet decided to keep it.
	for i := range 3 {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := mgr.Upgrade(ctx, "echo"); err == nil {
			t.Fatalf("attempt %d: a cancelled upgrade reported success", i)
		}
	}

	// The old version is untouched: a cancelled upgrade must not disable what
	// was already working.
	if got := len(reg.Current().Replicas("echo")); got != serving {
		t.Errorf("%d replica(s) serving after three cancelled upgrades, was %d; "+
			"an interrupted upgrade took the running version down with it", got, serving)
	}

	// And nothing was left running. Given a moment, because Kill is not
	// instantaneous and the point is that the process goes, not when.
	deadline := time.Now().Add(10 * time.Second)
	var leaked int
	for time.Now().Before(deadline) {
		leaked = pluginProcesses(t, binary) - settled
		if leaked <= 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if leaked > 0 {
		t.Errorf("%d plugin process(es) survived three cancelled upgrades. An orphan "+
			"is not just memory: it still holds its queue consumer and its locks, so "+
			"it goes on claiming work nobody knows is being done", leaked)
	}
}
