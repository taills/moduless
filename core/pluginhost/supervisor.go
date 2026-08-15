package pluginhost

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// ErrPluginDisabled means a restart was abandoned because an admin turned the
// plugin off, not because it failed.
//
// The distinction has to be a sentinel rather than a log line: a failed
// restart is retried with a longer backoff and each attempt counts toward the
// crash threshold, so treating "an admin disabled this" as a failure would
// let a plugin somebody deliberately switched off end up quarantined — a state
// the console presents as "Core gave up after repeated crashes" and which
// needs an explicit re-enable to leave.
var ErrPluginDisabled = errors.New("plugin is no longer enabled")

// RelaunchFunc starts a fresh instance of a plugin. The supervisor injects it
// rather than calling Launch directly, so restart policy can be tested without
// forking processes, and so Core can rebuild the LaunchSpec (paths, config,
// permissions) from the catalog at restart time rather than caching a stale one.
type RelaunchFunc func(ctx context.Context, key string) (*Instance, error)

// SupervisorConfig tunes crash recovery.
type SupervisorConfig struct {
	// PollInterval is how often a watched process is checked for exit.
	// go-plugin exposes only a boolean poll, not a notification.
	PollInterval time.Duration

	// InitialBackoff is the delay before the first restart attempt; it doubles
	// after each consecutive failure up to MaxBackoff.
	InitialBackoff time.Duration
	MaxBackoff     time.Duration

	// CrashThreshold is how many crashes within CrashWindow put a plugin into
	// quarantine instead of restarting it again. A plugin that crashes on
	// startup would otherwise restart forever, burning CPU and filling logs.
	CrashThreshold int
	CrashWindow    time.Duration
}

func DefaultSupervisorConfig() SupervisorConfig {
	return SupervisorConfig{
		PollInterval:   time.Second,
		InitialBackoff: time.Second,
		MaxBackoff:     time.Minute,
		CrashThreshold: 5,
		CrashWindow:    5 * time.Minute,
	}
}

type crashRecord struct {
	count   int
	firstAt time.Time
}

// Supervisor watches running plugins and restarts the ones that die
// unexpectedly.
type Supervisor struct {
	cfg      SupervisorConfig
	registry *Registry
	relaunch RelaunchFunc
	now      func() time.Time

	mu sync.Mutex
	// crashes counts recent deaths per plugin; quarantined records the ones
	// Core has given up on. Quarantine outlives the instances themselves —
	// they are removed from the registry — so without this the reason a plugin
	// has no replicas would be lost the moment it was isolated.
	crashes     map[string]*crashRecord
	quarantined map[string]time.Time

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

func NewSupervisor(reg *Registry, relaunch RelaunchFunc, cfg SupervisorConfig) *Supervisor {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.InitialBackoff <= 0 {
		cfg.InitialBackoff = time.Second
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = time.Minute
	}
	if cfg.CrashThreshold <= 0 {
		cfg.CrashThreshold = 5
	}
	if cfg.CrashWindow <= 0 {
		cfg.CrashWindow = 5 * time.Minute
	}
	return &Supervisor{
		cfg:         cfg,
		registry:    reg,
		relaunch:    relaunch,
		now:         time.Now,
		crashes:     map[string]*crashRecord{},
		quarantined: map[string]time.Time{},
		stop:        make(chan struct{}),
	}
}

// SetClock overrides the time source. Test-only.
func (s *Supervisor) SetClock(now func() time.Time) { s.now = now }

// Watch begins supervising an instance. It returns immediately.
func (s *Supervisor) Watch(ctx context.Context, inst *Instance) {
	s.wg.Go(func() { s.watchLoop(ctx, inst) })
}

// Stop ends all supervision and waits for the watchers to finish. In-flight
// restarts are abandoned.
func (s *Supervisor) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

func (s *Supervisor) watchLoop(ctx context.Context, inst *Instance) {
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !inst.ProcessExited() {
				continue
			}
			if !s.isUnexpected(inst) {
				return // deliberately stopped, or already replaced
			}
			inst.MarkFailed()
			logf("plugin %s: process exited unexpectedly", inst.Key)
			s.recover(ctx, inst.Key)
			return
		}
	}
}

// isUnexpected distinguishes a crash from a deliberate stop.
//
// Both checks matter. The state check catches a drain in progress. The
// identity check catches an instance that a blue-green swap already replaced:
// restarting it would resurrect an old version alongside the new one.
func (s *Supervisor) isUnexpected(inst *Instance) bool {
	switch inst.State() {
	case StateDraining, StateStopped, StateQuarantined:
		return false
	}
	return slices.Contains(s.registry.Current().Replicas(inst.Key), inst)
}

// recover applies the restart policy for a crashed plugin.
func (s *Supervisor) recover(ctx context.Context, key string) {
	if s.tooManyCrashes(key) {
		logf("plugin %s: crash threshold exceeded after %d crashes in %s, quarantining",
			key, s.cfg.CrashThreshold, s.cfg.CrashWindow)

		s.mu.Lock()
		s.quarantined[key] = s.now()
		s.mu.Unlock()

		for _, inst := range s.registry.Remove(key) {
			inst.MarkQuarantined()
			inst.Kill()
		}
		return
	}

	backoff := s.backoffFor(key)
	logf("plugin %s: restarting in %s", key, backoff)

	select {
	case <-s.stop:
		return
	case <-ctx.Done():
		return
	case <-time.After(backoff):
	}

	// An admin may have disabled the plugin while we were backing off, in
	// which case bringing it back would silently override their action.
	//
	// This is a cheap early-out, not the decision. Disable removes the plugin
	// from `enabled` before it removes it from the registry, so between those
	// two there is a moment when this check still says yes. relaunch is the
	// authority, and the sentinel below is what makes the two agree.
	if !s.registry.Current().Has(key) {
		logf("plugin %s: no longer registered, abandoning restart", key)
		return
	}

	inst, err := s.relaunch(ctx, key)
	if errors.Is(err, ErrPluginDisabled) {
		logf("plugin %s: disabled while restarting, abandoning", key)
		return
	}
	if err != nil {
		logf("plugin %s: restart failed: %v", key, err)
		s.recover(ctx, key)
		return
	}

	s.registry.Install(key, inst)
	s.Watch(ctx, inst)
	logf("plugin %s: restarted", key)
}

// tooManyCrashes records this crash and reports whether the plugin has now
// exceeded the threshold within the window.
func (s *Supervisor) tooManyCrashes(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	rec, ok := s.crashes[key]
	if !ok || now.Sub(rec.firstAt) > s.cfg.CrashWindow {
		s.crashes[key] = &crashRecord{count: 1, firstAt: now}
		return false
	}
	rec.count++
	return rec.count >= s.cfg.CrashThreshold
}

// backoffFor returns an exponential delay based on how many times this plugin
// has crashed in the current window.
func (s *Supervisor) backoffFor(key string) time.Duration {
	s.mu.Lock()
	rec, ok := s.crashes[key]
	n := 1
	if ok {
		n = rec.count
	}
	s.mu.Unlock()

	backoff := s.cfg.InitialBackoff
	for range n - 1 {
		backoff *= 2
		if backoff >= s.cfg.MaxBackoff {
			return s.cfg.MaxBackoff
		}
	}
	return backoff
}

// ClearCrashes resets a plugin's crash history, so an admin re-enabling a
// quarantined plugin gets a clean slate rather than being quarantined again on
// the first hiccup.
func (s *Supervisor) ClearCrashes(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.crashes, key)
	// Enabling, disabling or upgrading a plugin is an admin saying "try this
	// again", which is exactly what clears a quarantine.
	delete(s.quarantined, key)
}

// QuarantinedSince reports when Core gave up restarting a plugin, if it has.
//
// The isolated instances are gone from the registry by then, so this is the
// only thing that can tell an operator why a plugin they enabled is running no
// replicas — and distinguish "Core stopped trying" from "it is restarting
// right now".
func (s *Supervisor) QuarantinedSince(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.quarantined[key]
	return at, ok
}
