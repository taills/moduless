package db

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

func queueTestSetup(t *testing.T) (*Queue, func()) {
	t.Helper()

	connStr := os.Getenv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping queue integration test")
	}
	// InitDB rather than the plain testDB helper: the queue lives in a
	// migration, and migrations are what testDB deliberately does not run.
	conn, err := InitDB(connStr)
	if err != nil {
		t.Skipf("cannot reach or migrate the test database: %v", err)
	}
	if _, err := conn.Exec("TRUNCATE plugin_queue"); err != nil {
		t.Fatalf("truncate plugin_queue: %v", err)
	}

	q := NewQueue(conn)
	return q, func() { conn.Close() }
}

func TestQueueEnqueueAndClaim(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	for i := range 3 {
		if _, _, err := q.Enqueue(ctx, "p", "jobs", fmt.Appendf(nil, "job-%d", i), EnqueueOptions{}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	msgs, err := q.Claim(ctx, "p", "jobs", 2, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("claimed %d messages, want 2", len(msgs))
	}
	if msgs[0].Attempt != 1 {
		t.Errorf("attempt = %d, want 1", msgs[0].Attempt)
	}

	// Claimed messages are invisible to the next consumer.
	again, err := q.Claim(ctx, "p", "jobs", 5, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(again) != 1 {
		t.Errorf("second claim got %d messages, want the 1 remaining", len(again))
	}
}

// Topics are namespaced per plugin. Without that, one plugin could drain
// another's queue just by guessing a topic name.
func TestQueueIsIsolatedBetweenPlugins(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	if _, _, err := q.Enqueue(ctx, "plugin-a", "jobs", []byte("a's work"), EnqueueOptions{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stolen, err := q.Claim(ctx, "plugin-b", "jobs", 10, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(stolen) != 0 {
		t.Errorf("plugin-b claimed %d of plugin-a's messages", len(stolen))
	}

	mine, err := q.Claim(ctx, "plugin-a", "jobs", 10, time.Minute)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if len(mine) != 1 {
		t.Errorf("plugin-a claimed %d of its own messages, want 1", len(mine))
	}
}

func TestQueueDeduplication(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	_, dup, err := q.Enqueue(ctx, "p", "jobs", []byte("once"), EnqueueOptions{DedupKey: "order-42"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if dup {
		t.Error("the first enqueue was reported as a duplicate")
	}

	_, dup, err = q.Enqueue(ctx, "p", "jobs", []byte("again"), EnqueueOptions{DedupKey: "order-42"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if !dup {
		t.Error("the second enqueue with the same key was not deduplicated")
	}

	msgs, _ := q.Claim(ctx, "p", "jobs", 10, time.Minute)
	if len(msgs) != 1 {
		t.Fatalf("queue holds %d messages, want 1", len(msgs))
	}

	// Once the work is finished the same key may be reused for the next one.
	if err := q.Ack(ctx, "p", msgs[0].ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	_, dup, err = q.Enqueue(ctx, "p", "jobs", []byte("next time"), EnqueueOptions{DedupKey: "order-42"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if dup {
		t.Error("a dedup key stayed blocked after its message completed")
	}
}

func TestQueueDelayedMessage(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	now := time.Now()
	q.SetClock(func() time.Time { return now })

	if _, _, err := q.Enqueue(ctx, "p", "jobs", []byte("later"), EnqueueOptions{Delay: time.Minute}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	if msgs, _ := q.Claim(ctx, "p", "jobs", 10, time.Minute); len(msgs) != 0 {
		t.Errorf("a delayed message was delivered early")
	}

	now = now.Add(2 * time.Minute)
	if msgs, _ := q.Claim(ctx, "p", "jobs", 10, time.Minute); len(msgs) != 1 {
		t.Errorf("a delayed message was not delivered after its delay")
	}
}

func TestQueueRetryAndDeadLetter(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	if _, _, err := q.Enqueue(ctx, "p", "jobs", []byte("flaky"), EnqueueOptions{MaxAttempts: 2}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First attempt fails and is retried.
	msgs, _ := q.Claim(ctx, "p", "jobs", 1, time.Minute)
	if len(msgs) != 1 {
		t.Fatal("nothing to claim")
	}
	if err := q.Nack(ctx, "p", msgs[0].ID, "boom", 0); err != nil {
		t.Fatalf("Nack: %v", err)
	}

	stats, _ := q.Stats(ctx, "p", "jobs")
	if stats.Pending != 1 {
		t.Fatalf("after one failure: %+v, want it pending again", stats)
	}

	// Second attempt exhausts max_attempts, so it goes to the dead letter
	// state rather than looping forever.
	msgs, _ = q.Claim(ctx, "p", "jobs", 1, time.Minute)
	if len(msgs) != 1 {
		t.Fatal("the retried message was not redelivered")
	}
	if err := q.Nack(ctx, "p", msgs[0].ID, "boom again", 0); err != nil {
		t.Fatalf("Nack: %v", err)
	}

	stats, _ = q.Stats(ctx, "p", "jobs")
	if stats.Dead != 1 || stats.Pending != 0 {
		t.Errorf("after exhausting attempts: %+v, want one dead and none pending", stats)
	}
	if msgs, _ := q.Claim(ctx, "p", "jobs", 10, time.Minute); len(msgs) != 0 {
		t.Error("a dead-lettered message was redelivered")
	}
}

// A consumer that dies mid-handler must not strand its message.
func TestQueueVisibilityTimeoutRedelivers(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	now := time.Now()
	q.SetClock(func() time.Time { return now })

	if _, _, err := q.Enqueue(ctx, "p", "jobs", []byte("work"), EnqueueOptions{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	msgs, _ := q.Claim(ctx, "p", "jobs", 1, 30*time.Second)
	if len(msgs) != 1 {
		t.Fatal("nothing claimed")
	}

	// Consumer vanishes without acking.
	now = now.Add(31 * time.Second)

	reaped, err := q.ReapExpired(ctx)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if reaped != 1 {
		t.Errorf("reaped %d messages, want 1", reaped)
	}

	redelivered, _ := q.Claim(ctx, "p", "jobs", 1, time.Minute)
	if len(redelivered) != 1 {
		t.Fatal("the abandoned message was not redelivered")
	}
	if redelivered[0].Attempt != 2 {
		t.Errorf("attempt = %d, want 2 on redelivery", redelivered[0].Attempt)
	}
}

// One plugin must not be able to acknowledge another's message by id.
func TestQueueAckIsScopedToOwner(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	if _, _, err := q.Enqueue(ctx, "plugin-a", "jobs", []byte("work"), EnqueueOptions{}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	msgs, _ := q.Claim(ctx, "plugin-a", "jobs", 1, time.Minute)
	if len(msgs) != 1 {
		t.Fatal("nothing claimed")
	}

	if err := q.Ack(ctx, "plugin-b", msgs[0].ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	stats, _ := q.Stats(ctx, "plugin-a", "jobs")
	if stats.Done != 0 {
		t.Error("another plugin acknowledged the message")
	}
	if stats.Processing != 1 {
		t.Errorf("stats = %+v, want the message still processing", stats)
	}
}

// SKIP LOCKED is what lets several consumers run at once; this asserts they
// never receive the same message twice.
func TestQueueConcurrentConsumersDoNotOverlap(t *testing.T) {
	q, done := queueTestSetup(t)
	defer done()
	ctx := context.Background()

	const total = 40
	for i := range total {
		if _, _, err := q.Enqueue(ctx, "p", "jobs", fmt.Appendf(nil, "job-%d", i), EnqueueOptions{}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	var (
		mu   sync.Mutex
		seen = map[int64]int{}
		wg   sync.WaitGroup
	)
	for range 4 {
		wg.Go(func() {
			for {
				msgs, err := q.Claim(ctx, "p", "jobs", 3, time.Minute)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if len(msgs) == 0 {
					return
				}
				mu.Lock()
				for _, m := range msgs {
					seen[m.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Errorf("consumers saw %d distinct messages, want %d", len(seen), total)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("message %d was delivered %d times", id, count)
		}
	}
}
