package pluginhost

import (
	"sync/atomic"
	"time"
)

// BreakerConfig tunes when a misbehaving plugin is taken out of the request
// path.
type BreakerConfig struct {
	// FailureThreshold is how many consecutive failures trip the breaker.
	FailureThreshold int32

	// OpenDuration is how long the breaker stays open before allowing a single
	// probe through.
	OpenDuration time.Duration

	// HalfOpenSuccesses is how many consecutive probe successes are needed to
	// close the breaker again.
	HalfOpenSuccesses int32
}

// DefaultBreakerConfig is deliberately forgiving on the way in and quick on
// the way out: a plugin that fails five calls in a row is almost certainly
// broken rather than unlucky, and five seconds is short enough that a
// recovered plugin returns to service quickly.
func DefaultBreakerConfig() BreakerConfig {
	return BreakerConfig{
		FailureThreshold:  5,
		OpenDuration:      5 * time.Second,
		HalfOpenSuccesses: 2,
	}
}

// Breaker is a per-instance circuit breaker.
//
// It is deliberately attached to the instance rather than to each filter
// phase: when a plugin's connection breaks, every call to it fails, so
// counting phases separately would just produce several views of one fault.
//
// Allow is on the hot path of every filtered request, so it is lock-free.
type Breaker struct {
	cfg BreakerConfig

	consecutiveFailures atomic.Int32
	openUntilNanos      atomic.Int64
	halfOpenSuccesses   atomic.Int32
	probing             atomic.Bool

	// now is injectable so tests can drive the clock instead of sleeping.
	now func() time.Time
}

func NewBreaker(cfg BreakerConfig) *Breaker {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 5
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 5 * time.Second
	}
	if cfg.HalfOpenSuccesses <= 0 {
		cfg.HalfOpenSuccesses = 2
	}
	return &Breaker{cfg: cfg, now: time.Now}
}

// SetClock overrides the time source. Test-only.
func (b *Breaker) SetClock(now func() time.Time) { b.now = now }

// Allow reports whether a call may proceed.
//
// While open it admits exactly one probe per OpenDuration window; everything
// else is rejected without touching the plugin.
func (b *Breaker) Allow() bool {
	until := b.openUntilNanos.Load()
	if until == 0 {
		return true // closed: the common case, one atomic load
	}
	if b.now().UnixNano() < until {
		return false // open
	}
	// Half-open: let a single probe through so a recovered plugin can prove
	// itself without a thundering herd hitting it at once.
	return b.probing.CompareAndSwap(false, true)
}

// Open reports whether the breaker is currently rejecting calls. It does not
// consume a probe slot, so it is safe for diagnostics.
func (b *Breaker) Open() bool {
	until := b.openUntilNanos.Load()
	return until != 0 && b.now().UnixNano() < until
}

// RecordSuccess reports a successful call.
func (b *Breaker) RecordSuccess() {
	b.consecutiveFailures.Store(0)

	if b.openUntilNanos.Load() == 0 {
		return // already closed, nothing to do
	}
	// A probe succeeded. Close only after enough consecutive successes, so a
	// single lucky call does not restore full traffic to a flapping plugin.
	if b.halfOpenSuccesses.Add(1) >= b.cfg.HalfOpenSuccesses {
		b.openUntilNanos.Store(0)
		b.halfOpenSuccesses.Store(0)
	}
	b.probing.Store(false)
}

// RecordFailure reports a failed call: a transport error, a timeout, or a
// plugin that returned an error status.
func (b *Breaker) RecordFailure() {
	b.halfOpenSuccesses.Store(0)

	if b.openUntilNanos.Load() != 0 {
		// A probe failed: extend the open window rather than closing.
		b.openUntilNanos.Store(b.now().Add(b.cfg.OpenDuration).UnixNano())
		b.probing.Store(false)
		return
	}
	if b.consecutiveFailures.Add(1) >= b.cfg.FailureThreshold {
		b.openUntilNanos.Store(b.now().Add(b.cfg.OpenDuration).UnixNano())
		b.consecutiveFailures.Store(0)
	}
}

// Reset returns the breaker to its initial closed state.
func (b *Breaker) Reset() {
	b.consecutiveFailures.Store(0)
	b.openUntilNanos.Store(0)
	b.halfOpenSuccesses.Store(0)
	b.probing.Store(false)
}
