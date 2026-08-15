package hostsvc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// The cache and lock implementations here are in-process on purpose.
//
// Core is a single-instance design (see docs/deployment.md), and every plugin
// replica is a child of that one process, so an in-process lock is already
// correct for the only topology that exists. The interfaces are still written
// in distributed terms — leases with ids and expiry rather than a plain mutex
// — so swapping in a PostgreSQL or Redis implementation later is a change of
// backend, not a change of contract.
//
// Which is why every method takes a context it does not currently use: a
// network-backed implementation has to be cancellable, and adding the
// parameter later would break the promise above at the one moment it was
// supposed to pay off.

// --- Cache ------------------------------------------------------------------

type cacheEntry struct {
	value     []byte
	expiresAt time.Time
}

// MemoryCache is a TTL cache with a hard entry cap.
//
// The cap matters: a cache is the easiest way for a misbehaving plugin to
// exhaust Core's memory, and unlike its own heap this one is not bounded by
// the plugin's cgroup.
type MemoryCache struct {
	mu         sync.Mutex
	entries    map[string]cacheEntry
	maxEntries int
	now        func() time.Time
}

// DefaultCacheEntries caps total cached entries across all plugins.
const DefaultCacheEntries = 10000

func NewMemoryCache(maxEntries int) *MemoryCache {
	if maxEntries <= 0 {
		maxEntries = DefaultCacheEntries
	}
	return &MemoryCache{
		entries:    make(map[string]cacheEntry),
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

// SetClock overrides the time source. Test-only.
func (c *MemoryCache) SetClock(now func() time.Time) { c.now = now }

// cacheKey namespaces entries per plugin so one plugin cannot read or clobber
// another's keys.
func cacheKey(pluginKey, key string) string { return pluginKey + "\x00" + key }

// expired answers "is this deadline past" for both the cache and the locks.
//
// One function because there were three copies and two of them disagreed at
// the boundary: the cache and the lock sweeper treated now == expiresAt as
// still live, lock acquisition treated it as free. Nothing observable came of
// it — acquisition's boundary was the right one and the sweeper only reclaims
// memory — but copies that have already drifted a tick apart unnoticed are the
// condition under which the next copy breaks something.
//
// The rule: a deadline is live while now is strictly before it. A zero time
// means no deadline.
func expired(now, deadline time.Time) bool {
	return !deadline.IsZero() && !now.Before(deadline)
}

func (c *MemoryCache) Get(_ context.Context, pluginKey, key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[cacheKey(pluginKey, key)]
	if !ok {
		return nil, false
	}
	if expired(c.now(), e.expiresAt) {
		delete(c.entries, cacheKey(pluginKey, key))
		return nil, false
	}
	return e.value, true
}

func (c *MemoryCache) Set(_ context.Context, pluginKey, key string, value []byte, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= c.maxEntries {
		c.evictLocked()
	}

	var expires time.Time
	if ttl > 0 {
		expires = c.now().Add(ttl)
	}
	c.entries[cacheKey(pluginKey, key)] = cacheEntry{value: value, expiresAt: expires}
}

func (c *MemoryCache) Delete(_ context.Context, pluginKey, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, cacheKey(pluginKey, key))
}

// evictLocked drops expired entries first, and if that frees nothing, removes
// an arbitrary tenth of the cache. Approximate eviction is acceptable here —
// callers must already handle a miss — and it avoids the bookkeeping a strict
// LRU would add to every read.
func (c *MemoryCache) evictLocked() {
	now := c.now()
	freed := 0
	for k, e := range c.entries {
		if expired(now, e.expiresAt) {
			delete(c.entries, k)
			freed++
		}
	}
	if freed > 0 {
		return
	}
	target := len(c.entries) / 10
	if target == 0 {
		target = 1
	}
	for k := range c.entries {
		delete(c.entries, k)
		target--
		if target <= 0 {
			return
		}
	}
}

// Len reports the number of cached entries, for diagnostics and tests.
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// --- Locks ------------------------------------------------------------------

type lockState struct {
	leaseID   string
	expiresAt time.Time
}

// MemoryLocks hands out leases rather than holding mutexes, so a plugin that
// crashes while holding a lock cannot deadlock the system: the lease simply
// expires.
type MemoryLocks struct {
	mu   sync.Mutex
	held map[string]lockState
	now  func() time.Time

	// MaxHeld bounds how many locks may be held at once, across all plugins.
	//
	// Expiry alone does not bound this: an expired entry stays in the map
	// until something asks for that exact name again, so a plugin taking a
	// lock per document id leaves one entry per id behind forever. Sweeping
	// handles that; the cap handles the other case, a plugin genuinely holding
	// an unreasonable number at once.
	MaxHeld int

	lastSweep time.Time
}

// DefaultMaxLocks is the ceiling on simultaneously held locks.
//
// Locks coordinate work, so the count tracks concurrency rather than data
// volume: a plugin needing more than this at one moment is using them as a
// keyed map rather than as mutual exclusion.
const DefaultMaxLocks = 10_000

func NewMemoryLocks() *MemoryLocks {
	return &MemoryLocks{
		held:      make(map[string]lockState),
		now:       time.Now,
		MaxHeld:   DefaultMaxLocks,
		lastSweep: time.Now(),
	}
}

// Held reports how many lock entries are being tracked, for diagnostics.
func (l *MemoryLocks) Held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

// sweepExpiredLocked drops entries whose lease has lapsed.
//
// Amortised onto acquisition rather than run on a timer: locks are only ever
// created by acquiring one, so the work happens exactly when the map might be
// growing and never on an idle system.
func (l *MemoryLocks) sweepExpiredLocked(now time.Time) {
	if now.Sub(l.lastSweep) < lockSweepInterval {
		return
	}
	l.lastSweep = now
	for k, st := range l.held {
		if expired(now, st.expiresAt) {
			delete(l.held, k)
		}
	}
}

const lockSweepInterval = 30 * time.Second

// SetClock overrides the time source. Test-only.
func (l *MemoryLocks) SetClock(now func() time.Time) { l.now = now }

func lockKey(pluginKey, name string) string { return pluginKey + "\x00" + name }

// DefaultLockTTL bounds how long a lock is held if the owner never releases it.
const DefaultLockTTL = 30 * time.Second

func (l *MemoryLocks) Acquire(ctx context.Context, pluginKey, name string, ttl, wait time.Duration) (Lease, bool, error) {
	if ttl <= 0 {
		ttl = DefaultLockTTL
	}
	deadline := l.now().Add(wait)

	for {
		if lease, ok := l.tryAcquire(pluginKey, name, ttl); ok {
			return lease, true, nil
		}
		if wait <= 0 || !l.now().Before(deadline) {
			return Lease{}, false, nil
		}
		select {
		case <-ctx.Done():
			return Lease{}, false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (l *MemoryLocks) tryAcquire(pluginKey, name string, ttl time.Duration) (Lease, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	k := lockKey(pluginKey, name)
	now := l.now()
	l.sweepExpiredLocked(now)

	if cur, ok := l.held[k]; ok && !expired(now, cur.expiresAt) {
		return Lease{}, false
	}

	// Refuse a new name once the table is full, rather than growing without
	// bound. Re-taking a name already tracked is always allowed, so a plugin
	// at the ceiling can still renew the work it is actually doing.
	if _, existing := l.held[k]; !existing && l.maxHeld() > 0 && len(l.held) >= l.maxHeld() {
		return Lease{}, false
	}

	lease := Lease{ID: randomLeaseID(), ExpiresAt: now.Add(ttl)}
	l.held[k] = lockState{leaseID: lease.ID, expiresAt: lease.ExpiresAt}
	return lease, true
}

// Renew extends a lease. It fails when the caller no longer owns the lock,
// which is the signal that its work may have been taken over by someone else.
func (l *MemoryLocks) Renew(_ context.Context, pluginKey, name, leaseID string, ttl time.Duration) (Lease, bool) {
	if ttl <= 0 {
		ttl = DefaultLockTTL
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	k := lockKey(pluginKey, name)
	cur, ok := l.held[k]
	if !ok || cur.leaseID != leaseID || expired(l.now(), cur.expiresAt) {
		return Lease{}, false
	}
	cur.expiresAt = l.now().Add(ttl)
	l.held[k] = cur
	return Lease{ID: leaseID, ExpiresAt: cur.expiresAt}, true
}

// Release drops a lock only if the caller still owns it. Checking the lease id
// is what stops a process whose lease already expired from releasing the lock
// its successor now holds.
func (l *MemoryLocks) Release(_ context.Context, pluginKey, name, leaseID string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	k := lockKey(pluginKey, name)
	if cur, ok := l.held[k]; ok && cur.leaseID == leaseID {
		delete(l.held, k)
	}
}

func (l *MemoryLocks) maxHeld() int {
	if l.MaxHeld > 0 {
		return l.MaxHeld
	}
	return DefaultMaxLocks
}

func randomLeaseID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to a time-based id
		// rather than panicking inside a request.
		return hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// --- Config -----------------------------------------------------------------

// StaticConfig serves configuration from an in-memory map, used before the
// database-backed config store exists and in tests.
type StaticConfig struct {
	mu       sync.RWMutex
	byPlugin map[string]map[string]string
}

func NewStaticConfig() *StaticConfig {
	return &StaticConfig{byPlugin: map[string]map[string]string{}}
}

func (c *StaticConfig) Get(_ context.Context, pluginKey string) (map[string]string, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	src := c.byPlugin[pluginKey]
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out, nil
}

// Set replaces a plugin's configuration.
func (c *StaticConfig) Set(pluginKey string, cfg map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	copied := make(map[string]string, len(cfg))
	for k, v := range cfg {
		copied[k] = v
	}
	c.byPlugin[pluginKey] = copied
}
