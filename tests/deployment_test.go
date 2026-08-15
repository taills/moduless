package tests

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Core as an operator meets it: a compiled binary, an environment, a plugin
// directory, and nothing else.
//
// Everything else in this suite assembles Core's packages by hand, which means
// main.go — the file that decides what is wired to what, which capabilities
// exist, and what happens when a backend is absent — had no coverage at all.
// The wiring is exactly where a working set of components can still add up to
// a Core that does not serve.
//
// This runs the documented minimal deployment: no DATABASE_URL, no object
// storage. Plugins run; capabilities needing a backend report Unavailable.

// freePort asks the kernel for a port and gives it straight back, which is the
// usual small race but the only way to tell a child process where to listen.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startCore builds and runs the real Core binary over a plugin directory.
func startCore(t *testing.T, pluginDir string, extraEnv ...string) (baseURL string, stop func()) {
	t.Helper()

	binary := filepath.Join(t.TempDir(), "core")
	build := exec.Command("go", "build", "-o", binary, "../core")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building Core: %v", err)
	}

	port := freePort(t)
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("HTTP_ADDR=127.0.0.1:%d", port),
		"PLUGIN_DIR="+pluginDir,
		// Without it, Pdeathsig on Linux would take the plugins down when the
		// test's own process tree is torn down at an awkward moment.
		"PLUGIN_DEV_MODE=1",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting Core: %v", err)
	}

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		// SIGTERM so Core runs its own shutdown, which is what a container
		// runtime sends and therefore the path worth exercising.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("Core did not exit within 15s of SIGTERM")
		}
	}
	t.Cleanup(stop)

	baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForCore(t, baseURL)
	return baseURL, stop
}

func waitForCore(t *testing.T, baseURL string) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("Core never became healthy")
}

// The documented minimal deployment, start to finish: Core boots with no
// database, finds the plugin on disk, starts it, and serves its routes.
func TestCoreServesAPluginWithoutADatabase(t *testing.T) {
	dir := t.TempDir()
	installFixturePackage(t, dir)

	baseURL, _ := startCore(t, dir)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/api/plugins/echo/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	t.Logf("plugin route: %d %s", resp.StatusCode, body)
	if resp.StatusCode != 200 {
		t.Fatalf("a plugin route returned %d on a Core started without a database", resp.StatusCode)
	}
	// The trace id is assigned by the gateway, so seeing it proves the request
	// went through the real pipeline rather than some fallback.
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("no trace id on the response; the pipeline is not in the path")
	}
	if got := resp.Header.Get("X-Echo-Path"); got != "/items" {
		t.Errorf("plugin saw path %q", got)
	}
}

// Capabilities that need a backend must refuse rather than crash the process
// they are missing from. This is the same check as the hostsvc tests, but
// through the binary, where it is main.go that decided the backend is absent.
func TestCoreReportsUnavailableCapabilities(t *testing.T) {
	dir := t.TempDir()
	installFixturePackage(t, dir)

	baseURL, _ := startCore(t, dir)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/api/plugins/echo/db")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	t.Logf("document store without a database: %d %s", resp.StatusCode, body)
	if resp.StatusCode == 200 {
		t.Error("the document store worked with no database configured")
	}
	// It must fail because this Core has no database, not because the plugin
	// forgot to ask for the capability. The fixture declares db precisely so
	// this test reaches the backend check; asserting only "not 200" would pass
	// on a permission refusal and prove nothing about the deployment.
	if !strings.Contains(string(body), "not configured") {
		t.Errorf("refused for the wrong reason: %q", body)
	}

	// Core is still healthy afterwards.
	health, err := client.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("Core stopped answering: %v", err)
	}
	health.Body.Close()
	if health.StatusCode != 200 {
		t.Errorf("/healthz = %d after an unavailable capability", health.StatusCode)
	}
}

// SIGTERM is what a container runtime sends. Core must drain and exit rather
// than being killed, or every deploy truncates whatever was in flight.
func TestCoreShutsDownOnSIGTERM(t *testing.T) {
	dir := t.TempDir()
	installFixturePackage(t, dir)

	baseURL, stop := startCore(t, dir)

	// Prove it is serving, then ask it to stop. stop() fails the test itself
	// if the process does not exit in time.
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/api/plugins/echo/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	start := time.Now()
	stop()
	t.Logf("Core exited %s after SIGTERM", time.Since(start).Round(time.Millisecond))

	// The port must be free afterwards: a Core that exits while its listener
	// lingers makes the next deploy fail to bind.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Get(baseURL + "/healthz"); err != nil {
			return // refused, as it should be
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("Core is still answering after SIGTERM")
}

// A broken plugin package must not stop Core from booting. This is the case
// where being strict locks an operator out of the console they need in order
// to remove the broken plugin.
func TestCoreBootsWithABrokenPlugin(t *testing.T) {
	dir := t.TempDir()
	installFixturePackage(t, dir)

	broken := filepath.Join(dir, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "manifest.yaml"),
		[]byte("key: [not valid yaml"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	baseURL, _ := startCore(t, dir)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/api/plugins/echo/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("the working plugin returned %d; a broken package next to it stopped Core serving", resp.StatusCode)
	}
}

// An empty plugin directory is the very first thing an operator has: Core
// installed, nothing added yet. It must boot and serve.
func TestCoreBootsWithNoPlugins(t *testing.T) {
	baseURL, _ := startCore(t, t.TempDir())

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/healthz = %d with no plugins installed", resp.StatusCode)
	}

	// And a request for a plugin that does not exist is refused cleanly.
	missing, err := client.Get(baseURL + "/api/plugins/nothing/here")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode == 200 {
		t.Error("a request for a non-existent plugin succeeded")
	}
}
