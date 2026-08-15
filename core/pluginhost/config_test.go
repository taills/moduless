package pluginhost

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/taills/moduless/proto/plugin"
	"google.golang.org/protobuf/types/known/emptypb"
)

// What an operator sets, reaching the plugin.
//
// Every piece of this existed separately and was tested separately: manifests
// declare settings, MergeConfig fills in defaults, SetConfig pushes to running
// replicas. What had no test — and, it turned out, no implementation — was the
// join: nothing supplied ConfigSource, so a plugin only ever saw the defaults
// its own manifest declared, whatever an operator had configured.
//
// These tests run against the real echo fixture rather than a fake client,
// because the thing being checked is what arrives at the far end of the two
// separate paths a plugin reads its configuration through.

// configuredManager builds a manager whose stored settings are whatever the
// returned map holds, wired the way core/main.go wires it: the same answer for
// the values handed to Configure and for the reverse-channel GetConfig.
func configuredManager(t *testing.T, root string) (*Manager, map[string]map[string]string) {
	t.Helper()

	stored := map[string]map[string]string{}

	reg := NewRegistry()
	var mgr *Manager
	mgr = NewManager(ManagerConfig{
		Dir:          root,
		DataDirRoot:  filepath.Join(root, ".data"),
		DrainTimeout: 2 * time.Second,
		DevMode:      true,
		ConfigSource: func(key string) map[string]string { return stored[key] },
	}, reg, func(pkg *Package) pb.HostServicesServer {
		return &managerConfigHost{mgr: mgr, key: pkg.Key()}
	})
	t.Cleanup(mgr.Close)
	return mgr, stored
}

// managerConfigHost answers GetConfig from the manager, which is what Core
// does. Answering from anywhere else is the bug: a plugin re-reading its own
// settings would get something other than what it was started with.
type managerConfigHost struct {
	pb.UnimplementedHostServicesServer
	mgr *Manager
	key string
}

func (h *managerConfigHost) GetConfig(context.Context, *emptypb.Empty) (*pb.GetConfigResponse, error) {
	return &pb.GetConfigResponse{Config: h.mgr.ConfigFor(h.key)}, nil
}

// echoConfig returns the two values the fixture reports: what Configure handed
// it, and what asking Core returned.
func echoConfig(t *testing.T, inst *Instance) (launched, asked string) {
	t.Helper()

	resp, err := inst.Client.HandleHTTP(context.Background(),
		&pb.HttpRequest{Method: http.MethodGet, Path: "/items"})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}
	get := func(name string) string {
		if v := resp.GetHeaders()[name]; v != nil && len(v.GetValues()) > 0 {
			return v.GetValues()[0]
		}
		return ""
	}
	return get("X-Launch-Config"), get("X-Host-Config")
}

func firstReplica(t *testing.T, reg *Registry, key string) *Instance {
	t.Helper()

	replicas := reg.Current().Replicas(key)
	if len(replicas) == 0 {
		t.Fatalf("plugin %s has no running replica", key)
	}
	return replicas[0]
}

// A value an operator set is what the plugin starts with — not the manifest's
// default, which is what it got before any of this was connected.
func TestStoredConfigReachesAPluginAtLaunch(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0,
		"config:\n  - key: greeting\n    default: from-manifest\n")

	mgr, stored := configuredManager(t, root)
	stored["alpha"] = map[string]string{"greeting": "from-operator"}
	mgr.Scan()

	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	launched, asked := echoConfig(t, firstReplica(t, mgr.registry, "alpha"))
	if launched != "from-operator" {
		t.Errorf("Configure was handed %q; the stored value never reached the plugin", launched)
	}
	if asked != "from-operator" {
		t.Errorf("GetConfig returned %q; the plugin cannot read back what it was started with", asked)
	}
}

// The manifest's default applies where an operator set nothing — otherwise
// declaring a default would be pointless, and every plugin would carry a
// second copy of it in its own fallback logic.
func TestManifestDefaultAppliesWhenNothingIsSet(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0,
		"config:\n  - key: greeting\n    default: from-manifest\n")

	mgr, _ := configuredManager(t, root)
	mgr.Scan()

	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	launched, asked := echoConfig(t, firstReplica(t, mgr.registry, "alpha"))
	if launched != "from-manifest" || asked != "from-manifest" {
		t.Errorf("launched=%q asked=%q; want the declared default on both paths", launched, asked)
	}
}

// The two paths must agree. A plugin is handed its configuration at Configure
// and can ask for it again at any time; Core originally computed the first
// from the manager and served the second from a separate in-memory store, so
// the two answers were free to differ and did.
func TestBothConfigPathsAgree(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0,
		"config:\n  - key: greeting\n    default: from-manifest\n")

	mgr, stored := configuredManager(t, root)
	mgr.Scan()
	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// Before any change, and after one.
	for _, want := range []string{"from-manifest", "changed"} {
		if want == "changed" {
			stored["alpha"] = map[string]string{"greeting": "changed"}
			if err := mgr.SetConfig(context.Background(), "alpha", stored["alpha"]); err != nil {
				t.Fatalf("SetConfig: %v", err)
			}
		}
		launched, asked := echoConfig(t, firstReplica(t, mgr.registry, "alpha"))
		if launched != asked {
			t.Errorf("the two paths disagree: Configure gave %q, GetConfig gives %q", launched, asked)
		}
		if asked != want {
			t.Errorf("config = %q; want %q", asked, want)
		}
	}
}

// A change survives the plugin restarting, which is the whole point of storing
// it. Pushing to a running process and never writing it down works exactly
// once, which is long enough to be trusted.
func TestConfigSurvivesADisableEnableCycle(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0,
		"config:\n  - key: greeting\n    default: from-manifest\n")

	mgr, stored := configuredManager(t, root)
	mgr.Scan()
	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	stored["alpha"] = map[string]string{"greeting": "persisted"}
	if err := mgr.SetConfig(context.Background(), "alpha", stored["alpha"]); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	if err := mgr.Disable(context.Background(), "alpha"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	launched, asked := echoConfig(t, firstReplica(t, mgr.registry, "alpha"))
	if launched != "persisted" {
		t.Errorf("after a restart the plugin was launched with %q; the change did not survive", launched)
	}
	if asked != "persisted" {
		t.Errorf("after a restart GetConfig returns %q", asked)
	}
}

// Configuration is per plugin. Config keys are short words — every plugin will
// have a "timeout" — so one plugin reading another's settings would be both
// easy to cause and hard to notice.
func TestConfigDoesNotLeakBetweenPlugins(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0,
		"config:\n  - key: greeting\n    default: alpha-default\n")
	writePackage(t, root, "beta", "1.0.0", 0,
		"config:\n  - key: greeting\n    default: beta-default\n")

	mgr, stored := configuredManager(t, root)
	stored["alpha"] = map[string]string{"greeting": "alpha-set"}
	mgr.Scan()

	for _, key := range []string{"alpha", "beta"} {
		if err := mgr.Enable(context.Background(), key); err != nil {
			t.Fatalf("enable %s: %v", key, err)
		}
	}

	if _, asked := echoConfig(t, firstReplica(t, mgr.registry, "alpha")); asked != "alpha-set" {
		t.Errorf("alpha sees %q", asked)
	}
	if _, asked := echoConfig(t, firstReplica(t, mgr.registry, "beta")); asked != "beta-default" {
		t.Errorf("beta sees %q; alpha's setting reached it", asked)
	}
}

// A tripped circuit breaker is visible from outside.
//
// Breaker.Open() existed, said in its own comment that it was safe for
// diagnostics, and was called by nothing but tests: Status carried no breaker
// state, so the console could not show it. From there a plugin Core has
// stopped calling looks exactly like one that is merely slow — enabled,
// ready, failing — and the two need opposite responses. One is Core protecting
// itself from a plugin that has been erroring, and it clears on its own; the
// other does not.
func TestStatusReportsATrippedBreaker(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0, "")

	mgr, _ := configuredManager(t, root)
	mgr.Scan()
	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	inst := firstReplica(t, mgr.registry, "alpha")

	if got := statusFor(t, mgr, "alpha"); got.Tripped != 0 {
		t.Fatalf("a healthy plugin reports %d tripped replica(s)", got.Tripped)
	}

	// Fail it until the breaker opens.
	for range 20 {
		inst.Breaker.RecordFailure()
	}
	if !inst.Breaker.Open() {
		t.Fatal("the breaker did not open after 20 failures; this test is not testing anything")
	}

	if got := statusFor(t, mgr, "alpha"); got.Tripped != 1 {
		t.Errorf("tripped = %d, want 1; an operator cannot tell this plugin from a slow one",
			got.Tripped)
	}
}

// The start time reaches the console too, which is what says whether a plugin
// has been quietly restarting rather than running since Core came up.
func TestStatusReportsWhenAReplicaStarted(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0, "")

	mgr, _ := configuredManager(t, root)
	mgr.Scan()
	before := time.Now()
	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("enable: %v", err)
	}

	got := statusFor(t, mgr, "alpha")
	if got.OldestStartedAt.IsZero() {
		t.Fatal("no start time reported for a running plugin")
	}
	if got.OldestStartedAt.Before(before.Add(-time.Minute)) {
		t.Errorf("start time %s is not from this run", got.OldestStartedAt)
	}
}

func statusFor(t *testing.T, mgr *Manager, key string) Status {
	t.Helper()
	for _, s := range mgr.List() {
		if s.Key == key {
			return s
		}
	}
	t.Fatalf("no status for %s", key)
	return Status{}
}
