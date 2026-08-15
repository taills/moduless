package pluginhost

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// HostServicesFor builds the Core-side capability server for one plugin
// instance. Core supplies this; the manager never constructs it, so the
// permission set and the backends stay Core's concern.
type HostServicesFor func(pkg *Package) pb.HostServicesServer

// ManagerConfig configures plugin management.
type ManagerConfig struct {
	// Dir is the root under which plugin packages live.
	Dir string

	// DataDirRoot is where each plugin gets a private writable directory.
	DataDirRoot string

	// BaseEnv is the environment handed to every plugin process. It is an
	// allow-list, not an addition to Core's own environment: SkipHostEnv is
	// set precisely so a third-party plugin never inherits DATABASE_URL,
	// ADMIN_PASSWORD or the object-store credentials.
	BaseEnv []string

	DrainTimeout    time.Duration
	MaxMessageBytes int
	LogLevel        string

	// ConfigSource supplies a plugin's admin-managed settings at launch. Nil
	// means every plugin starts with an empty config.
	//
	// The manager reads settings but does not own them: whoever persists a
	// change calls SetConfig afterwards to push it out. Keeping storage on the
	// caller's side is what lets the same manager run against the database in
	// production and against a map in tests.
	ConfigSource func(pluginKey string) map[string]string

	// Supervisor tunes crash recovery: restart backoff, and how many crashes
	// within a window put a plugin into quarantine. The zero value uses
	// DefaultSupervisorConfig.
	Supervisor SupervisorConfig

	// DevMode relaxes process isolation for local development. Never in
	// production: see the sandbox notes.
	DevMode bool
}

// Status is one plugin's state, for the admin console.
type Status struct {
	Key         string   `json:"key"`
	DisplayName string   `json:"display_name"`
	Version     string   `json:"version"`
	Enabled     bool     `json:"enabled"`
	Replicas    int      `json:"replicas"`
	Ready       int      `json:"ready"`
	InFlight    int64    `json:"in_flight"`
	Permissions []string `json:"permissions"`
	Filters     int      `json:"filters"`
	Jobs        int      `json:"jobs"`
	HasFrontend bool     `json:"has_frontend"`
	LoadError   string   `json:"load_error,omitempty"`

	// Quarantined means Core stopped restarting this plugin after it crashed
	// repeatedly. Without it, a quarantined plugin looks identical in the
	// console to one that is merely between restarts: enabled, zero replicas.
	// One of those resolves itself and the other never will.
	Quarantined   bool      `json:"quarantined,omitempty"`
	QuarantinedAt time.Time `json:"quarantined_at,omitempty"`

	// Config is what the plugin declares it can be configured with, so the
	// console can render a form instead of a free-text key/value editor where
	// a typo goes unnoticed by both sides.
	Config []manifest.ConfigDecl `json:"config,omitempty"`
}

// Manager owns installed packages and drives their lifecycle: enable, disable
// and upgrade. It is the layer the admin API calls into.
type Manager struct {
	cfg      ManagerConfig
	registry *Registry
	sup      *Supervisor
	hostFor  HostServicesFor

	mu       sync.Mutex
	packages map[string]*Package
	enabled  map[string]struct{}
	errors   map[string]string
}

// NewManager wires a manager over a registry. The supervisor is created here
// so a crashed plugin is relaunched from its package rather than from a cached
// launch spec that may have gone stale.
func NewManager(cfg ManagerConfig, reg *Registry, hostFor HostServicesFor) *Manager {
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = DefaultDrainTimeout
	}
	m := &Manager{
		cfg:      cfg,
		registry: reg,
		hostFor:  hostFor,
		packages: map[string]*Package{},
		enabled:  map[string]struct{}{},
		errors:   map[string]string{},
	}
	supCfg := cfg.Supervisor
	if supCfg == (SupervisorConfig{}) {
		supCfg = DefaultSupervisorConfig()
	}
	m.sup = NewSupervisor(reg, m.relaunch, supCfg)
	return m
}

// Close stops supervision. Draining running plugins is the caller's job, via
// the registry, so shutdown ordering stays explicit.
func (m *Manager) Close() { m.sup.Stop() }

// Scan reloads the package directory. Packages that fail to load are recorded
// and reported through List rather than aborting the scan: one broken plugin
// must not stop Core from serving the others.
func (m *Manager) Scan() {
	packages, failures := ScanPackages(m.cfg.Dir)

	m.mu.Lock()
	defer m.mu.Unlock()

	m.packages = make(map[string]*Package, len(packages))
	for _, p := range packages {
		m.packages[p.Key()] = p
	}
	m.errors = make(map[string]string, len(failures))
	for name, err := range failures {
		m.errors[name] = err.Error()
		logf("plugin package %s failed to load: %v", name, err)
	}
}

// Enable launches a plugin's processes and routes to it.
//
// Nothing is published until every replica has started and completed its
// Configure handshake, so a plugin that fails to start leaves the live routing
// table untouched.
func (m *Manager) Enable(ctx context.Context, key string) error {
	m.mu.Lock()
	pkg, ok := m.packages[key]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("plugin %s is not installed", key)
	}

	instances, err := m.launchAll(ctx, pkg)
	if err != nil {
		return err
	}

	m.registry.InstallPlugin(Registration{
		Key:                   key,
		Instances:             instances,
		Filters:               pkg.Filters,
		AllowIdentityMutation: pkg.AllowsIdentityMutation(),
	})
	for _, inst := range instances {
		m.sup.Watch(context.WithoutCancel(ctx), inst)
	}

	m.mu.Lock()
	m.enabled[key] = struct{}{}
	m.mu.Unlock()

	// Enabling is an admin deciding to try this plugin again, which clears any
	// quarantine and the crash history behind it. Leaving them would report a
	// running plugin as isolated, and would re-quarantine it on its first
	// hiccup rather than after the usual threshold.
	m.sup.ClearCrashes(key)

	logf("plugin %s v%s enabled (%d replica(s))", key, pkg.Version(), len(instances))
	return nil
}

// Disable stops routing to a plugin and drains its processes.
func (m *Manager) Disable(ctx context.Context, key string) error {
	m.mu.Lock()
	delete(m.enabled, key)
	m.mu.Unlock()

	displaced := m.registry.Remove(key)
	if len(displaced) == 0 {
		return nil
	}

	// Draining happens after the plugin has already left the snapshot, so no
	// new request can reach it while we wait for the in-flight ones.
	var firstErr error
	for _, inst := range displaced {
		if err := inst.Drain(ctx, m.cfg.DrainTimeout); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	m.sup.ClearCrashes(key)
	logf("plugin %s disabled", key)
	return firstErr
}

// Upgrade re-reads the package from disk and swaps the running processes for
// new ones, without dropping a request.
//
// The new version is launched and must complete its handshake before anything
// is published. If it fails, the new processes are killed and the old ones
// keep serving — the rollback needs no undo step because nothing was committed.
//
// Whatever delivers the new package must REPLACE the binary rather than
// overwrite it: rename(2) or an unlink followed by a write, never a copy over
// the existing path. Writing into a file that a process is currently executing
// corrupts that process's image, and the old version is still serving traffic
// right up to the swap. Deploy with `mv`, not `cp`.
func (m *Manager) Upgrade(ctx context.Context, key string) error {
	pkg, err := LoadPackage(filepath.Join(m.cfg.Dir, key))
	if err != nil {
		return fmt.Errorf("reload plugin %s: %w", key, err)
	}

	instances, err := m.launchAll(ctx, pkg)
	if err != nil {
		return fmt.Errorf("upgrade %s: %w", key, err)
	}

	m.mu.Lock()
	m.packages[key] = pkg
	m.enabled[key] = struct{}{}
	m.mu.Unlock()

	m.registry.Swap(context.WithoutCancel(ctx), Registration{
		Key:                   key,
		Instances:             instances,
		Filters:               pkg.Filters,
		AllowIdentityMutation: pkg.AllowsIdentityMutation(),
	}, m.cfg.DrainTimeout)

	for _, inst := range instances {
		m.sup.Watch(context.WithoutCancel(ctx), inst)
	}
	m.sup.ClearCrashes(key)

	logf("plugin %s upgraded to v%s", key, pkg.Version())
	return nil
}

// EnableAll starts every installed plugin, returning the first failure while
// still attempting the rest.
func (m *Manager) EnableAll(ctx context.Context) error {
	m.mu.Lock()
	keys := make([]string, 0, len(m.packages))
	for k := range m.packages {
		keys = append(keys, k)
	}
	m.mu.Unlock()
	sort.Strings(keys)

	var firstErr error
	for _, key := range keys {
		if err := m.Enable(ctx, key); err != nil {
			logf("plugin %s failed to enable: %v", key, err)
			m.mu.Lock()
			m.errors[key] = err.Error()
			m.mu.Unlock()
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// List reports every installed plugin and its current state.
func (m *Manager) List() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()

	snap := m.registry.Current()
	out := make([]Status, 0, len(m.packages)+len(m.errors))

	for key, pkg := range m.packages {
		_, on := m.enabled[key]
		st := Status{
			Key:         key,
			DisplayName: pkg.Manifest.DisplayName,
			Version:     pkg.Version(),
			Enabled:     on,
			Permissions: pkg.Manifest.Permissions,
			Filters:     len(pkg.Filters),
			Jobs:        len(pkg.Manifest.Jobs),
			HasFrontend: pkg.FrontendDir != "",
			LoadError:   m.errors[key],
			Config:      pkg.Manifest.Config,
		}
		for _, inst := range snap.Replicas(key) {
			st.Replicas++
			st.InFlight += inst.InFlight()
			if inst.State() == StateReady {
				st.Ready++
			}
		}
		if at, isolated := m.sup.QuarantinedSince(key); isolated {
			st.Quarantined, st.QuarantinedAt = true, at
		}
		out = append(out, st)
	}

	// Packages that failed to load still appear, so a broken plugin is visible
	// in the console instead of silently missing.
	for name, msg := range m.errors {
		if _, ok := m.packages[name]; ok {
			continue
		}
		out = append(out, Status{Key: name, LoadError: msg})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Package returns an installed package by key.
func (m *Manager) Package(key string) (*Package, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pkg, ok := m.packages[key]
	return pkg, ok
}

// SetConfig delivers settings to every running replica of a plugin.
//
// The caller persists the change first; this only pushes it. Delivery is
// best-effort per replica and the first failure is returned after all of them
// have been tried, because a replica that missed an update is a stale replica,
// not a reason to leave the others stale too. Every replica picks the new
// settings up at its next launch regardless, since ConfigSource is read there.
//
// A plugin that is not running is not an error: there is nobody to tell.
func (m *Manager) SetConfig(ctx context.Context, key string, cfg map[string]string) error {
	// Merged the same way a launch merges, so a plugin cannot see one shape of
	// config at start-up and another after an edit.
	m.mu.Lock()
	pkg, ok := m.packages[key]
	m.mu.Unlock()
	if ok && pkg.Manifest != nil {
		cfg = pkg.Manifest.MergeConfig(cfg)
	}

	replicas := m.registry.Current().Replicas(key)

	var firstErr error
	for _, inst := range replicas {
		if err := inst.Client.OnConfigChanged(ctx, &pb.ConfigChangeEvent{Config: cfg}); err != nil {
			logf("plugin %s: pushing config to %s: %v", key, inst.InstanceID, err)
			if firstErr == nil {
				firstErr = fmt.Errorf("push config to %s: %w", inst.InstanceID, err)
			}
		}
	}
	if len(replicas) > 0 && firstErr == nil {
		logf("plugin %s: config pushed to %d replica(s)", key, len(replicas))
	}
	return firstErr
}

// configFor is what a plugin receives: whatever an operator set, with the
// manifest's declared defaults filled in for anything they did not.
//
// Supplying defaults here rather than leaving it to each plugin means a
// setting has one stated default — the one a reviewer reads in the manifest
// and an operator sees in the console — instead of that value and a second
// copy buried in the plugin's own fallback logic, free to disagree with it.
func (m *Manager) configFor(key string) map[string]string {
	var set map[string]string
	if m.cfg.ConfigSource != nil {
		set = m.cfg.ConfigSource(key)
	}

	m.mu.Lock()
	pkg, ok := m.packages[key]
	m.mu.Unlock()
	if !ok || pkg.Manifest == nil {
		return set
	}
	return pkg.Manifest.MergeConfig(set)
}

// launchAll starts every replica a package asks for, killing any that already
// started if a later one fails. A half-started plugin must never be published.
func (m *Manager) launchAll(ctx context.Context, pkg *Package) ([]*Instance, error) {
	count := pkg.Manifest.ReplicaCount()
	instances := make([]*Instance, 0, count)

	for i := range count {
		inst, err := m.launchOne(ctx, pkg, i)
		if err != nil {
			for _, started := range instances {
				started.Kill()
			}
			return nil, err
		}
		instances = append(instances, inst)
	}
	return instances, nil
}

func (m *Manager) launchOne(ctx context.Context, pkg *Package, index int) (*Instance, error) {
	dataDir, err := m.dataDirFor(pkg.Key())
	if err != nil {
		return nil, err
	}

	return Launch(ctx, LaunchSpec{
		Key:        pkg.Key(),
		InstanceID: fmt.Sprintf("%s-%d", pkg.Key(), index),
		Version:    pkg.Version(),
		Weight:     pkg.Manifest.Weight,
		BinaryPath: pkg.BinaryPath,
		// Without this go-plugin runs whatever is at BinaryPath, unchecked.
		// The verification exists precisely for the gap between validating a
		// package and executing it, and leaving it unset made "SHA-256
		// verified" true only of the launch path tests used directly.
		Checksum:           pkg.Checksum,
		HostImpl:           m.hostFor(pkg),
		GrantedPermissions: pkg.Manifest.Permissions,
		Config:             m.configFor(pkg.Key()),
		DataDir:            dataDir,
		LogLevel:           m.cfg.LogLevel,
		MaxMessageBytes:    m.cfg.MaxMessageBytes,
		Env:                m.envFor(pkg, dataDir),
		Stdout:             os.Stderr,
		Stderr:             os.Stderr,
		DevMode:            m.cfg.DevMode,
	})
}

// relaunch is what the supervisor calls after a crash. It reads the package
// afresh so a plugin that was upgraded or removed while crashing is handled
// correctly.
func (m *Manager) relaunch(ctx context.Context, key string) (*Instance, error) {
	m.mu.Lock()
	pkg, ok := m.packages[key]
	_, on := m.enabled[key]
	m.mu.Unlock()

	if !ok || !on {
		return nil, fmt.Errorf("plugin %s is no longer enabled", key)
	}
	return m.launchOne(ctx, pkg, 0)
}

func (m *Manager) dataDirFor(key string) (string, error) {
	if m.cfg.DataDirRoot == "" {
		return "", nil
	}
	dir := filepath.Join(m.cfg.DataDirRoot, key)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create data dir for %s: %w", key, err)
	}
	return dir, nil
}

// envFor builds the plugin's entire environment. Note this replaces rather
// than extends Core's own: the plugin sees only what is listed here.
func (m *Manager) envFor(pkg *Package, dataDir string) []string {
	env := make([]string, 0, len(m.cfg.BaseEnv)+3)
	if len(m.cfg.BaseEnv) == 0 {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	} else {
		env = append(env, m.cfg.BaseEnv...)
	}
	env = append(env,
		"MODULESS_PLUGIN_KEY="+pkg.Key(),
		"MODULESS_PLUGIN_VERSION="+pkg.Version(),
	)
	if dataDir != "" {
		env = append(env, "MODULESS_DATA_DIR="+dataDir)
	}
	return env
}
