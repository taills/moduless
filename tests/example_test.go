package tests

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
	"gopkg.in/yaml.v3"
)

// These tests exercise the shipped ratelimit example the way an operator would
// actually install it: a directory containing a manifest and a binary, scanned
// off disk, enabled through the manager, and then serving live traffic.
//
// Nothing here reaches into the plugin's internals. If the example works, the
// framework works — and if the framework's documented behaviour and its real
// behaviour disagree, one of these fails rather than the disagreement surviving
// into someone's production.

// installExample builds one of the example plugins into a package directory
// laid out exactly as a distributed plugin would be.
func installExample(t *testing.T, root, name, source string) {
	t.Helper()
	installExampleAs(t, root, name, name, source)
}

// installExampleAs is installExample for an example whose source directory is
// not named after its plugin key.
func installExampleAs(t *testing.T, root, dirName, key, source string) {
	t.Helper()

	dir := filepath.Join(root, dirName)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// The binary is named for the manifest's entrypoint, which every example
	// declares as bin/<key>... except notes, which uses bin/plugin. Read the
	// manifest first and honour what it says rather than guessing.
	manifestSrc, err := os.ReadFile(filepath.Join(source, "manifest.yaml"))
	if err != nil {
		t.Fatalf("reading the example manifest: %v", err)
	}
	entrypoint := entrypointFrom(t, manifestSrc)

	build := exec.Command("go", "build", "-o", filepath.Join(dir, entrypoint), source)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("building the %s example: %v", key, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), manifestSrc, 0o644); err != nil {
		t.Fatalf("writing manifest: %v", err)
	}
}

// entrypointFrom reads runtime.entrypoint out of a manifest.
func entrypointFrom(t *testing.T, manifestSrc []byte) string {
	t.Helper()
	var m struct {
		Runtime struct {
			Entrypoint string `yaml:"entrypoint"`
		} `yaml:"runtime"`
	}
	if err := yaml.Unmarshal(manifestSrc, &m); err != nil {
		t.Fatalf("parsing the example manifest: %v", err)
	}
	if m.Runtime.Entrypoint == "" {
		t.Fatal("the example manifest declares no runtime.entrypoint")
	}
	return m.Runtime.Entrypoint
}

// exampleStack builds a Core running the ratelimit example over a plugin that
// serves ordinary traffic, which is the arrangement the example is for: a
// filter guarding somebody else's routes.
func exampleStack(t *testing.T, config map[string]string) (url string, mgr *pluginhost.Manager, cleanup func()) {
	t.Helper()

	root := t.TempDir()
	installExample(t, root, "ratelimit", "../extension-example/ratelimit")

	cfg := hostsvc.NewStaticConfig()
	cfg.Set("ratelimit", config)

	reg := pluginhost.NewRegistry()
	mgr = pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         root,
		DataDirRoot: filepath.Join(root, "data"),
		DevMode:     true,
		ConfigSource: func(key string) map[string]string {
			out, _ := cfg.Get(context.Background(), key)
			return out
		},
	}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{
			Config: cfg,
			Cache:  hostsvc.NewMemoryCache(100),
			Locks:  hostsvc.NewMemoryLocks(),
		})
	})

	mgr.Scan()
	if err := mgr.Enable(context.Background(), "ratelimit"); err != nil {
		t.Fatalf("enabling the ratelimit example: %v", err)
	}

	// A second plugin to be protected. The limiter has never heard of it,
	// which is the point: filters apply to the whole system.
	backend := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	return srv.URL, mgr, func() {
		srv.Close()
		mgr.Close()
		_ = mgr.Disable(context.Background(), "ratelimit")
	}
}

// The headline claim of the filter model: a plugin that knows nothing about
// another plugin's routes can still govern traffic to them.
func TestExampleRateLimitGuardsAnotherPlugin(t *testing.T) {
	url, _, cleanup := exampleStack(t, map[string]string{
		"requests_per_minute": "60",
		"burst":               "5",
	})
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}
	target := url + "/api/plugins/hello/items"

	var ok, limited int
	var retryAfter string
	for range 20 {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		switch resp.StatusCode {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
			if retryAfter == "" {
				retryAfter = resp.Header.Get("Retry-After")
			}
		default:
			t.Errorf("unexpected status %d", resp.StatusCode)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	t.Logf("%d allowed, %d limited (burst 5, 60/min)", ok, limited)
	if limited == 0 {
		t.Error("the limiter never rejected anything; a filter in one plugin did not reach another plugin's route")
	}
	if ok == 0 {
		t.Error("the limiter rejected everything, including the burst it should have allowed")
	}
	// The burst is spent first, so the early requests must be the successful
	// ones. Roughly: a handful through, the rest rejected.
	if ok > 10 {
		t.Errorf("%d requests passed a burst of 5; the bucket is not being consumed", ok)
	}
	if retryAfter == "" {
		t.Error("a 429 came back without Retry-After, so a caller cannot tell when to retry")
	} else if _, err := strconv.Atoi(retryAfter); err != nil {
		t.Errorf("Retry-After = %q, which is not a number of seconds", retryAfter)
	}
}

// A limiter that limits its own status page leaves an operator blind exactly
// when they need to look.
func TestExampleRateLimitExemptsItsOwnStatus(t *testing.T) {
	url, _, cleanup := exampleStack(t, map[string]string{
		"requests_per_minute": "60",
		"burst":               "2",
	})
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}

	// Exhaust the budget against the protected plugin.
	for range 10 {
		resp, err := client.Get(url + "/api/plugins/hello/items")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// The status page must still answer.
	resp, err := client.Get(url + "/api/plugins/ratelimit/stats")
	if err != nil {
		t.Fatalf("GET stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the limiter limited its own status page: %d", resp.StatusCode)
	}

	var stats struct {
		RequestsPerMinute float64 `json:"requests_per_minute"`
		Rejected          int64   `json:"rejected"`
		TrackedCallers    int     `json:"tracked_callers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decoding stats: %v", err)
	}

	t.Logf("stats: %.0f/min, %d rejected, %d callers tracked",
		stats.RequestsPerMinute, stats.Rejected, stats.TrackedCallers)

	// The config reached the plugin at launch. Before this was wired up the
	// SDK's GetConfig always returned an empty map, so the plugin silently ran
	// on its compiled-in defaults while the console showed the admin's values.
	if stats.RequestsPerMinute != 60 {
		t.Errorf("plugin reports %.0f requests/min, want the configured 60; "+
			"launch-time config did not reach it", stats.RequestsPerMinute)
	}
	if stats.Rejected == 0 {
		t.Error("stats show no rejections after the budget was exhausted")
	}
}

// Config changes must reach a running plugin without restarting it. During an
// incident, restarting the limiter to change the limit is the worst possible
// moment to drop it.
func TestExampleRateLimitConfigHotPush(t *testing.T) {
	url, mgr, cleanup := exampleStack(t, map[string]string{
		"requests_per_minute": "60",
		"burst":               "3",
	})
	defer cleanup()

	client := &http.Client{Timeout: 10 * time.Second}

	readRate := func() float64 {
		t.Helper()
		resp, err := client.Get(url + "/api/plugins/ratelimit/stats")
		if err != nil {
			t.Fatalf("GET stats: %v", err)
		}
		defer resp.Body.Close()
		var stats struct {
			RequestsPerMinute float64 `json:"requests_per_minute"`
			Burst             float64 `json:"burst"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
			t.Fatalf("decoding stats: %v", err)
		}
		return stats.RequestsPerMinute
	}

	if got := readRate(); got != 60 {
		t.Fatalf("starting rate = %.0f, want 60", got)
	}

	// Tighten the limit while the plugin is serving.
	if err := mgr.SetConfig(context.Background(), "ratelimit", map[string]string{
		"requests_per_minute": "600",
		"burst":               "50",
	}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	if got := readRate(); got != 600 {
		t.Errorf("rate after the push = %.0f, want 600; the change never reached the running process", got)
	}

	// And the new limit is actually in force, not just reported.
	var ok int
	for range 30 {
		resp, err := client.Get(url + "/api/plugins/hello/items")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		if resp.StatusCode == http.StatusOK {
			ok++
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	t.Logf("%d/30 allowed after raising the limit", ok)
	if ok < 20 {
		t.Errorf("only %d of 30 requests passed after raising the burst to 50; "+
			"the new settings were reported but not applied", ok)
	}
}

// Pushing config to a plugin that is not running must not be an error: there
// is simply nobody to tell, and the next launch reads the stored value.
func TestExampleRateLimitConfigPushToStoppedPlugin(t *testing.T) {
	_, mgr, cleanup := exampleStack(t, nil)
	defer cleanup()

	if err := mgr.Disable(context.Background(), "ratelimit"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := mgr.SetConfig(context.Background(), "ratelimit", map[string]string{"burst": "1"}); err != nil {
		t.Errorf("pushing config to a stopped plugin reported %v; it should be a no-op", err)
	}
	if err := mgr.SetConfig(context.Background(), "never-installed", nil); err != nil {
		t.Errorf("pushing config to an unknown plugin reported %v; it should be a no-op", err)
	}
}

// Every shipped example must be installable exactly as it stands, with no
// test-only edits. An example that does not load is worse than no example: it
// is the first thing a new plugin author copies.
func TestExampleManifestsAreValid(t *testing.T) {
	// The notes example lives in a directory named "plugin" but declares the
	// key "notes", and package directories must be named for their key.
	examples := []struct{ dir, key, source string }{
		{"ratelimit", "ratelimit", "../extension-example/ratelimit"},
		{"notes", "notes", "../extension-example/plugin"},
	}

	root := t.TempDir()
	for _, ex := range examples {
		t.Run(ex.key, func(t *testing.T) {
			installExampleAs(t, root, ex.dir, ex.key, ex.source)

			pkg, err := pluginhost.LoadPackage(filepath.Join(root, ex.dir))
			if err != nil {
				t.Fatalf("the shipped %s example does not load: %v", ex.key, err)
			}
			t.Logf("%s: %d filter(s), %d permission(s), %d job(s), %d collection(s)",
				pkg.Key(), len(pkg.Filters), len(pkg.Manifest.Permissions),
				len(pkg.Manifest.Jobs), len(pkg.Manifest.Database.Collections))
		})
	}
}

// Both examples must actually start: complete the handshake, accept their
// configuration and report ready. Compiling is not the same as booting, and a
// plugin that cannot boot fails in a way that points at Core rather than at
// the example.
//
// The notes example needs a database for its own routes, but not to start —
// so this covers start-up without requiring PostgreSQL.
func TestExamplesStart(t *testing.T) {
	for _, ex := range []struct{ dir, key, source string }{
		{"ratelimit", "ratelimit", "../extension-example/ratelimit"},
		{"notes", "notes", "../extension-example/plugin"},
	} {
		t.Run(ex.key, func(t *testing.T) {
			root := t.TempDir()
			installExampleAs(t, root, ex.dir, ex.key, ex.source)

			cfg := hostsvc.NewStaticConfig()
			reg := pluginhost.NewRegistry()
			mgr := pluginhost.NewManager(pluginhost.ManagerConfig{
				Dir:         root,
				DataDirRoot: filepath.Join(root, "data"),
				DevMode:     true,
			}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
				return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{
					Config: cfg,
					Cache:  hostsvc.NewMemoryCache(100),
					Locks:  hostsvc.NewMemoryLocks(),
				})
			})
			defer mgr.Close()

			mgr.Scan()
			if err := mgr.Enable(context.Background(), ex.key); err != nil {
				t.Fatalf("the shipped %s example does not start: %v", ex.key, err)
			}

			for _, st := range mgr.List() {
				if st.Key == ex.key && st.Ready != 1 {
					t.Errorf("%s started but reports %d ready replica(s)", ex.key, st.Ready)
				}
			}
		})
	}
}
