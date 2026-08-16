package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The lock is the only reason this example exists, and the part of it that
// matters most — renewing the lease while the work runs, and stopping when the
// renewal says the lock is gone — is invisible in production until two replicas
// have already written the same account twice.

// --- a lease that records what was asked of it --------------------------

type fakeLease struct {
	mu       sync.Mutex
	renewals int
	released int
	// limited caps how many renewals succeed; holds is that cap, so
	// limited with holds 0 means the very first renewal reports the lock gone.
	// Without the separate flag, "fail immediately" and "never fail" would both
	// be holds == 0.
	limited  bool
	holds    int
	renewErr error
}

func (f *fakeLease) Renew(_ context.Context, _ time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renewals++
	if f.renewErr != nil {
		return false, f.renewErr
	}
	if f.limited && f.renewals > f.holds {
		return false, nil
	}
	return true, nil
}

func (f *fakeLease) Release(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released++
	return nil
}

func (f *fakeLease) counts() (renewals, released int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.renewals, f.released
}

// --- harness ------------------------------------------------------------

type lockCall struct {
	name string
	ttl  time.Duration
	wait time.Duration
}

type harness struct {
	mu        sync.Mutex
	calls     []lockCall
	published []job

	lease      *fakeLease
	granted    []bool // one entry per Acquire, in order; short means "granted"
	publishErr error
}

// newHarness wires the seams and sizes the timings.
//
// The renewal interval is (work + leaseMargin)/3, not leaseMargin/3 — the first
// version of these tests assumed the latter and expected five renewals where
// one happened. The arithmetic is stated here rather than guessed at, and tick
// is returned so a test can say what it depends on.
func newHarness(t *testing.T, work time.Duration) (*harness, time.Duration) {
	t.Helper()

	h := &harness{lease: &fakeLease{}}

	settings.Lock()
	settings.work, settings.lockWait = work, 0
	settings.Unlock()

	// Small enough that a renewal happens inside a test; the production value
	// is restored on cleanup.
	prevMargin := leaseMargin
	leaseMargin = 30 * time.Millisecond
	tick := (work + leaseMargin) / 3

	prevAcquire, prevRepublish := acquire, republish
	acquire = func(_ context.Context, name string, ttl, wait time.Duration) (lease, bool, error) {
		h.mu.Lock()
		n := len(h.calls)
		h.calls = append(h.calls, lockCall{name, ttl, wait})
		granted := n >= len(h.granted) || h.granted[n]
		h.mu.Unlock()
		if !granted {
			return nil, false, nil
		}
		return h.lease, true, nil
	}
	republish = func(_ context.Context, j job) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.publishErr != nil {
			return h.publishErr
		}
		h.published = append(h.published, j)
		return nil
	}

	t.Cleanup(func() {
		acquire, republish = prevAcquire, prevRepublish
		leaseMargin = prevMargin
	})
	return h, tick
}

func (h *harness) seen() ([]lockCall, []job) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]lockCall(nil), h.calls...), append([]job(nil), h.published...)
}

// --- the happy path -----------------------------------------------------

func TestASyncTakesTheAccountLockAndGivesItBack(t *testing.T) {
	h, _ := newHarness(t, 10*time.Millisecond)

	if err := syncAccount(context.Background(), job{Account: "acct-1"}); err != nil {
		t.Fatalf("syncAccount: %v", err)
	}

	calls, _ := h.seen()
	if len(calls) != 1 {
		t.Fatalf("acquired %d times, want 1: %+v", len(calls), calls)
	}
	// Namespaced per account, or one slow account would block every other.
	if calls[0].name != "account:acct-1" {
		t.Errorf("locked %q, want the account's own name", calls[0].name)
	}
	// The lease has to outlast the work, or it expires under a live holder.
	if calls[0].ttl <= 10*time.Millisecond {
		t.Errorf("ttl = %v, want more than the work itself", calls[0].ttl)
	}

	if _, released := h.lease.counts(); released != 1 {
		t.Errorf("released %d times, want 1: a lock held to its ttl blocks the "+
			"account for the full lease rather than the seconds it needed", released)
	}
}

// The release is deferred, so it must happen on the way out of a failure too.
func TestTheLockIsGivenBackWhenTheSyncFails(t *testing.T) {
	h, _ := newHarness(t, 300*time.Millisecond)
	h.lease.limited, h.lease.holds = true, 1 // the second renewal reports it gone

	if err := syncAccount(context.Background(), job{Account: "acct-1"}); !errors.Is(err, errLostLock) {
		t.Fatalf("err = %v, want errLostLock", err)
	}
	if _, released := h.lease.counts(); released != 1 {
		t.Errorf("released %d times after a failed sync, want 1", released)
	}
}

// --- renewal: the double-write guard ------------------------------------

func TestTheLeaseIsRenewedWhileTheWorkRuns(t *testing.T) {
	const work = 300 * time.Millisecond
	h, tick := newHarness(t, work)

	if err := syncAccount(context.Background(), job{Account: "acct-1"}); err != nil {
		t.Fatalf("syncAccount: %v", err)
	}

	// The work spans about three renewal intervals, so at least two renewals
	// must have happened. Stated against tick rather than a bare number, or
	// this test silently stops meaning anything when the timings change.
	want := int(work/tick) - 1
	renewals, _ := h.lease.counts()
	if renewals < want {
		t.Errorf("renewed %d times in %v with a %v interval, want at least %d; "+
			"without renewal the lock expires while the work is still using it",
			renewals, work, tick, want)
	}
}

// The reason renewal exists. A renewal that reports the lock gone means another
// replica may already be on this account, so the work must stop — finishing is
// what would write it twice.
func TestLosingTheLockMidSyncStopsRatherThanFinishing(t *testing.T) {
	tests := []struct {
		name    string
		limited bool
		err     error
	}{
		{name: "the lease was taken by someone else", limited: true},
		{name: "the renewal call itself failed", err: errors.New("host unreachable")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const work = time.Second // far longer than a lost lock should allow
			h, tick := newHarness(t, work)
			h.lease.limited, h.lease.renewErr = tc.limited, tc.err

			start := time.Now()
			err := syncAccount(context.Background(), job{Account: "acct-1"})
			if !errors.Is(err, errLostLock) {
				t.Fatalf("err = %v, want errLostLock", err)
			}
			// It must return at the first renewal, which is a third of the way
			// in, rather than running the work to completion. Half the work is a
			// generous ceiling that still fails loudly if it ran on.
			if elapsed := time.Since(start); elapsed > work/2 {
				t.Errorf("took %v of %v work (renewal interval %v): the work ran "+
					"on against a lock this replica no longer held", elapsed, work, tick)
			}
		})
	}
}

// --- contention ---------------------------------------------------------

// Being busy is not a failure. The queue counts an attempt when a message is
// claimed rather than when it fails, so returning an error here would burn the
// message's whole retry budget on contention and land it in the dead-letter
// table having never actually failed.
func TestABusyAccountIsPutBackRatherThanFailed(t *testing.T) {
	h, _ := newHarness(t, 10*time.Millisecond)
	h.granted = []bool{false} // busy on the first attempt

	if err := syncAccount(context.Background(), job{Account: "acct-1"}); err != nil {
		t.Fatalf("err = %v; contention must not be reported as a failure", err)
	}

	_, published := h.seen()
	if len(published) != 1 {
		t.Fatalf("republished %d times, want 1", len(published))
	}
	// The counter travels in the payload because the queue's own attempt count
	// cannot tell contention from failure.
	if published[0].Requeues != 1 {
		t.Errorf("requeues = %d, want 1", published[0].Requeues)
	}
	if _, released := h.lease.counts(); released != 0 {
		t.Error("a lock was released that was never taken")
	}
}

// When the backlog is full the republish is refused, and adding to a queue that
// is already over its limit would be the wrong move anyway — the message is
// already in it. Waiting for the lock costs neither a retry nor a slot.
func TestAFullQueueMeansWaitingForTheLockInstead(t *testing.T) {
	h, _ := newHarness(t, 10*time.Millisecond)
	h.publishErr = errors.New("backlog limit reached")
	h.granted = []bool{false, true} // busy first, then the wait succeeds

	if err := syncAccount(context.Background(), job{Account: "acct-1"}); err != nil {
		t.Fatalf("syncAccount: %v", err)
	}

	calls, published := h.seen()
	if len(published) != 0 {
		t.Errorf("republished %d messages into a full queue", len(published))
	}
	if len(calls) != 2 {
		t.Fatalf("acquired %d times, want a second attempt that waits: %+v", len(calls), calls)
	}
	if calls[1].wait != busyHoldWait {
		t.Errorf("second attempt waited %v, want busyHoldWait", calls[1].wait)
	}
	if calls[0].wait == calls[1].wait {
		t.Error("the fallback used the same wait as the first attempt, so it is " +
			"not the blocking backpressure it is meant to be")
	}
}

// An account that stays locked through every requeue is not busy, it is stuck,
// and the message should fail visibly rather than shuffle forever.
func TestAnAccountStuckAcrossEveryRequeueFailsVisibly(t *testing.T) {
	h, _ := newHarness(t, 10*time.Millisecond)
	h.granted = []bool{false, false}

	err := syncAccount(context.Background(), job{Account: "acct-1", Requeues: maxRequeues})
	if !errors.Is(err, errStuck) {
		t.Fatalf("err = %v, want errStuck", err)
	}

	_, published := h.seen()
	if len(published) != 0 {
		t.Errorf("a job at the requeue limit was put back %d more times", len(published))
	}
}
