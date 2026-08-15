package pluginhost

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock lets breaker tests advance time deterministically instead of
// sleeping, which keeps them fast and free of timing flakes.
type fakeClock struct{ nanos atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.nanos.Add(int64(d)) }

func newTestBreaker(clock *fakeClock) *Breaker {
	b := NewBreaker(BreakerConfig{
		FailureThreshold:  3,
		OpenDuration:      5 * time.Second,
		HalfOpenSuccesses: 2,
	})
	b.SetClock(clock.Now)
	return b
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	for i := range 2 {
		b.RecordFailure()
		if !b.Allow() {
			t.Fatalf("breaker opened after %d failures, threshold is 3", i+1)
		}
	}

	b.RecordFailure() // third
	if b.Allow() {
		t.Fatal("breaker did not open at the failure threshold")
	}
	if !b.Open() {
		t.Error("Open() disagrees with Allow()")
	}
}

// A success before the threshold must clear the streak, otherwise an
// occasional failure would eventually trip a perfectly healthy plugin.
func TestBreakerSuccessResetsFailureStreak(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	b.RecordFailure()
	b.RecordFailure()
	b.RecordSuccess()
	b.RecordFailure()
	b.RecordFailure()

	if !b.Allow() {
		t.Fatal("breaker opened even though a success reset the streak")
	}
}

func TestBreakerHalfOpenAdmitsOneProbe(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	for range 3 {
		b.RecordFailure()
	}
	if b.Allow() {
		t.Fatal("breaker should be open")
	}

	clock.Advance(6 * time.Second)

	if !b.Allow() {
		t.Fatal("half-open breaker refused the first probe")
	}
	// Only one probe at a time: a recovering plugin must not be hit by every
	// waiting request at once.
	if b.Allow() {
		t.Error("half-open breaker admitted a second concurrent probe")
	}
}

func TestBreakerClosesAfterEnoughProbeSuccesses(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	for range 3 {
		b.RecordFailure()
	}
	clock.Advance(6 * time.Second)

	if !b.Allow() {
		t.Fatal("expected a probe slot")
	}
	b.RecordSuccess() // 1 of 2

	if b.Open() {
		t.Error("breaker closed after a single probe success; it needs 2")
	}

	if !b.Allow() {
		t.Fatal("expected a second probe slot")
	}
	b.RecordSuccess() // 2 of 2

	if b.Open() {
		t.Error("breaker did not close after enough probe successes")
	}
	// Fully closed means unlimited traffic again.
	for range 5 {
		if !b.Allow() {
			t.Fatal("closed breaker rejected a call")
		}
	}
}

func TestBreakerProbeFailureReopens(t *testing.T) {
	clock := newFakeClock()
	b := newTestBreaker(clock)

	for range 3 {
		b.RecordFailure()
	}
	clock.Advance(6 * time.Second)

	if !b.Allow() {
		t.Fatal("expected a probe slot")
	}
	b.RecordFailure() // probe failed

	if !b.Open() {
		t.Fatal("breaker did not reopen after a failed probe")
	}
	// The window must restart, not resume: advancing past the *original*
	// deadline should still find it open.
	clock.Advance(4 * time.Second)
	if !b.Open() {
		t.Error("open window did not restart after the failed probe")
	}
}

func TestBreakerAllowIsRaceFree(t *testing.T) {
	b := NewBreaker(DefaultBreakerConfig())

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for i := range 1000 {
				b.Allow()
				if i%3 == 0 {
					b.RecordFailure()
				} else {
					b.RecordSuccess()
				}
			}
		})
	}
	wg.Wait()
}
