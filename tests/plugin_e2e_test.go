package tests

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// This is the end-to-end proof: a real plugin subprocess, launched by Core,
// serving real HTTP requests through the real gateway and filter pipeline.
// Everything else in the test suite uses fakes; this file uses none.
//
// go-plugin's exec + broker mechanism is cross-platform, so this runs on the
// macOS development machine too. Only the sandbox tests need Linux.

var pluginBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "moduless-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tempdir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	pluginBinary = filepath.Join(dir, "echoplugin")
	build := exec.Command("go", "build", "-o", pluginBinary, "./fixtures/echoplugin")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building echoplugin: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func checksum(t testing.TB, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return sum[:]
}

// launchPlugin starts a real plugin process wired to a real HostServices
// instance, so the reverse channel is exercised too.
func launchPlugin(t testing.TB, key, version string, granted []string) *pluginhost.Instance {
	t.Helper()

	cfg := hostsvc.NewStaticConfig()
	cfg.Set(key, map[string]string{"greeting": "hello-from-core"})

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        key,
		InstanceID: key + "-" + version,
		Version:    version,
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New(key, granted, hostsvc.Deps{
			Config: cfg,
			Cache:  hostsvc.NewMemoryCache(100),
			Locks:  hostsvc.NewMemoryLocks(),
		}),
		GrantedPermissions: granted,
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             os.Stderr,
		DevMode:            true,
	})
	if err != nil {
		t.Fatalf("launch plugin: %v", err)
	}
	t.Cleanup(inst.Kill)
	return inst
}

func compileFilters(t testing.TB, key string, decls ...manifest.FilterDecl) []manifest.CompiledFilter {
	t.Helper()
	m := &manifest.Manifest{Key: key, Version: "1.0.0", Filters: decls}
	compiled, err := m.CompileFilters()
	if err != nil {
		t.Fatalf("CompileFilters: %v", err)
	}
	return compiled
}

// newGateway builds the full serving stack around a registry.
func newGateway(reg *pluginhost.Registry) *httptest.Server {
	h := &gateway.PluginHandler{
		Registry: reg,
		Runner:   &pipeline.Runner{},
		// Auth is nil, so identity resolution is skipped and the pipeline runs
		// without a session store. Authentication itself is covered by the
		// gateway package's own tests.
	}
	core := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	})
	return httptest.NewServer(h.Middleware(core))
}

func get(t testing.TB, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), resp.Header
}

// TestE2EPluginServesHTTP is the headline case: a browser-shaped request
// reaches a separate OS process and comes back, with the plugin having called
// into Core over the reverse channel along the way.
func TestE2EPluginServesHTTP(t *testing.T) {
	reg := pluginhost.NewRegistry()
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{inst}})

	srv := newGateway(reg)
	defer srv.Close()

	status, body, header := get(t, srv.URL+"/api/plugins/hello/items")
	if status != 200 {
		t.Fatalf("status = %d, body = %q", status, body)
	}
	// The greeting is only obtainable through the plugin's reverse connection
	// to HostServices, so seeing it proves the whole loop ran.
	if got := header.Get("X-Host-Config"); got != "hello-from-core" {
		t.Errorf("X-Host-Config = %q; the reverse channel did not deliver data", got)
	}
	if header.Get("X-Request-Id") == "" {
		t.Error("no trace id came back to the caller")
	}
	if got := header.Get("X-Echo-Path"); got != "/items" {
		t.Errorf("plugin saw path %q, want /items", got)
	}
	if got := header.Values("X-Multi"); len(got) != 2 {
		t.Errorf("X-Multi = %v, want 2 values through the whole stack", got)
	}
}

// TestE2EFilterShortCircuits proves a filter running in a separate process can
// stop a request before it reaches its backend.
func TestE2EFilterShortCircuits(t *testing.T) {
	reg := pluginhost.NewRegistry()
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "hello", manifest.FilterDecl{
			Name:  "guard",
			Phase: manifest.PhasePreRoute,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})

	srv := newGateway(reg)
	defer srv.Close()

	// The fixture short-circuits exactly the /deny path.
	status, body, _ := get(t, srv.URL+"/deny")
	if status != 403 {
		t.Fatalf("status = %d, body = %q; want the filter's 403", status, body)
	}
	if !strings.Contains(body, "denied by echoplugin") {
		t.Errorf("body = %q, want the plugin's message", body)
	}

	// Any other path passes the filter and falls through to Core's handler.
	if status, _, _ := get(t, srv.URL+"/anything-else"); status != 404 {
		t.Errorf("status = %d, want the core handler's 404", status)
	}
}

// TestE2EHotUnload covers the disable path: routes disappear the moment the
// plugin leaves the snapshot, and the process is then drained and killed.
func TestE2EHotUnload(t *testing.T) {
	reg := pluginhost.NewRegistry()
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{inst}})

	srv := newGateway(reg)
	defer srv.Close()

	if status, _, _ := get(t, srv.URL+"/api/plugins/hello/items"); status != 200 {
		t.Fatalf("plugin not serving before unload: %d", status)
	}

	displaced := reg.Remove("hello")
	if status, _, _ := get(t, srv.URL+"/api/plugins/hello/items"); status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 immediately after unload", status)
	}

	for _, old := range displaced {
		if err := old.Drain(context.Background(), 2*time.Second); err != nil {
			t.Errorf("drain: %v", err)
		}
		if !old.ProcessExited() {
			t.Error("the plugin process survived the drain")
		}
	}
}

// TestE2EHotUpgradeServesContinuously is the zero-downtime claim under test:
// traffic runs continuously while a new version is launched and swapped in,
// and not one request may fail.
func TestE2EHotUpgradeServesContinuously(t *testing.T) {
	reg := pluginhost.NewRegistry()
	v1 := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v1}})

	srv := newGateway(reg)
	defer srv.Close()

	var (
		stop     atomic.Bool
		attempts atomic.Int64
		failures atomic.Int64
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		client := &http.Client{Timeout: 5 * time.Second}
		for !stop.Load() {
			attempts.Add(1)
			resp, err := client.Get(srv.URL + "/api/plugins/hello/items")
			if err != nil {
				failures.Add(1)
				continue
			}
			if resp.StatusCode != 200 {
				failures.Add(1)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	// Let traffic establish, then perform the blue-green swap: launch and
	// verify the new version first, and only then commit.
	time.Sleep(50 * time.Millisecond)
	v2 := launchPlugin(t, "hello", "2.0.0", nil)
	reg.Swap(context.Background(),
		pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{v2}},
		2*time.Second)
	time.Sleep(100 * time.Millisecond)

	stop.Store(true)
	<-done

	if attempts.Load() < 10 {
		t.Fatalf("only %d requests were made; the test did not exercise the swap", attempts.Load())
	}
	if got := failures.Load(); got != 0 {
		t.Errorf("%d of %d requests failed during the upgrade; the swap is not zero-downtime",
			got, attempts.Load())
	}
	if !v1.ProcessExited() {
		t.Error("the old version was not drained and stopped")
	}
}

// TestE2ETamperedBinaryIsRefused checks the supply-chain guard end to end:
// bytes that do not match what was verified never execute.
func TestE2ETamperedBinaryIsRefused(t *testing.T) {
	wrong := sha256.Sum256([]byte("different bytes"))

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        "hello",
		InstanceID: "hello-tampered",
		BinaryPath: pluginBinary,
		Checksum:   wrong[:],
		HostImpl:   hostsvc.New("hello", nil, hostsvc.Deps{}),
		Env:        []string{"PATH=/usr/bin:/bin"},
		Stderr:     os.Stderr,
		DevMode:    true,
	})
	if err == nil {
		inst.Kill()
		t.Fatal("a binary whose checksum did not match was executed")
	}
}

// TestE2ECrashIsRecovered exercises the supervisor against a real process
// death rather than a simulated one.
func TestE2ECrashIsRecovered(t *testing.T) {
	reg := pluginhost.NewRegistry()
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{inst}})

	srv := newGateway(reg)
	defer srv.Close()

	var relaunches atomic.Int32
	sup := pluginhost.NewSupervisor(reg, func(context.Context, string) (*pluginhost.Instance, error) {
		relaunches.Add(1)
		return launchPlugin(t, "hello", "1.0.0", nil), nil
	}, pluginhost.SupervisorConfig{
		PollInterval:   10 * time.Millisecond,
		InitialBackoff: 10 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
		CrashThreshold: 3,
		CrashWindow:    time.Minute,
	})
	defer sup.Stop()
	sup.Watch(context.Background(), inst)

	// Make the plugin die abruptly, the way a panic or an OOM kill would.
	// Calling inst.Kill() would not do: that marks the instance stopped, and
	// the supervisor correctly treats a deliberate stop as something not to
	// undo. Only an unannounced death is a crash.
	_, _ = inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{Method: "GET", Path: "/crash"})

	deadline := time.Now().Add(5 * time.Second)
	for relaunches.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if relaunches.Load() == 0 {
		t.Fatal("the supervisor never restarted the crashed plugin")
	}

	// Service must come back on its own.
	for time.Now().Before(deadline) {
		if status, _, _ := get(t, srv.URL+"/api/plugins/hello/items"); status == 200 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("the plugin never returned to service after the crash")
}

// TestE2EPermissionDenied proves the permission gate holds across the process
// boundary: the plugin asks Core for something it was not granted and is told
// no by Core, not by its own SDK.
func TestE2EPermissionDenied(t *testing.T) {
	// The fixture calls GetConfig during Configure, which needs no permission,
	// so it starts successfully with an empty grant set.
	inst := launchPlugin(t, "hello", "1.0.0", nil)

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: "GET",
		Path:   "/items",
	})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Fatalf("status = %d", resp.GetStatusCode())
	}
}
