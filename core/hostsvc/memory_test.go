package hostsvc

import (
	"context"
	"testing"
	"time"
)

// The cache and the locks agree on what "expired" means.
//
// They had drifted: lock acquisition treated now == expiresAt as free, while
// the cache and the lock sweeper treated it as still live. Worth being precise
// about how much that mattered, because the answer is "less than it looks".
// Acquisition's boundary was already the correct one, and the sweeper is a
// memory-reclaim pass whose verdict nothing depends on — acquisition rechecks.
// So the only boundary this change actually moves is the cache's, by one tick,
// and reverting that half is the only mutation this test catches.
//
// The value is in there being one predicate rather than three copies. Two of
// them had already disagreed without anything noticing, which is the condition
// under which the third one breaks something.
func TestCacheAndLocksExpireAtTheSameInstant(t *testing.T) {
	at := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := at
	now := func() time.Time { return clock }

	cache := NewMemoryCache(10)
	cache.SetClock(now)
	locks := NewMemoryLocks()
	locks.SetClock(now)

	ctx := context.Background()
	const ttl = time.Minute

	cache.Set(ctx, "p", "k", []byte("v"), ttl)
	if _, ok, _ := locks.Acquire(ctx, "p", "job", ttl, 0); !ok {
		t.Fatal("could not take a free lock")
	}

	// One tick before the deadline: both still live.
	clock = at.Add(ttl - time.Nanosecond)
	if _, ok := cache.Get(ctx, "p", "k"); !ok {
		t.Error("the cache entry expired a tick early")
	}
	if _, ok, _ := locks.Acquire(ctx, "p", "job", ttl, 0); ok {
		t.Error("the lock was released a tick early")
	}

	// Exactly at the deadline: both over. This is the instant they disagreed
	// about.
	clock = at.Add(ttl)
	if _, ok := cache.Get(ctx, "p", "k"); ok {
		t.Error("the cache entry is still live at its deadline")
	}
	if _, ok, _ := locks.Acquire(ctx, "p", "job", ttl, 0); !ok {
		t.Error("the lock is still held at its deadline")
	}
}
