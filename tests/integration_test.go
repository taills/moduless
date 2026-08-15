package tests

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/event"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// All three examples in one Core, which is the arrangement they are meant for
// and the only one nothing had exercised.
//
// Each has been tested on its own. What only appears together is the
// interaction: the rate limiter governs traffic to plugins it has never heard
// of, the audit plugin records requests to all of them including the limiter's
// own, and one plugin being upgraded or crashing has to leave the others
// serving. A framework whose parts each work in isolation is not the same as
// one whose parts work at the same time.

// stackOfThree installs every shipped example against one registry, one
// database and one gateway.
func stackOfThree(t *testing.T, config map[string]map[string]string) (url string, mgr *pluginhost.Manager, reg *pluginhost.Registry) {
	t.Helper()

	handle := requireDB(t)
	root := t.TempDir()

	for _, ex := range []struct{ dir, source string }{
		{"ratelimit", "../extension-example/ratelimit"},
		{"notes", "../extension-example/notes"},
		{"audit", "../extension-example/audit"},
	} {
		installExampleAs(t, root, ex.dir, ex.dir, ex.source)
	}

	cfg := hostsvc.NewStaticConfig()
	for key, settings := range config {
		cfg.Set(key, settings)
	}

	cmds := db.NewCMDSManager(handle)
	txs := db.NewTxRegistry()
	t.Cleanup(txs.Close)
	data := hostsvc.NewCMDSData(handle, cmds, txs)

	reg = pluginhost.NewRegistry()
	mgr = pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         root,
		DataDirRoot: filepath.Join(root, "data"),
		DevMode:     true,
		Supervisor:  pluginhost.SupervisorConfig{PollInterval: 20 * time.Millisecond},
		ConfigSource: func(key string) map[string]string {
			out, _ := cfg.Get(context.Background(), key)
			return out
		},
	}, reg, func(pkg *pluginhost.Package) pb.HostServicesServer {
		// The same provisioning Core does, so a plugin's first write does not
		// meet a missing table.
		var collections []db.CollectionSchema
		for _, c := range pkg.Manifest.Database.Collections {
			collections = append(collections, db.CollectionSchema{Name: c.Name})
		}
		if len(collections) > 0 {
			if err := data.ProvisionSchema(pkg.Key(), collections); err != nil {
				t.Errorf("provisioning %s: %v", pkg.Key(), err)
			}
		}
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostsvc.Deps{
			Config: cfg,
			Data:   data,
			Queue:  hostsvc.NewPGQueue(db.NewQueue(handle)),
			Events: hostsvc.NewBusEvents(event.NewEventBus()),
			Cache:  hostsvc.NewMemoryCache(200),
			Locks:  hostsvc.NewMemoryLocks(),
		})
	})
	t.Cleanup(mgr.Close)

	mgr.Scan()
	if err := mgr.EnableAll(context.Background()); err != nil {
		t.Fatalf("enabling the examples: %v", err)
	}

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL, mgr, reg
}

// The whole arrangement working at once: a request to one plugin passes
// through another's filter and is recorded by a third.
func TestThreePluginsServeTogether(t *testing.T) {
	url, mgr, reg := stackOfThree(t, map[string]map[string]string{
		// Generous, so the limiter is present without being what the test
		// measures.
		"ratelimit": {"requests_per_minute": "6000", "burst": "500"},
	})

	for _, st := range mgr.List() {
		t.Logf("%s: enabled=%v ready=%d filters=%d jobs=%d",
			st.Key, st.Enabled, st.Ready, st.Filters, st.Jobs)
		if !st.Enabled || st.Ready == 0 {
			t.Errorf("%s is not serving", st.Key)
		}
	}

	client := warmClientPlain()

	// Each plugin answers its own routes while the other two are installed.
	for _, probe := range []struct{ name, path string }{
		{"ratelimit", "/api/plugins/ratelimit/stats"},
		{"notes", "/api/plugins/notes/notes"},
		{"audit", "/api/plugins/audit/entries"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			code := getStatus(t, client, url+probe.path)
			// audit's own route is admin-only and this stack has no auth, so a
			// 403 is the correct answer there rather than a failure.
			if code != 200 && code != 403 {
				t.Errorf("%s returned %d", probe.path, code)
			}
			t.Logf("%s -> %d", probe.path, code)
		})
	}

	// Unauthenticated traffic, which is what this stack serves, must not kill
	// anything. A plugin reading the caller is the most ordinary code there
	// is, and reading it wrongly panics — which in a plugin is not a 500 but a
	// dead process. This caught exactly that in the audit example.
	for range 20 {
		getStatus(t, client, url+"/api/plugins/notes/notes")
		postTo(t, client, url+"/api/plugins/notes/notes")
	}
	time.Sleep(300 * time.Millisecond)

	for _, key := range []string{"ratelimit", "notes", "audit"} {
		for _, inst := range reg.Current().Replicas(key) {
			if inst.ProcessExited() {
				t.Errorf("the %s plugin died while serving anonymous requests", key)
			}
		}
	}
}

func postTo(t *testing.T, client *http.Client, url string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// The rate limiter governs plugins it knows nothing about. This is the claim
// the filter model exists for, and with three plugins installed it can be
// checked against a real neighbour rather than a fixture.
func TestRateLimiterGovernsTheOtherExamples(t *testing.T) {
	url, _, _ := stackOfThree(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "60", "burst": "4"},
	})

	client := warmClientPlain()

	var ok, limited int
	for range 20 {
		switch getStatus(t, client, url+"/api/plugins/notes/notes") {
		case http.StatusTooManyRequests:
			limited++
		case 200:
			ok++
		}
	}

	t.Logf("notes traffic under the limiter: %d allowed, %d limited", ok, limited)
	if limited == 0 {
		t.Error("the rate limiter did not govern another plugin's routes")
	}
	if ok == 0 {
		t.Error("the limiter rejected everything, including the burst")
	}

	// Its own status route stays reachable, which is what makes an incident
	// diagnosable.
	if code := getStatus(t, client, url+"/api/plugins/ratelimit/stats"); code != 200 {
		t.Errorf("the limiter's own status returned %d while it was limiting", code)
	}
}

// The audit plugin records write requests to every plugin, including ones that
// were refused. Its own log-phase filter runs after the response, so a request
// the limiter rejected is still auditable.
func TestAuditRecordsAcrossPlugins(t *testing.T) {
	handle := requireDB(t)
	if _, err := handle.Exec(`TRUNCATE ext_audit_audit_log`); err != nil {
		t.Logf("clearing earlier audit rows: %v", err)
	}

	url, _, _ := stackOfThree(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "6000", "burst": "500"},
	})

	client := warmClientPlain()

	// Writes to two different plugins.
	for _, target := range []string{
		url + "/api/plugins/notes/notes",
		url + "/api/plugins/audit/entries",
	} {
		req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(`{"title":"x"}`))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", target, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// The log phase is asynchronous, so give it a moment to land.
	var paths []string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		paths = auditedPaths(t, handle)
		if len(paths) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	t.Logf("audited paths: %v", paths)
	if len(paths) < 2 {
		t.Fatalf("%d write(s) recorded, want both", len(paths))
	}

	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "/notes") {
		t.Error("a write to the notes plugin was not audited")
	}
	if !strings.Contains(joined, "/audit") {
		t.Error("a write to the audit plugin itself was not audited")
	}
}

// auditedPaths reads the paths the audit plugin recorded. It goes to the table
// directly rather than through the plugin's own API, which is admin-only.
func auditedPaths(t *testing.T, handle *sql.DB) []string {
	t.Helper()

	rows, err := handle.Query(`SELECT data->>'path' FROM ext_audit_audit_log`)
	if err != nil {
		t.Fatalf("reading audit rows: %v", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var path sql.NullString
		if err := rows.Scan(&path); err != nil {
			t.Fatalf("scanning audit row: %v", err)
		}
		if path.Valid {
			out = append(out, path.String)
		}
	}
	return out
}

// One plugin being upgraded must not disturb the others. This is the operation
// an operator performs most often, and the one where a shared registry could
// most plausibly leak between plugins.
func TestUpgradingOnePluginLeavesOthersServing(t *testing.T) {
	url, mgr, _ := stackOfThree(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "6000", "burst": "500"},
	})

	var (
		stop     atomic.Bool
		attempts atomic.Int64
		failures atomic.Int64
		wg       sync.WaitGroup
	)
	client := warmClientPlain()
	for range 3 {
		wg.Go(func() {
			for !stop.Load() {
				attempts.Add(1)
				// Traffic aimed at a plugin that is not the one being upgraded.
				if code := getStatus(t, client, url+"/api/plugins/ratelimit/stats"); code != 200 {
					failures.Add(1)
				}
			}
		})
	}

	time.Sleep(100 * time.Millisecond)
	if err := mgr.Upgrade(context.Background(), "notes"); err != nil {
		t.Fatalf("upgrading notes: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	stop.Store(true)
	wg.Wait()

	t.Logf("%d requests to ratelimit during a notes upgrade, %d failed",
		attempts.Load(), failures.Load())
	if failures.Load() > 0 {
		t.Errorf("%d of %d requests to an unrelated plugin failed during an upgrade",
			failures.Load(), attempts.Load())
	}
}

// One plugin crashing must not take the others with it. Separate processes are
// the reason to pay the cost of this architecture at all.
func TestOnePluginCrashingLeavesOthersServing(t *testing.T) {
	url, _, reg := stackOfThree(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "6000", "burst": "500"},
	})

	client := warmClientPlain()
	if code := getStatus(t, client, url+"/api/plugins/notes/notes"); code != 200 {
		t.Fatalf("notes is not serving before the crash: %d", code)
	}

	// Kill the audit plugin's process outright.
	for _, inst := range reg.Current().Replicas("audit") {
		inst.Kill()
	}

	// The other two carry on.
	for range 10 {
		if code := getStatus(t, client, url+"/api/plugins/notes/notes"); code != 200 {
			t.Errorf("notes returned %d after the audit plugin was killed", code)
		}
		if code := getStatus(t, client, url+"/api/plugins/ratelimit/stats"); code != 200 {
			t.Errorf("ratelimit returned %d after the audit plugin was killed", code)
		}
	}
}
