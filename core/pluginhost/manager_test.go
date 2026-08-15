package pluginhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/taills/moduless/proto/plugin"
)

// writePackage lays out a plugin package on disk around the echo fixture.
//
// The binary is unlinked before being rewritten rather than overwritten in
// place. Overwriting a file that a process is currently executing corrupts
// that process's image; replacing it creates a new inode and leaves the
// running process on the old one. This is the same reason a deployment should
// move a new binary into place rather than copying over the old one, and it is
// what makes an in-place upgrade survivable.
func writePackage(t *testing.T, root, key, version string, replicas int, extraYAML string) string {
	t.Helper()

	dir := filepath.Join(root, key)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	binary, err := os.ReadFile(echoBinary)
	if err != nil {
		t.Fatalf("read fixture binary: %v", err)
	}
	binPath := filepath.Join(dir, "bin", "plugin")
	_ = os.Remove(binPath)
	if err := os.WriteFile(binPath, binary, 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	runtime := "runtime:\n  entrypoint: bin/plugin\n"
	if replicas > 0 {
		runtime += fmt.Sprintf("  replicas: %d\n", replicas)
	}
	yaml := "key: " + key + "\n" +
		"display_name: " + key + "\n" +
		"version: " + version + "\n" +
		runtime + extraYAML
	if err := os.WriteFile(filepath.Join(dir, ManifestFilename), []byte(yaml), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return dir
}

func newTestManager(t *testing.T, root string) (*Manager, *Registry) {
	t.Helper()

	reg := NewRegistry()
	mgr := NewManager(ManagerConfig{
		Dir:          root,
		DataDirRoot:  filepath.Join(root, ".data"),
		DrainTimeout: 2 * time.Second,
		DevMode:      true,
	}, reg, func(*Package) pb.HostServicesServer {
		return &stubHost{config: map[string]string{"greeting": "hi"}}
	})
	t.Cleanup(mgr.Close)
	return mgr, reg
}

func TestManagerScanLoadsPackages(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0, "")
	writePackage(t, root, "beta", "2.0.0", 0, "")

	mgr, _ := newTestManager(t, root)
	mgr.Scan()

	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("scanned %d packages, want 2: %+v", len(list), list)
	}
	if list[0].Key != "alpha" || list[1].Key != "beta" {
		t.Errorf("packages = %v, want them sorted by key", []string{list[0].Key, list[1].Key})
	}
	for _, st := range list {
		if st.Enabled {
			t.Errorf("%s is enabled straight after a scan; enabling must be explicit", st.Key)
		}
	}
}

// One broken package must not stop the others from loading, and it must stay
// visible in the console rather than silently disappearing.
func TestManagerScanSurvivesABadPackage(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "good", "1.0.0", 0, "")

	bad := filepath.Join(root, "broken")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bad, ManifestFilename), []byte("key: broken\n"), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr, _ := newTestManager(t, root)
	mgr.Scan()

	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("List returned %d entries, want the good and the broken one: %+v", len(list), list)
	}

	byKey := map[string]Status{}
	for _, st := range list {
		byKey[st.Key] = st
	}
	if byKey["good"].LoadError != "" {
		t.Errorf("the good package reported an error: %s", byKey["good"].LoadError)
	}
	if byKey["broken"].LoadError == "" {
		t.Error("the broken package reported no error")
	}
}

func TestLoadPackageRejectsBadLayouts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, root string) string
	}{
		{
			name: "missing manifest",
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "nomanifest")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			name: "missing entrypoint",
			setup: func(t *testing.T, root string) string {
				dir := filepath.Join(root, "noentry")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				yaml := "key: noentry\nversion: 1.0.0\nruntime:\n  entrypoint: bin/missing\n"
				if err := os.WriteFile(filepath.Join(dir, ManifestFilename), []byte(yaml), 0o600); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			name: "entrypoint not executable",
			setup: func(t *testing.T, root string) string {
				dir := writePackage(t, root, "notexec", "1.0.0", 0, "")
				if err := os.Chmod(filepath.Join(dir, "bin", "plugin"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
		},
		{
			name: "unknown permission",
			setup: func(t *testing.T, root string) string {
				return writePackage(t, root, "badperm", "1.0.0", 0, "permissions:\n  - root:everything\n")
			},
		},
		{
			name: "filter without paths",
			setup: func(t *testing.T, root string) string {
				return writePackage(t, root, "badfilter", "1.0.0", 0,
					"filters:\n  - phase: pre_route\n")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.setup(t, t.TempDir())
			if _, err := LoadPackage(dir); err == nil {
				t.Error("LoadPackage accepted an invalid package")
			}
		})
	}
}

// The directory name is what an admin sees and what the on-disk layout keys
// on, so a manifest claiming a different key is a packaging error.
func TestScanRejectsKeyDirectoryMismatch(t *testing.T) {
	root := t.TempDir()
	dir := writePackage(t, root, "declared", "1.0.0", 0, "")
	renamed := filepath.Join(root, "actual")
	if err := os.Rename(dir, renamed); err != nil {
		t.Fatalf("rename: %v", err)
	}

	packages, failures := ScanPackages(root)
	if len(packages) != 0 {
		t.Errorf("loaded %d packages, want none", len(packages))
	}
	if failures["actual"] == nil {
		t.Error("the key/directory mismatch was not reported")
	}
}

func TestManagerEnableDisable(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0, "")

	mgr, reg := newTestManager(t, root)
	mgr.Scan()

	ctx := context.Background()
	if err := mgr.Enable(ctx, "alpha"); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	inst, ok := reg.Current().Pick("alpha")
	if !ok {
		t.Fatal("plugin is not routable after Enable")
	}
	if inst.State() != StateReady {
		t.Errorf("state = %v, want ready", inst.State())
	}

	st := mgr.List()[0]
	if !st.Enabled || st.Ready != 1 {
		t.Errorf("status = %+v, want enabled with one ready replica", st)
	}

	if err := mgr.Disable(ctx, "alpha"); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if _, ok := reg.Current().Pick("alpha"); ok {
		t.Error("plugin is still routable after Disable")
	}
	if !inst.ProcessExited() {
		t.Error("the plugin process survived Disable")
	}
	if mgr.List()[0].Enabled {
		t.Error("status still reports the plugin as enabled")
	}
}

func TestManagerEnableUnknownPlugin(t *testing.T) {
	mgr, _ := newTestManager(t, t.TempDir())
	mgr.Scan()

	if err := mgr.Enable(context.Background(), "ghost"); err == nil {
		t.Error("Enable accepted a plugin that is not installed")
	}
}

func TestManagerUpgradeSwapsProcesses(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0, "")

	mgr, reg := newTestManager(t, root)
	mgr.Scan()
	ctx := context.Background()

	if err := mgr.Enable(ctx, "alpha"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	before, _ := reg.Current().Pick("alpha")

	// Ship a new version in place, as an upload would.
	writePackage(t, root, "alpha", "2.0.0", 0, "")

	if err := mgr.Upgrade(ctx, "alpha"); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}

	after, ok := reg.Current().Pick("alpha")
	if !ok {
		t.Fatal("plugin is not routable after the upgrade")
	}
	if after == before {
		t.Error("the upgrade did not replace the instance")
	}
	if after.Version != "2.0.0" {
		t.Errorf("running version = %q, want 2.0.0", after.Version)
	}

	deadline := time.Now().Add(3 * time.Second)
	for !before.ProcessExited() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !before.ProcessExited() {
		t.Error("the previous version was not drained and stopped")
	}
}

// A failed upgrade must leave the running version untouched: nothing is
// published until the new processes have completed their handshake.
func TestManagerUpgradeRollsBackOnFailure(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 0, "")

	mgr, reg := newTestManager(t, root)
	mgr.Scan()
	ctx := context.Background()

	if err := mgr.Enable(ctx, "alpha"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	before, _ := reg.Current().Pick("alpha")

	// Corrupt the package the way a bad upload would.
	if err := os.WriteFile(filepath.Join(root, "alpha", "bin", "plugin"), []byte("not a binary"), 0o755); err != nil {
		t.Fatalf("corrupt binary: %v", err)
	}

	if err := mgr.Upgrade(ctx, "alpha"); err == nil {
		t.Fatal("Upgrade reported success with a broken package")
	}

	after, ok := reg.Current().Pick("alpha")
	if !ok {
		t.Fatal("the plugin stopped serving after a failed upgrade")
	}
	if after != before {
		t.Error("a failed upgrade replaced the running instance")
	}
	if before.ProcessExited() {
		t.Error("a failed upgrade killed the running process")
	}
}

func TestManagerEnableAllReportsFailuresButContinues(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "good", "1.0.0", 0, "")
	writePackage(t, root, "bad", "1.0.0", 0, "")
	if err := os.WriteFile(filepath.Join(root, "bad", "bin", "plugin"), []byte("nope"), 0o755); err != nil {
		t.Fatalf("corrupt binary: %v", err)
	}

	mgr, reg := newTestManager(t, root)
	mgr.Scan()

	if err := mgr.EnableAll(context.Background()); err == nil {
		t.Error("EnableAll reported success despite a broken plugin")
	}
	if _, ok := reg.Current().Pick("good"); !ok {
		t.Error("the healthy plugin was not enabled")
	}
}

// Replicas are how a plugin scales; the round-robin picker needs all of them
// registered.
func TestManagerLaunchesDeclaredReplicas(t *testing.T) {
	root := t.TempDir()
	writePackage(t, root, "alpha", "1.0.0", 3, "")

	mgr, reg := newTestManager(t, root)
	mgr.Scan()

	if err := mgr.Enable(context.Background(), "alpha"); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if got := len(reg.Current().Replicas("alpha")); got != 3 {
		t.Errorf("launched %d replicas, want 3", got)
	}
}
