package hostsvc

import (
	"context"
	"testing"
	"time"

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
