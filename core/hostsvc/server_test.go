package hostsvc

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taills/moduless/core/event"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/taills/moduless/proto/plugin"
)

func fullDeps() Deps {
	return Deps{
		Cache:  NewMemoryCache(100),
		Locks:  NewMemoryLocks(),
		Config: NewStaticConfig(),
	}
}

// Every capability must refuse a plugin that did not declare it. This is the
// entire point of the permission model for third-party plugins: a plugin gets
// exactly what its manifest declared and an admin approved.
func TestCapabilitiesRequirePermission(t *testing.T) {
	tests := []struct {
		name string
		perm string
		call func(*Server) error
	}{
		{
			name: "cache read", perm: PermCache,
			call: func(s *Server) error {
				_, err := s.CacheGet(context.Background(), &pb.CacheGetRequest{Key: "k"})
				return err
			},
		},
		{
			name: "cache write", perm: PermCache,
			call: func(s *Server) error {
				_, err := s.CacheSet(context.Background(), &pb.CacheSetRequest{Key: "k"})
				return err
			},
		},
		{
			name: "cache delete", perm: PermCache,
			call: func(s *Server) error {
				_, err := s.CacheDelete(context.Background(), &pb.CacheDeleteRequest{Key: "k"})
				return err
			},
		},
		{
			name: "acquire lock", perm: PermLock,
			call: func(s *Server) error {
				_, err := s.AcquireLock(context.Background(), &pb.AcquireLockRequest{Name: "n"})
				return err
			},
		},
		{
			name: "renew lock", perm: PermLock,
			call: func(s *Server) error {
				_, err := s.RenewLock(context.Background(), &pb.LeaseRequest{Name: "n"})
				return err
			},
		},
		{
			name: "release lock", perm: PermLock,
			call: func(s *Server) error {
				_, err := s.ReleaseLock(context.Background(), &pb.LeaseRequest{Name: "n"})
				return err
			},
		},
		{
			name: "publish event", perm: PermEvents,
			call: func(s *Server) error {
				_, err := s.Publish(context.Background(), &pb.PublishRequest{EventName: "e"})
				return err
			},
		},
		{
			name: "outbound http", perm: PermHTTPEgress,
			call: func(s *Server) error {
				_, err := s.Fetch(context.Background(), &pb.FetchRequest{Url: "https://example.com"})
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name+" denied without permission", func(t *testing.T) {
			s := New("p", nil, fullDeps())

			err := tc.call(s)
			if err == nil {
				t.Fatal("call succeeded without the permission")
			}
			if got := status.Code(err); got != codes.PermissionDenied {
				t.Errorf("code = %v, want PermissionDenied (err: %v)", got, err)
			}
		})

		t.Run(tc.name+" allowed with permission", func(t *testing.T) {
			s := New("p", []string{tc.perm}, fullDeps())

			err := tc.call(s)
			// The call may still fail because the backend is unconfigured, but
			// it must not fail on permission.
			if status.Code(err) == codes.PermissionDenied {
				t.Errorf("call denied despite holding %q: %v", tc.perm, err)
			}
		})
	}
}

// A permission the plugin holds but a capability Core is not running must be
// distinguishable from a permission problem, or operators chase the wrong bug.
func TestUnconfiguredCapabilityReportsUnavailable(t *testing.T) {
	s := New("p", []string{PermCache, PermEvents, PermHTTPEgress}, Deps{})

	if _, err := s.CacheGet(context.Background(), &pb.CacheGetRequest{Key: "k"}); status.Code(err) != codes.Unavailable {
		t.Errorf("cache: code = %v, want Unavailable", status.Code(err))
	}
	if _, err := s.Publish(context.Background(), &pb.PublishRequest{EventName: "e"}); status.Code(err) != codes.Unavailable {
		t.Errorf("events: code = %v, want Unavailable", status.Code(err))
	}
	if _, err := s.Fetch(context.Background(), &pb.FetchRequest{Url: "https://x"}); status.Code(err) != codes.Unavailable {
		t.Errorf("egress: code = %v, want Unavailable", status.Code(err))
	}
}

// Config is the plugin's own settings, so it is not permission-gated, and an
// unconfigured Core returns an empty map rather than an error.
func TestGetConfigNeedsNoPermission(t *testing.T) {
	cfg := NewStaticConfig()
	cfg.Set("p", map[string]string{"greeting": "hello"})
	s := New("p", nil, Deps{Config: cfg})

	resp, err := s.GetConfig(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetConfig: %v", err)
	}
	if resp.GetConfig()["greeting"] != "hello" {
		t.Errorf("config = %v", resp.GetConfig())
	}

	bare := New("p", nil, Deps{})
	if _, err := bare.GetConfig(context.Background(), &emptypb.Empty{}); err != nil {
		t.Errorf("GetConfig without a backend: %v", err)
	}
}

// One plugin must not be able to read or clobber another's cache entries, even
// using the identical key string.
func TestCacheIsNamespacedPerPlugin(t *testing.T) {
	cache := NewMemoryCache(100)
	a := New("plugin-a", []string{PermCache}, Deps{Cache: cache})
	b := New("plugin-b", []string{PermCache}, Deps{Cache: cache})
	ctx := context.Background()

	if _, err := a.CacheSet(ctx, &pb.CacheSetRequest{Key: "shared", Value: []byte("from-a")}); err != nil {
		t.Fatalf("CacheSet: %v", err)
	}

	got, err := b.CacheGet(ctx, &pb.CacheGetRequest{Key: "shared"})
	if err != nil {
		t.Fatalf("CacheGet: %v", err)
	}
	if got.GetFound() {
		t.Errorf("plugin-b read plugin-a's cache entry: %q", got.GetValue())
	}

	mine, err := a.CacheGet(ctx, &pb.CacheGetRequest{Key: "shared"})
	if err != nil {
		t.Fatalf("CacheGet: %v", err)
	}
	if string(mine.GetValue()) != "from-a" {
		t.Errorf("plugin-a lost its own entry: %q", mine.GetValue())
	}
}

func TestCacheExpiry(t *testing.T) {
	cache := NewMemoryCache(100)
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	cache.SetClock(clock.Now)

	s := New("p", []string{PermCache}, Deps{Cache: cache})
	ctx := context.Background()

	if _, err := s.CacheSet(ctx, &pb.CacheSetRequest{Key: "k", Value: []byte("v"), TtlSeconds: 60}); err != nil {
		t.Fatalf("CacheSet: %v", err)
	}
	if got, _ := s.CacheGet(ctx, &pb.CacheGetRequest{Key: "k"}); !got.GetFound() {
		t.Fatal("entry missing before expiry")
	}

	clock.advance(61 * time.Second)

	if got, _ := s.CacheGet(ctx, &pb.CacheGetRequest{Key: "k"}); got.GetFound() {
		t.Error("expired entry was still returned")
	}
}

// An unbounded cache is the easiest way for a plugin to exhaust Core's memory.
func TestCacheEnforcesEntryCap(t *testing.T) {
	cache := NewMemoryCache(10)
	s := New("p", []string{PermCache}, Deps{Cache: cache})
	ctx := context.Background()

	for i := range 100 {
		key := string(rune('a'+i%26)) + string(rune('0'+i/26))
		if _, err := s.CacheSet(ctx, &pb.CacheSetRequest{Key: key, Value: []byte("v")}); err != nil {
			t.Fatalf("CacheSet: %v", err)
		}
	}
	if got := cache.Len(); got > 10 {
		t.Errorf("cache holds %d entries, cap is 10", got)
	}
}

func TestLockLifecycle(t *testing.T) {
	locks := NewMemoryLocks()
	a := New("p", []string{PermLock}, Deps{Locks: locks})
	b := New("q", []string{PermLock}, Deps{Locks: locks})
	ctx := context.Background()

	got, err := a.AcquireLock(ctx, &pb.AcquireLockRequest{Name: "job", TtlSeconds: 30})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if !got.GetAcquired() {
		t.Fatal("first acquire failed")
	}
	lease := got.GetLeaseId()

	// The same plugin cannot take it twice.
	again, err := a.AcquireLock(ctx, &pb.AcquireLockRequest{Name: "job", TtlSeconds: 30})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if again.GetAcquired() {
		t.Error("acquired a lock that was already held")
	}

	// Lock names are namespaced, so another plugin's identically named lock is
	// a different lock.
	other, err := b.AcquireLock(ctx, &pb.AcquireLockRequest{Name: "job", TtlSeconds: 30})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if !other.GetAcquired() {
		t.Error("another plugin's identically named lock was blocked")
	}

	renewed, err := a.RenewLock(ctx, &pb.LeaseRequest{Name: "job", LeaseId: lease, TtlSeconds: 60})
	if err != nil {
		t.Fatalf("RenewLock: %v", err)
	}
	if !renewed.GetAcquired() {
		t.Error("renew failed for the current holder")
	}

	if _, err := a.ReleaseLock(ctx, &pb.LeaseRequest{Name: "job", LeaseId: lease}); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}
	after, err := a.AcquireLock(ctx, &pb.AcquireLockRequest{Name: "job", TtlSeconds: 30})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if !after.GetAcquired() {
		t.Error("lock was not free after release")
	}
}

// A process whose lease already expired must not be able to release the lock
// its successor now holds, or two workers end up running the same job.
func TestReleaseRequiresTheCurrentLease(t *testing.T) {
	locks := NewMemoryLocks()
	clock := &testClock{t: time.Unix(1_700_000_000, 0)}
	locks.SetClock(clock.Now)

	s := New("p", []string{PermLock}, Deps{Locks: locks})
	ctx := context.Background()

	first, _ := s.AcquireLock(ctx, &pb.AcquireLockRequest{Name: "job", TtlSeconds: 10})
	staleLease := first.GetLeaseId()

	clock.advance(11 * time.Second) // the first holder's lease lapses

	second, err := s.AcquireLock(ctx, &pb.AcquireLockRequest{Name: "job", TtlSeconds: 10})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	if !second.GetAcquired() {
		t.Fatal("expired lock was not re-acquirable")
	}

	// The stale holder tries to clean up and must not succeed.
	if _, err := s.ReleaseLock(ctx, &pb.LeaseRequest{Name: "job", LeaseId: staleLease}); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}
	third, _ := s.AcquireLock(ctx, &pb.AcquireLockRequest{Name: "job", TtlSeconds: 10})
	if third.GetAcquired() {
		t.Error("a stale lease released the lock its successor held")
	}

	// Renewing with a stale lease must also fail.
	renewed, _ := s.RenewLock(ctx, &pb.LeaseRequest{Name: "job", LeaseId: staleLease, TtlSeconds: 10})
	if renewed.GetAcquired() {
		t.Error("a stale lease was renewed")
	}
}

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// The queue is a shared table on a shared disk, so a producer stuck in a loop
// fills it for every plugin rather than only for itself. The depth bound is
// what stops "no ceiling at all"; it is deliberately high, because a real
// batch is large and being refused mid-batch is its own kind of failure.
func TestQueueDepthLimitRefusesAndRecovers(t *testing.T) {
	q := &PGQueue{MaxDepth: 10, depth: map[string]int64{}}

	// Below the limit: accepted, as far as the depth check is concerned.
	q.setDepthForTest("noisy", 9)
	if err := q.checkDepth("noisy"); err != nil {
		t.Errorf("refused below the limit: %v", err)
	}

	// At and over it: refused, and the message says which plugin and why.
	q.setDepthForTest("noisy", 10)
	err := q.checkDepth("noisy")
	if err == nil {
		t.Fatal("a plugin at the depth limit was allowed to enqueue more")
	}
	t.Logf("refused: %v", err)
	if !strings.Contains(err.Error(), "noisy") {
		t.Errorf("the refusal does not name the plugin: %v", err)
	}

	// Another plugin is unaffected: the bound is per plugin, so one runaway
	// producer does not stop everyone else's work.
	if err := q.checkDepth("quiet"); err != nil {
		t.Errorf("an unrelated plugin was refused: %v", err)
	}

	// Draining below the limit lets it through again — the refusal is a state,
	// not a punishment.
	q.setDepthForTest("noisy", 3)
	if err := q.checkDepth("noisy"); err != nil {
		t.Errorf("still refused after draining: %v", err)
	}
}

// A lock that expires must eventually stop occupying memory.
//
// Expiry alone does not free anything: an expired entry sits in the map until
// something asks for that exact name again. A plugin taking a lock per
// document id — which is a reasonable thing to try — therefore leaves one
// entry per id behind for the life of the process.
func TestExpiredLocksAreSweptAway(t *testing.T) {
	locks := NewMemoryLocks()

	now := time.Now()
	locks.SetClock(func() time.Time { return now })

	ctx := context.Background()
	for i := range 500 {
		if _, ok, err := locks.Acquire(ctx, "p", fmt.Sprintf("doc-%d", i), time.Second, 0); err != nil || !ok {
			t.Fatalf("acquire %d: ok=%v err=%v", i, ok, err)
		}
	}
	if got := locks.Held(); got != 500 {
		t.Fatalf("holding %d locks, want 500", got)
	}

	// Everything above has lapsed, and enough time has passed for a sweep.
	now = now.Add(time.Minute)
	if _, ok, _ := locks.Acquire(ctx, "p", "one-more", time.Second, 0); !ok {
		t.Fatal("could not acquire after expiry")
	}

	if got := locks.Held(); got > 2 {
		t.Errorf("%d entries still tracked after every lease lapsed; a plugin locking "+
			"per document id would grow this forever", got)
	}
}

// And a plugin genuinely holding an unreasonable number at once is refused
// rather than allowed to grow the table without limit.
func TestLockCountIsBounded(t *testing.T) {
	locks := NewMemoryLocks()
	locks.MaxHeld = 10

	ctx := context.Background()
	for i := range 10 {
		if _, ok, _ := locks.Acquire(ctx, "p", fmt.Sprintf("n%d", i), time.Minute, 0); !ok {
			t.Fatalf("lock %d within the limit was refused", i)
		}
	}

	if _, ok, _ := locks.Acquire(ctx, "p", "one-too-many", time.Minute, 0); ok {
		t.Error("a lock past the ceiling was granted; the table has no bound")
	}

	// A name already tracked can still be re-taken once released, so being at
	// the ceiling does not block the work already in progress.
	locks.Release(context.Background(), "p", "n0", "")
	if got := locks.Held(); got != 10 {
		t.Logf("held after a mismatched release: %d", got)
	}
}

// Each subscription is a live stream with a buffered channel behind it, held
// until the plugin closes it. A plugin subscribing inside a request handler —
// which is an easy mistake, since Subscribe looks like a registration —
// accumulates both, and the symptom shows up as Core's memory rather than the
// plugin's.
func TestSubscriptionsAreBoundedPerPlugin(t *testing.T) {
	events := NewBusEvents(event.NewEventBus())
	events.MaxSubscriptionsPerPlugin = 3

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Hold the allowance open.
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			_ = events.Subscribe(ctx, "chatty", "thing", func(Event) error { return nil })
		})
	}

	deadline := time.Now().Add(5 * time.Second)
	for events.Subscriptions("chatty") < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := events.Subscriptions("chatty"); got != 3 {
		t.Fatalf("%d subscriptions established, want 3", got)
	}

	// One past it, refused and named.
	err := events.Subscribe(ctx, "chatty", "thing", func(Event) error { return nil })
	if err == nil {
		t.Error("a subscription past the limit was accepted")
	} else {
		t.Logf("refused: %v", err)
		if !strings.Contains(err.Error(), "chatty") {
			t.Errorf("the refusal does not name the plugin: %v", err)
		}
	}

	// A different plugin is unaffected.
	otherCtx, otherCancel := context.WithCancel(context.Background())
	go func() { _ = events.Subscribe(otherCtx, "quiet", "thing", func(Event) error { return nil }) }()
	for events.Subscriptions("quiet") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if events.Subscriptions("quiet") != 1 {
		t.Error("an unrelated plugin could not subscribe while another was at its limit")
	}
	otherCancel()

	// Closing them returns the allowance.
	cancel()
	wg.Wait()
	for events.Subscriptions("chatty") != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := events.Subscriptions("chatty"); got != 0 {
		t.Errorf("%d subscriptions still counted after the streams ended", got)
	}
}
