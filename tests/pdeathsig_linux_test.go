//go:build linux

package tests

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Pdeathsig: the kernel kills a plugin when Core dies.
//
// This is the only thing standing between a Core crash and a machine full of
// orphaned plugin processes, each holding its socket and its memory, waiting to
// collide with the next Core that starts. It is also Linux-only, set behind a
// build tag, and skipped entirely in DevMode — which every other test in this
// repo runs with, because taking every plugin down on each rebuild makes the
// edit loop unusable.
//
// So the one code path that matters in production had never once been
// executed. This test is the only thing that runs it.

// countPluginProcesses counts running processes whose command line mentions
// the fixture binary. Reading /proc keeps this free of assumptions about which
// process tools the image happens to ship.
func countPluginProcesses(t *testing.T, needle string) int {
	t.Helper()

	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}

	count := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Process directories are named by pid; skip everything else.
		if e.Name()[0] < '0' || e.Name()[0] > '9' {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue // the process ended between listing and reading
		}
		if strings.Contains(string(cmdline), needle) {
			count++
		}
	}
	return count
}

func TestPdeathsigKillsOrphanedPlugins(t *testing.T) {
	dir := t.TempDir()

	host := filepath.Join(dir, "orphanhost")
	build := exec.Command("go", "build", "-o", host, "./fixtures/orphanhost")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building orphanhost: %v", err)
	}

	// A private copy of the plugin binary, so the count below is not confused
	// by plugins other tests are running.
	plugin := filepath.Join(dir, "orphanplugin")
	data, err := os.ReadFile(pluginBinary)
	if err != nil {
		t.Fatalf("read the fixture binary: %v", err)
	}
	if err := os.WriteFile(plugin, data, 0o755); err != nil {
		t.Fatalf("write the plugin copy: %v", err)
	}

	before := countPluginProcesses(t, plugin)
	if before != 0 {
		t.Fatalf("%d processes already match %s", before, plugin)
	}

	cmd := exec.Command(host, plugin)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start orphanhost: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// Wait for it to report the plugin is up.
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
		}
		close(ready)
	}()
	select {
	case line := <-ready:
		if line != "READY" {
			t.Fatalf("orphanhost said %q", line)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("orphanhost never reported the plugin as started")
	}

	// The count includes orphanhost itself, since the plugin path is on its
	// command line. That does not weaken the assertion below: if the plugin
	// outlived its parent the count would be one, not zero.
	if n := countPluginProcesses(t, plugin); n == 0 {
		t.Fatal("the plugin process is not running, so this test would prove nothing")
	} else {
		t.Logf("%d process(es) reference the plugin (orphanhost plus its child)", n)
	}

	// Kill the stand-in Core the way a crash would: no signal handler runs, no
	// deferred cleanup, no chance to stop anything.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("killing orphanhost: %v", err)
	}
	_, _ = cmd.Process.Wait()

	// Nothing in this repository is now responsible for that plugin. If it
	// stops, the kernel stopped it.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if countPluginProcesses(t, plugin) == 0 {
			t.Log("the plugin died with its parent, as Pdeathsig asks")
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Errorf("%d plugin process(es) outlived the Core that started them; "+
		"Pdeathsig is not in effect and a Core crash would leak processes",
		countPluginProcesses(t, plugin))
}
