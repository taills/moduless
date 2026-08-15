package tests

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// Start-up behaviour: what Core does with a directory of plugin packages.
//
// This is the path every restart takes, and the one an operator meets first.
// It has to be forgiving in a specific way — a broken package must be visible
// and skipped, not fatal — because the alternative is that one bad plugin
// stops the whole system from starting, which is exactly when nobody can get
// into the console to remove it.

func newManagerOver(t *testing.T, root string, cfg map[string]string) (*pluginhost.Manager, *pluginhost.Registry) {
	t.Helper()

	store := hostsvc.NewStaticConfig()
	for key := range cfg {
		store.Set(key, cfg)
	}

	reg := pluginhost.NewRegistry()
	mgr := pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         root,
		DataDirRoot: filepath.Join(root, "data"),
		DevMode:     true,
		ConfigSource: func(key string) map[string]string {
			out, _ := store.Get(context.Background(), key)
			return out
		},
	}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{
			Config: store,
			Cache:  hostsvc.NewMemoryCache(100),
			Locks:  hostsvc.NewMemoryLocks(),
		})
	})
	t.Cleanup(mgr.Close)
	return mgr, reg
}

// Core restarting must bring every plugin back from what is on disk. Nothing
// about a running plugin is persisted, so this is the only path that exists.
func TestStartupRestoresPluginsFromDisk(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	// First boot.
	mgr1, reg1 := newManagerOver(t, root, nil)
	mgr1.Scan()
	if err := mgr1.EnableAll(context.Background()); err != nil {
		t.Fatalf("first boot: %v", err)
	}

	srv1 := newGateway(reg1)
	if code := getStatus(t, warmClientPlain(), srv1.URL+"/api/plugins/echo/items"); code != 200 {
		t.Fatalf("plugin not serving after the first boot: %d", code)
	}
	srv1.Close()

	// Shut down the way Core would: stop routing and drain.
	if err := mgr1.Disable(context.Background(), "echo"); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	mgr1.Close()

	// Second boot, over the same directory, with no shared state.
	mgr2, reg2 := newManagerOver(t, root, nil)
	mgr2.Scan()
	if err := mgr2.EnableAll(context.Background()); err != nil {
		t.Fatalf("second boot: %v", err)
	}

	srv2 := newGateway(reg2)
	defer srv2.Close()

	if code := getStatus(t, warmClientPlain(), srv2.URL+"/api/plugins/echo/items"); code != 200 {
		t.Errorf("plugin did not come back after a restart: %d", code)
	}

	// And it is a genuinely new process, not the old one still running.
	for _, st := range mgr2.List() {
		if st.Key == "echo" {
			t.Logf("after restart: enabled=%v replicas=%d ready=%d", st.Enabled, st.Replicas, st.Ready)
			if st.Ready != 1 {
				t.Errorf("ready replicas = %d, want 1", st.Ready)
			}
		}
	}
}

// One unloadable package must not stop the others. This is the case where
// being strict would be actively harmful: a Core that refuses to boot cannot
// serve the console an operator needs in order to remove the bad plugin.
func TestStartupSkipsBrokenPackages(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	// A directory that is not a plugin at all. This is what Core's own data
	// directory looks like when PLUGIN_DATA_DIR is placed inside PLUGIN_DIR,
	// and it must be ignored rather than reported as a broken plugin: an
	// operator cannot act on it, and noise here hides the failures they can
	// act on.
	if err := os.MkdirAll(filepath.Join(root, "not-a-plugin", "sub"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// A package whose manifest is not valid YAML.
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(broken, "manifest.yaml"),
		[]byte("key: [this is not\n  valid yaml"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A package whose manifest is fine but whose binary is missing.
	missing := filepath.Join(root, "missing")
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(missing, "manifest.yaml"), []byte(
		"key: missing\nversion: 1.0.0\nruntime:\n  entrypoint: bin/nope\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	mgr, reg := newManagerOver(t, root, nil)
	mgr.Scan()
	_ = mgr.EnableAll(context.Background()) // errors are expected for the broken ones

	srv := newGateway(reg)
	defer srv.Close()

	if code := getStatus(t, warmClientPlain(), srv.URL+"/api/plugins/echo/items"); code != 200 {
		t.Errorf("the working plugin is not serving (%d); a broken package stopped a healthy one", code)
	}

	// Both failures must be reported, or an operator has no way to find out
	// why a plugin they installed never appeared.
	reported := map[string]string{}
	for _, st := range mgr.List() {
		if st.LoadError != "" {
			reported[st.Key] = st.LoadError
		}
	}
	t.Logf("reported load failures: %v", reported)

	for _, key := range []string{"broken", "missing"} {
		if _, ok := reported[key]; !ok {
			t.Errorf("package %q failed to load but is not reported; it would be invisible in the console", key)
		}
	}
	if msg, ok := reported["not-a-plugin"]; ok {
		t.Errorf("a directory with no manifest was reported as a broken plugin: %s", msg)
	}
}

// Rescanning must pick up a package added after Core started, so installing a
// plugin does not require a restart.
func TestRescanFindsNewPackages(t *testing.T) {
	root := t.TempDir()

	mgr, reg := newManagerOver(t, root, nil)
	mgr.Scan()
	if n := len(mgr.List()); n != 0 {
		t.Fatalf("%d plugins found in an empty directory", n)
	}

	installFixturePackage(t, root)
	mgr.Scan()

	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enabling a plugin installed after start-up: %v", err)
	}

	srv := newGateway(reg)
	defer srv.Close()
	if code := getStatus(t, warmClientPlain(), srv.URL+"/api/plugins/echo/items"); code != 200 {
		t.Errorf("a plugin installed without restarting Core is not serving: %d", code)
	}
}

// Enabling a plugin that was never installed must fail cleanly rather than
// panicking or leaving a half-registered entry behind.
func TestEnableUnknownPluginFails(t *testing.T) {
	root := t.TempDir()
	mgr, reg := newManagerOver(t, root, nil)
	mgr.Scan()

	err := mgr.Enable(context.Background(), "does-not-exist")
	if err == nil {
		t.Fatal("enabling a plugin that is not installed reported success")
	}
	t.Logf("enable of an unknown plugin: %v", err)

	if reg.Current().Has("does-not-exist") {
		t.Error("a failed enable left the plugin in the routing table")
	}
}

// A plugin that fails its own start-up handshake must not enter rotation, and
// must not leave a stray process behind.
func TestFailedConfigureIsNotPublished(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	store := hostsvc.NewStaticConfig()
	reg := pluginhost.NewRegistry()
	mgr := pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         root,
		DataDirRoot: filepath.Join(root, "data"),
		DevMode:     true,
		// The fixture refuses to become ready when this is set, which is how a
		// plugin reports that its own start-up failed.
		BaseEnv: []string{"PATH=/usr/bin:/bin", "ECHO_FAIL_CONFIGURE=1"},
	}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{Config: store})
	})
	defer mgr.Close()

	mgr.Scan()
	err := mgr.Enable(context.Background(), "echo")
	if err == nil {
		t.Fatal("a plugin that failed Configure was enabled anyway")
	}
	t.Logf("enable of a plugin that fails its handshake: %v", err)

	if reg.Current().Has("echo") {
		t.Error("a plugin that never became ready is in the routing table")
	}
}

// Disabling twice must be harmless. An admin double-clicking, or a retry after
// a timeout, should not produce an error or a hang.
func TestDisableIsIdempotent(t *testing.T) {
	root := t.TempDir()
	installFixturePackage(t, root)

	mgr, _ := newManagerOver(t, root, nil)
	mgr.Scan()
	if err := mgr.Enable(context.Background(), "echo"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := mgr.Disable(ctx, "echo"); err != nil {
		t.Fatalf("first disable: %v", err)
	}
	if err := mgr.Disable(ctx, "echo"); err != nil {
		t.Errorf("second disable reported %v; disabling an already-disabled plugin should be a no-op", err)
	}
	if err := mgr.Disable(ctx, "never-installed"); err != nil {
		t.Errorf("disabling an unknown plugin reported %v; it should be a no-op", err)
	}
}
