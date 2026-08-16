package tests

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
)

// What a dedup key actually promises.
//
// The notes example says a nightly summary keyed on job.Scheduled means "a
// re-run of the same scheduled occurrence does not produce a second summary".
// The queue's own unit test says the opposite in passing: it treats a key that
// stays blocked after its message completes as a failure. Both cannot be the
// whole truth, and the difference decides whether a plugin author can rely on
// the key for correctness or only for collapsing a burst.
//
// Also untested: what two simultaneous publishes with the same key do. The
// column has a unique partial index, so one of them loses — but losing at the
// database is not the same as the caller being told cleanly.

func dedupQueue(t *testing.T) (*db.Queue, string) {
	t.Helper()

	handle := requireDB(t)
	const key = "deduptest"
	if _, err := handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = $1`, key); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = $1`, key)
	})
	return db.NewQueue(handle), key
}

// While the first message is still outstanding, a second publish with the same
// key is collapsed into it.
func TestDedupCollapsesWhileTheFirstIsPending(t *testing.T) {
	q, owner := dedupQueue(t)
	ctx := context.Background()

	_, dup1, err := q.Enqueue(ctx, owner, "sums", []byte(`{"n":1}`),
		db.EnqueueOptions{DedupKey: "nightly-2026-08-16"})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if dup1 {
		t.Fatal("the first publish reported itself as a duplicate")
	}

	_, dup2, err := q.Enqueue(ctx, owner, "sums", []byte(`{"n":2}`),
		db.EnqueueOptions{DedupKey: "nightly-2026-08-16"})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if !dup2 {
		t.Error("a second publish with the same key was accepted while the first was " +
			"still waiting; the key collapses nothing")
	}

	stats, err := q.Stats(ctx, owner, "sums")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 1 {
		t.Errorf("%d messages pending, want 1: %+v", stats.Pending, stats)
	}
}

// Once the first has been processed, the same key is free again.
//
// This is the half the notes example did not say, and it changes what the key
// can be relied on for: it collapses a burst, it does not make an operation
// once-ever. A job re-run tomorrow with the same scheduled timestamp — because
// somebody replayed it, or because the row was purged — produces a second
// summary.
func TestDedupIsReleasedOnceTheMessageIsDone(t *testing.T) {
	q, owner := dedupQueue(t)
	ctx := context.Background()

	const key = "nightly-2026-08-16"
	if _, _, err := q.Enqueue(ctx, owner, "sums", []byte(`{"n":1}`),
		db.EnqueueOptions{DedupKey: key}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := q.Claim(ctx, owner, "sums", 1, 30*time.Second)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("claim: %v (%d messages)", err, len(msgs))
	}
	if err := q.Ack(ctx, owner, msgs[0].ID); err != nil {
		t.Fatalf("ack: %v", err)
	}

	_, dup, err := q.Enqueue(ctx, owner, "sums", []byte(`{"n":2}`),
		db.EnqueueOptions{DedupKey: key})
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if dup {
		t.Error("the same key was still blocked after its message was acknowledged; " +
			"then a key is permanent and a plugin can never re-run anything")
	}
	t.Log("a dedup key is released once its message completes: it collapses a burst, " +
		"it does not make an operation once-ever")
}

// Simultaneous publishes with one key: exactly one message, and every caller
// is told cleanly rather than one of them getting a constraint violation.
func TestDedupUnderConcurrentPublishes(t *testing.T) {
	q, owner := dedupQueue(t)
	ctx := context.Background()

	const (
		attempts = 20
		key      = "burst-2026-08-16"
	)
	var (
		accepted atomic.Int64
		duped    atomic.Int64
		failed   atomic.Int64
		wg       sync.WaitGroup
	)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, dup, err := q.Enqueue(ctx, owner, "sums", []byte(`{}`),
				db.EnqueueOptions{DedupKey: key})
			switch {
			case err != nil:
				failed.Add(1)
			case dup:
				duped.Add(1)
			default:
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()

	t.Logf("%d accepted, %d reported duplicate, %d failed",
		accepted.Load(), duped.Load(), failed.Load())

	stats, err := q.Stats(ctx, owner, "sums")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Pending != 1 {
		t.Errorf("%d messages queued from %d simultaneous publishes with one key; "+
			"the unique index is the only thing standing between a burst and a "+
			"duplicated job", stats.Pending, attempts)
	}
	if failed.Load() > 0 {
		t.Errorf("%d publisher(s) got an error rather than being told it was a duplicate; "+
			"a caller cannot tell a lost write from a collapsed one", failed.Load())
	}
	if accepted.Load() != 1 {
		t.Errorf("%d publishers were told their message was accepted; exactly one was",
			accepted.Load())
	}
}

// Delayed delivery: a message with a delay is not handed out before its time.
func TestDelayedMessageIsNotDeliveredEarly(t *testing.T) {
	q, owner := dedupQueue(t)
	ctx := context.Background()

	const delay = 600 * time.Millisecond
	if _, _, err := q.Enqueue(ctx, owner, "later", []byte(`{}`),
		db.EnqueueOptions{Delay: delay}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Immediately: nothing.
	msgs, err := q.Claim(ctx, owner, "later", 1, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("a message with a %s delay was delivered immediately", delay)
	}

	// After its time: there.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err = q.Claim(ctx, owner, "later", 1, 30*time.Second)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(msgs) == 1 {
			t.Logf("delivered after %s", time.Since(deadline.Add(-5*time.Second)).Round(time.Millisecond))
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("a delayed message never arrived; a delay is not a discard")
}
