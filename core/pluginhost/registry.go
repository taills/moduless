package pluginhost

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/manifest"
)

// DefaultDrainTimeout bounds how long a replaced instance may keep running to
// finish in-flight requests before it is killed anyway.
const DefaultDrainTimeout = 30 * time.Second

// replicaSet holds every instance of one plugin plus the round-robin state
// used to choose between them.
//
// The instance slice is immutable once built, so readers need no
// synchronisation to iterate it. Only the weighted round-robin counters are
// mutable, and they are guarded by a mutex that is never held across a call
// into a plugin. Single-replica plugins — the overwhelmingly common case —
// bypass the mutex entirely.
type replicaSet struct {
	instances []*Instance

	mu      sync.Mutex
	current []int
}

func newReplicaSet(instances []*Instance) *replicaSet {
	return &replicaSet{
		instances: instances,
		current:   make([]int, len(instances)),
	}
}

// pick selects a replica using nginx's smooth weighted round-robin, so traffic
// is spread proportionally to weight while staying evenly interleaved rather
// than arriving in bursts per replica.
//
// This is the algorithm the reverse tunnel used, carried over unchanged.
func (rs *replicaSet) pick() (*Instance, bool) {
	switch len(rs.instances) {
	case 0:
		return nil, false
	case 1:
		inst := rs.instances[0]
		if inst.State() != StateReady {
			return nil, false
		}
		return inst, true
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	total := 0
	best := -1
	for idx, inst := range rs.instances {
		if inst.State() != StateReady {
			continue
		}
		rs.current[idx] += inst.Weight
		total += inst.Weight
		if best < 0 || rs.current[idx] > rs.current[best] {
			best = idx
		}
	}
	if best < 0 {
		return nil, false
	}
	rs.current[best] -= total
	return rs.instances[best], true
}

// Snapshot is an immutable view of every routable plugin at one instant.
//
// The gateway loads it once per request and passes it down, so a request sees
// one consistent view even if an admin enables or disables a plugin midway
// through: it will not route to the new plugin's backend while still applying
// the old plugin's filters.
type Snapshot struct {
	plugins map[string]*replicaSet
	chain   *pipeline.Chain
	version uint64
}

func emptySnapshot() *Snapshot {
	return &Snapshot{plugins: map[string]*replicaSet{}, chain: pipeline.EmptyChain()}
}

// Chain is the filter table for this snapshot. It is never nil.
func (s *Snapshot) Chain() *pipeline.Chain {
	if s.chain == nil {
		return pipeline.EmptyChain()
	}
	return s.chain
}

// Target implements pipeline.Resolver, so the filter pipeline can reach
// plugins without importing this package.
func (s *Snapshot) Target(pluginKey string) (pipeline.Target, bool) {
	inst, ok := s.Pick(pluginKey)
	if !ok {
		return nil, false
	}
	return inst, true
}

// Version increments on every change and is what the console's SSE stream
// reports so the browser knows to refetch.
func (s *Snapshot) Version() uint64 { return s.version }

// Pick returns a ready instance of key, or false when the plugin is absent,
// disabled or has no healthy replica.
func (s *Snapshot) Pick(key string) (*Instance, bool) {
	rs, ok := s.plugins[key]
	if !ok {
		return nil, false
	}
	return rs.pick()
}

// Has reports whether key is currently routable.
func (s *Snapshot) Has(key string) bool {
	_, ok := s.plugins[key]
	return ok
}

// Keys lists the routable plugin keys in deterministic order.
func (s *Snapshot) Keys() []string {
	keys := make([]string, 0, len(s.plugins))
	for k := range s.plugins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Replicas returns the instances registered for key.
func (s *Snapshot) Replicas(key string) []*Instance {
	rs, ok := s.plugins[key]
	if !ok {
		return nil
	}
	return rs.instances
}

// All returns every instance across every plugin, ordered by key.
func (s *Snapshot) All() []*Instance {
	out := make([]*Instance, 0, len(s.plugins))
	for _, k := range s.Keys() {
		out = append(out, s.plugins[k].instances...)
	}
	return out
}

// Registration is everything the registry needs to know about one plugin:
// which processes serve it, and what it declared.
type Registration struct {
	Key       string
	Instances []*Instance

	// Filters are the plugin's compiled lifecycle subscriptions.
	Filters []manifest.CompiledFilter

	// AllowIdentityMutation mirrors the filter:authenticate permission.
	AllowIdentityMutation bool
}

// Registry owns the current snapshot and serialises every change to it.
//
// Reads go through Current, which is a single atomic load with no lock, so
// request handling never contends with an admin enabling a plugin. Writes take
// a mutex, build a whole new snapshot, and publish it with one atomic store —
// there is no window in which a partially-updated routing table is visible.
type Registry struct {
	current atomic.Pointer[Snapshot]

	mu      sync.Mutex
	version uint64
	regs    map[string]Registration

	// filterDefaults fill in what a filter declaration leaves unset.
	filterDefaults pipeline.Defaults

	// onChange fires after every successful swap, with the new snapshot. Core
	// uses it to push a console refresh over SSE.
	onChange func(*Snapshot)
}

func NewRegistry() *Registry {
	r := &Registry{
		regs:           map[string]Registration{},
		filterDefaults: pipeline.DefaultDefaults(),
	}
	r.current.Store(emptySnapshot())
	return r
}

// SetFilterDefaults overrides the timeout and body ceiling applied to filters
// that do not specify their own.
func (r *Registry) SetFilterDefaults(d pipeline.Defaults) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filterDefaults = d
}

// OnChange registers the post-swap callback. It is invoked synchronously while
// the registry lock is held, so it must not block or call back into the
// registry.
func (r *Registry) OnChange(fn func(*Snapshot)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

// Current returns the live snapshot. Callers should load it once per request
// and reuse it, rather than calling this repeatedly mid-request.
func (r *Registry) Current() *Snapshot { return r.current.Load() }

// InstallPlugin registers a plugin — its processes and its declarations — and
// returns the instances it displaced, if any. The caller owns draining them.
//
// Publishing happens after the new instances are already running and healthy,
// so a failed launch never disturbs live traffic: the caller simply never gets
// here.
func (r *Registry) InstallPlugin(reg Registration) []*Instance {
	r.mu.Lock()
	defer r.mu.Unlock()

	displaced := r.current.Load().Replicas(reg.Key)
	if len(reg.Instances) == 0 {
		delete(r.regs, reg.Key)
	} else {
		r.regs[reg.Key] = reg
	}
	r.rebuildLocked()
	return displaced
}

// Install registers a plugin that declares no filters. It exists because most
// callers and tests only care about routing.
func (r *Registry) Install(key string, instances ...*Instance) []*Instance {
	return r.InstallPlugin(Registration{Key: key, Instances: instances})
}

// Remove takes a plugin out of rotation and returns its instances for
// draining. New requests stop reaching them the moment this returns.
func (r *Registry) Remove(key string) []*Instance {
	return r.InstallPlugin(Registration{Key: key})
}

// rebuildLocked constructs the next snapshot from the current registrations.
//
// Replica sets whose instances are unchanged are carried over by pointer
// rather than rebuilt, so enabling one plugin does not reset every other
// plugin's round-robin position.
func (r *Registry) rebuildLocked() {
	old := r.current.Load()
	next := &Snapshot{plugins: make(map[string]*replicaSet, len(r.regs))}

	pfs := make([]pipeline.PluginFilters, 0, len(r.regs))
	for key, reg := range r.regs {
		if prev, ok := old.plugins[key]; ok && sameInstances(prev.instances, reg.Instances) {
			next.plugins[key] = prev
		} else {
			next.plugins[key] = newReplicaSet(reg.Instances)
		}
		if len(reg.Filters) > 0 {
			pfs = append(pfs, pipeline.PluginFilters{
				Key:                   key,
				Filters:               reg.Filters,
				AllowIdentityMutation: reg.AllowIdentityMutation,
			})
		}
	}

	chain, err := pipeline.BuildChain(pfs, r.filterDefaults)
	if err != nil {
		// Filters are validated when a plugin is installed, so reaching here
		// means a bug rather than bad input. Serving an empty chain is the
		// safe response: filters stop running, but routing keeps working.
		logf("filter chain rebuild failed, continuing without filters: %v", err)
		chain = pipeline.EmptyChain()
	}
	next.chain = chain

	r.publishLocked(next)
}

func sameInstances(a, b []*Instance) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Swap atomically replaces a plugin's instances with fresh ones and drains the
// old ones in the background. This is the blue-green upgrade commit point.
//
// The new instances must already be launched and health-checked. By the time
// this is called there is no failure path left, which is precisely what makes
// everything before it safely abortable.
func (r *Registry) Swap(ctx context.Context, reg Registration, drainTimeout time.Duration) {
	displaced := r.InstallPlugin(reg)
	if len(displaced) == 0 {
		return
	}
	if drainTimeout <= 0 {
		drainTimeout = DefaultDrainTimeout
	}
	for _, old := range displaced {
		go func() {
			if err := old.Drain(ctx, drainTimeout); err != nil {
				// The process is dead either way; this is worth reporting
				// because a repeated drain timeout means requests are hanging.
				logf("plugin %s: %v", old.Key, err)
			}
		}()
	}
}

// DrainAll stops every plugin, used on Core shutdown.
func (r *Registry) DrainAll(ctx context.Context, drainTimeout time.Duration) {
	r.mu.Lock()
	old := r.current.Load()
	r.regs = map[string]Registration{}
	r.rebuildLocked()
	r.mu.Unlock()

	if drainTimeout <= 0 {
		drainTimeout = DefaultDrainTimeout
	}
	var wg sync.WaitGroup
	for _, inst := range old.All() {
		wg.Go(func() { _ = inst.Drain(ctx, drainTimeout) })
	}
	wg.Wait()
}

func (r *Registry) publishLocked(next *Snapshot) {
	r.version++
	next.version = r.version
	r.current.Store(next)
	if r.onChange != nil {
		r.onChange(next)
	}
}

// logf is a seam so the registry does not hard-depend on a logger. Core
// replaces it during startup.
var logf = func(format string, args ...any) {
	fmt.Printf("[pluginhost] "+format+"\n", args...)
}

// SetLogger installs the process-wide log sink for lifecycle events.
func SetLogger(fn func(format string, args ...any)) {
	if fn != nil {
		logf = fn
	}
}
