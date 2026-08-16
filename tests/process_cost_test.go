package tests

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
)

// What a plugin costs to run.
//
// This architecture's premise is one operating-system process per plugin, and
// the number nobody had was what that costs. It decides deployment sizing —
// whether twenty plugins is a 512MB container or a 4GB one — and it decides
// how long an operator waits for Core to come back after a restart, since
// every plugin is cold-started with it.
//
//	MEASURE=1 go test ./tests/ -run TestPluginProcessCost -v
//
// The absolute numbers belong to this machine and this fixture. What travels
// is the shape: whether the cost per plugin is flat, and whether start-up is
// linear or worse.

// childRSS sums the resident memory of this process's children, in KiB.
//
// Read from ps rather than from the children themselves: a plugin has no
// obligation to report its own memory, and what an operator sizes a container
// against is what the kernel says.
func childRSS(t *testing.T) (total int64, count int) {
	t.Helper()

	out, err := exec.Command("ps", "-o", "ppid=,rss=", "-ax").Output()
	if err != nil {
		t.Skipf("ps: %v", err)
	}
	me := int64(os.Getpid())
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		ppid, err1 := strconv.ParseInt(f[0], 10, 64)
		rss, err2 := strconv.ParseInt(f[1], 10, 64)
		if err1 != nil || err2 != nil || ppid != me {
			continue
		}
		total += rss
		count++
	}
	return total, count
}

func TestPluginProcessCost(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE is not set")
	}

	// Whatever is already running under this test binary, so the plugins are
	// measured against a baseline rather than against zero.
	baseRSS, baseCount := childRSS(t)
	t.Logf("baseline: %d child process(es), %.1f MB", baseCount, float64(baseRSS)/1024)
	t.Logf("%-6s %-12s %-14s %-14s %s", "count", "start", "per plugin", "child total", "Core heap")

	for _, n := range []int{1, 5, 10, 20} {
		t.Run(fmt.Sprintf("%d_plugins", n), func(t *testing.T) {
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)

			start := time.Now()
			insts := make([]*pluginhost.Instance, 0, n)
			for i := range n {
				insts = append(insts, launchPlugin(t, fmt.Sprintf("cost-%d", i), "1.0.0", nil))
			}
			elapsed := time.Since(start)

			// Let the children finish their own start-up allocations before
			// reading, or the first ones are measured warm and the last cold.
			time.Sleep(300 * time.Millisecond)
			rss, count := childRSS(t)

			runtime.GC()
			var after runtime.MemStats
			runtime.ReadMemStats(&after)

			plugins := count - baseCount
			perPlugin := float64(0)
			if plugins > 0 {
				perPlugin = float64(rss-baseRSS) / float64(plugins) / 1024
			}
			t.Logf("%-6d %-12s %-14s %-14s %s",
				n,
				elapsed.Round(time.Millisecond),
				fmt.Sprintf("%.1f MB", perPlugin),
				fmt.Sprintf("%.1f MB", float64(rss-baseRSS)/1024),
				fmt.Sprintf("%.1f MB", float64(after.HeapAlloc-before.HeapAlloc)/(1<<20)),
			)

			// Every one of them has to actually be serving, or this measures
			// the cost of processes that failed to start.
			for _, inst := range insts {
				if inst.ProcessExited() {
					t.Fatal("a plugin exited during the measurement")
				}
			}

			// Killed here rather than at test end, so the next size starts from
			// the same baseline instead of accumulating.
			for _, inst := range insts {
				inst.Kill()
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, c := childRSS(t); c <= baseCount {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		})
	}
}

// Start-up cost, which is what an operator waits through after a restart.
//
// Separate from the memory measurement because it answers a different
// question and wants a different shape: launching is serialised per plugin
// today, so the interesting property is whether the total is linear in the
// number of plugins or whether something in Core is quadratic.
func TestPluginStartupIsLinear(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE is not set")
	}

	timeFor := func(n int) time.Duration {
		insts := make([]*pluginhost.Instance, 0, n)
		start := time.Now()
		for i := range n {
			insts = append(insts, launchPlugin(t, fmt.Sprintf("startup-%d", i), "1.0.0", nil))
		}
		took := time.Since(start)
		for _, inst := range insts {
			inst.Kill()
		}
		return took
	}

	// Warm the page cache and the Go runtime, so the first measurement is not
	// paying for everything the later ones get for free.
	timeFor(2)

	one := timeFor(4)
	four := timeFor(16)

	perPluginSmall := one / 4
	perPluginLarge := four / 16
	t.Logf("4 plugins: %s (%s each); 16 plugins: %s (%s each)",
		one.Round(time.Millisecond), perPluginSmall.Round(time.Millisecond),
		four.Round(time.Millisecond), perPluginLarge.Round(time.Millisecond))

	// Generous: this is a laptop and process creation is noisy. What it rules
	// out is a per-plugin cost that grows with the number already running,
	// which is what a scan of everything on each launch would look like.
	if perPluginLarge > perPluginSmall*3 {
		t.Errorf("each plugin costs %s at 16 and %s at 4; start-up is worse than linear, "+
			"so something in Core walks the existing set on every launch",
			perPluginLarge, perPluginSmall)
	}
}

// A sanity check that runs by default: one plugin starts in a time an operator
// would accept. Not a benchmark — a floor, so a change that makes start-up
// pathological is noticed without anyone running the measurement.
func TestOnePluginStartsPromptly(t *testing.T) {
	start := time.Now()
	inst := launchPlugin(t, "prompt", "1.0.0", nil)
	took := time.Since(start)

	if inst.ProcessExited() {
		t.Fatal("the plugin exited immediately")
	}
	t.Logf("one plugin: %s from launch to ready", took.Round(time.Millisecond))

	// Two seconds is far above what it costs (tens of milliseconds) and far
	// below what anyone would tolerate for twenty of them.
	if took > 2*time.Second {
		t.Errorf("a plugin took %s to become ready; a Core with twenty of them would "+
			"be unavailable for the better part of a minute after a restart", took)
	}
}
