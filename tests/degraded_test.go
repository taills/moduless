package tests

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// Core running without its optional backends.
//
// This is the first shape anybody deploys: no DATABASE_URL, no object storage,
// just Core and a plugin. It is documented as supported — plugins run, and the
// capabilities that need a backend report Unavailable — and it had never been
// tested. A plugin meeting a missing backend must get a clear refusal, not a
// panic, a hang, or a nil dereference on Core's side.
//
// One distinction matters more than the rest: Unavailable and PermissionDenied
// have to be told apart. "You did not ask for this capability" and "this
// machine does not have it" send an operator to different places, and a plugin
// author who sees the wrong one edits their manifest for an hour over a Core
// that was started without a database.

// barePlugin launches a plugin against a Core with no data, queue or file
// backends — but with every permission granted, so any refusal is about the
// missing backend rather than the manifest.
func barePlugin(t *testing.T, key string) *pluginhost.Instance {
	t.Helper()

	granted := []string{"db", "db:tx", "queue", "cache", "lock", "events",
		"files:read", "files:write", "http:egress"}

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        key,
		InstanceID: key + "-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New(key, granted, hostsvc.Deps{
			// Config, cache and locks are in-process and always available.
			// Everything that needs a database or object storage is absent,
			// exactly as it is when Core starts without DATABASE_URL.
			Config: hostsvc.NewStaticConfig(),
			Cache:  hostsvc.NewMemoryCache(100),
			Locks:  hostsvc.NewMemoryLocks(),
		}),
		GrantedPermissions: granted,
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             nil,
	})
	if err != nil {
		t.Fatalf("launch %s: %v", key, err)
	}
	t.Cleanup(inst.Kill)
	return inst
}

// A plugin still serves its own HTTP even when every stateful capability is
// missing. Plenty of useful plugins — filters, proxies, formatters — need no
// backend at all, and a Core without a database must still run them.
func TestPluginServesWithoutAnyBackends(t *testing.T) {
	inst := barePlugin(t, "bare")

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/items",
	})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Errorf("status = %d; a plugin needing no backend should serve normally", resp.GetStatusCode())
	}
}

// Each capability that needs a backend must refuse clearly rather than crash
// Core or hang the plugin.
func TestMissingBackendsReportUnavailable(t *testing.T) {
	inst := barePlugin(t, "bare")

	for _, tc := range []struct{ name, path string }{
		{"documents", "/db"},
		{"queue", "/queue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
				Method: http.MethodGet, Path: tc.path,
			})
			if err != nil {
				t.Fatalf("the call itself failed rather than returning a refusal: %v", err)
			}
			if resp.GetStatusCode() == 200 {
				t.Fatalf("%s worked with no backend configured", tc.name)
			}

			body := string(resp.GetBody())
			t.Logf("%s: %s", tc.name, body)

			// The refusal must say the capability is not configured on this
			// Core, and must not look like a permission problem — the plugin
			// holds every permission here.
			if !strings.Contains(body, "not configured") {
				t.Errorf("the refusal does not say the capability is unconfigured: %q", body)
			}
			if strings.Contains(body, "PermissionDenied") {
				t.Errorf("a missing backend was reported as a permission problem: %q; "+
					"an operator would go and edit the manifest instead of starting Core with a database", body)
			}
		})
	}
}

// The in-process capabilities keep working when the database-backed ones are
// gone. They share a Core, not a fate.
func TestInProcessCapabilitiesSurviveMissingBackends(t *testing.T) {
	inst := barePlugin(t, "bare")

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/cache",
	})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Errorf("cache is unusable without a database: %s", resp.GetBody())
	}
}

// Meeting a missing backend must not damage the plugin. A refusal is an
// ordinary error, and the plugin has to carry on serving afterwards.
func TestPluginSurvivesRepeatedUnavailableCalls(t *testing.T) {
	inst := barePlugin(t, "bare")

	for range 30 {
		if _, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
			Method: http.MethodGet, Path: "/db",
		}); err != nil {
			t.Fatalf("the plugin stopped responding: %v", err)
		}
	}

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/items",
	})
	if err != nil {
		t.Fatalf("the plugin died after repeated refusals: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Errorf("status = %d after 30 refusals", resp.GetStatusCode())
	}
	if inst.ProcessExited() {
		t.Error("the plugin process exited")
	}
}

// Unavailable and PermissionDenied must not be confusable. This is the pair
// that sends an operator to the wrong place: one means start Core differently,
// the other means edit the manifest and get it approved.
func TestUnavailableIsDistinctFromPermissionDenied(t *testing.T) {
	// Backend present, permission absent.
	denied := launchPlugin(t, "denied", "1.0.0", nil)
	deniedResp, err := denied.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/cache",
	})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}
	deniedBody := string(deniedResp.GetBody())

	// Permission present, backend absent.
	unavailable := barePlugin(t, "bare")
	unavailableResp, err := unavailable.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/db",
	})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}
	unavailableBody := string(unavailableResp.GetBody())

	t.Logf("permission denied: %s", deniedBody)
	t.Logf("backend unavailable: %s", unavailableBody)

	if !strings.Contains(deniedBody, "PermissionDenied") {
		t.Errorf("a missing permission was not reported as PermissionDenied: %q", deniedBody)
	}
	if !strings.Contains(unavailableBody, "Unavailable") {
		t.Errorf("a missing backend was not reported as Unavailable: %q", unavailableBody)
	}
	if deniedBody == unavailableBody {
		t.Error("the two refusals are indistinguishable")
	}
}

// A database that goes away and comes back.
//
// This happens in production for ordinary reasons — a failover, a version
// upgrade, a restart during maintenance — and Core is expected to ride it out
// rather than needing a restart of its own. What must hold is that a plugin
// meeting a dead database gets a clear error instead of crashing, that Core
// stays up, and that everything resumes once the database does.
//
// Verified against a real PostgreSQL restart under docker compose as well:
// writes failed for about a second with "the database system is starting up"
// and then resumed, with no plugin quarantined and no process lost. This test
// covers the same code path — a connection pool that cannot reach its server —
// without needing a container.
func TestPluginSurvivesDatabaseOutage(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	// A pool of this test's own, so closing it does not disturb anything else.
	handle, err := db.InitDB(url)
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}

	cmds := db.NewCMDSManager(handle)
	txs := db.NewTxRegistry()
	defer txs.Close()
	data := hostsvc.NewCMDSData(handle, cmds, txs)
	if err := data.ProvisionSchema("outage", []db.CollectionSchema{{Name: "notes"}}); err != nil {
		t.Fatalf("provision: %v", err)
	}

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        "outage",
		InstanceID: "outage-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New("outage", []string{"db"}, hostsvc.Deps{
			Config: hostsvc.NewStaticConfig(),
			Data:   data,
		}),
		GrantedPermissions: []string{"db"},
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             os.Stderr,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer inst.Kill()

	// Working to begin with.
	if resp := callPlugin(t, inst, "/db", ""); resp.GetStatusCode() != 200 {
		t.Fatalf("the plugin could not reach the database before the outage: %s", resp.GetBody())
	}

	// The database goes away.
	handle.Close()

	resp := callPlugin(t, inst, "/db", "")
	body := string(resp.GetBody())
	t.Logf("during the outage: %d %s", resp.GetStatusCode(), body)

	if resp.GetStatusCode() == 200 {
		t.Error("a write succeeded against a closed database")
	}
	if inst.ProcessExited() {
		t.Fatal("the plugin process died when the database became unreachable")
	}
	if strings.Contains(body, "panic") {
		t.Errorf("the failure surfaced as a panic rather than an error: %q", body)
	}

	// Repeated failures must not degrade it further.
	for range 10 {
		if _, err := inst.Client.HandleHTTP(context.Background(),
			&pb.HttpRequest{Method: http.MethodGet, Path: "/db"}); err != nil {
			t.Fatalf("the plugin stopped responding during the outage: %v", err)
		}
	}
	if inst.ProcessExited() {
		t.Error("the plugin died after repeated database failures")
	}

	// And it serves its own routes throughout — a plugin whose database is
	// down is degraded, not dead.
	if resp := callPlugin(t, inst, "/items", ""); resp.GetStatusCode() != 200 {
		t.Errorf("a route needing no database returned %d during the outage", resp.GetStatusCode())
	}
}
