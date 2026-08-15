package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
)

// What happens to work the queue gives up on.
//
// The durable queue exists so that work survives a crash, a restart and a
// plugin that is temporarily broken. It retries a failing message up to
// max_attempts and then parks it as dead — which is the right policy, because
// retrying a poison message forever is worse.
//
// The question these tests ask is what anyone is told when that happens. A
// message parked as dead is work that was accepted and will never be done, and
// the person who has to know is not the plugin author — whose handler already
// returned an error and moved on — but whoever operates the system.

// deadQueue builds a queue over the test database with a short retry budget so
// exhaustion takes seconds rather than minutes.
func deadQueue(t *testing.T, _ string) (*db.Queue, *hostsvc.PGQueue, string) {
	t.Helper()

	handle := requireDB(t)
	key := "deadtest"

	raw := db.NewQueue(handle)
	// Everything this test wrote before, so a re-run does not measure the last
	// one's leftovers.
	if _, err := handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = $1`, key); err != nil {
		t.Fatalf("clearing the queue: %v", err)
	}
	t.Cleanup(func() {
		_, _ = handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = $1`, key)
	})

	return raw, hostsvc.NewPGQueue(raw), key
}

// exhaust publishes one message and nacks it until the queue gives up,
// returning how many deliveries it took.
func exhaust(t *testing.T, q *db.Queue, key, topic string, maxAttempts int) int {
	t.Helper()
	ctx := context.Background()

	if _, _, err := q.Enqueue(ctx, key, topic, []byte(`{"work":true}`),
		db.EnqueueOptions{MaxAttempts: maxAttempts}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	deliveries := 0
	for range maxAttempts + 2 {
		msgs, err := q.Claim(ctx, key, topic, 1, 30*time.Second)
		if err != nil {
			t.Fatalf("consume: %v", err)
		}
		if len(msgs) == 0 {
			break
		}
		deliveries++
		if err := q.Nack(ctx, key, msgs[0].ID, "handler failed", 0); err != nil {
			t.Fatalf("nack: %v", err)
		}
	}
	return deliveries
}

// A message whose handler keeps failing is retried and then parked, rather
// than retried forever. This is the policy working.
func TestQueueParksAPoisonMessage(t *testing.T) {
	raw, _, key := deadQueue(t, "poison")

	deliveries := exhaust(t, raw, key, "poison", 3)
	if deliveries != 3 {
		t.Errorf("delivered %d times against a budget of 3", deliveries)
	}

	stats, err := raw.Stats(context.Background(), key, "poison")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Dead != 1 {
		t.Errorf("dead = %d, want 1; stats = %+v", stats.Dead, stats)
	}
	if stats.Pending != 0 {
		t.Errorf("pending = %d; an exhausted message should not still be waiting", stats.Pending)
	}
}

// The gap: nothing that watches the system reports it.
//
// PendingDepth — the number the console shows and the ceiling is enforced
// against — counts pending and processing. A dead message is neither, so work
// the queue has given up on makes the depth go down, not up. A plugin whose
// handler is permanently broken drains its whole backlog into 'dead' and reads
// as a plugin that has caught up.
func TestDeadMessagesDoNotShowInTheDepth(t *testing.T) {
	raw, _, key := deadQueue(t, "invisible")
	ctx := context.Background()

	for i := range 3 {
		exhaust(t, raw, key, fmt.Sprintf("invisible-%d", i), 2)
	}

	depths, err := raw.PendingDepth(ctx)
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	t.Logf("pending depth after three messages were given up on: %d", depths[key])

	if depths[key] != 0 {
		t.Errorf("depth = %d, want 0; this test is asserting the opposite of what it measured",
			depths[key])
	}

	// And the number Core surfaces, which is built from the same measurement.
	dead, err := raw.DeadDepth(ctx)
	if err != nil {
		t.Fatalf("dead depth: %v", err)
	}
	if dead[key] != 3 {
		t.Errorf("dead depth = %d, want 3; work that was accepted and will never be done "+
			"has to be visible somewhere", dead[key])
	}
}

// Core says so when it gives up. The plugin's handler already returned an
// error and moved on; the person who needs to know is whoever operates the
// system, and until this there was no moment at which they were told.
func TestCoreReportsWhenItGivesUpOnAMessage(t *testing.T) {
	raw, _, key := deadQueue(t, "reported")

	var (
		gotTopic  string
		gotReason string
		calls     int
	)
	raw.OnDeadLetter = func(ownerKey, topic string, id int64, attempts int, reason string) {
		calls++
		gotTopic, gotReason = topic, reason
	}

	exhaust(t, raw, key, "reported", 2)

	if calls != 1 {
		t.Fatalf("the dead-letter hook fired %d times, want 1", calls)
	}
	if gotTopic != "reported" {
		t.Errorf("topic = %q", gotTopic)
	}
	if gotReason == "" {
		t.Error("no reason was reported; the last error is the only clue why it kept failing")
	}
	t.Logf("reported: topic=%s reason=%s", gotTopic, gotReason)
}

// The other direction: a nack that still has attempts left is an ordinary
// retry and must not be reported as giving up. A hook that fired on every
// failure would be noise, and noise gets filtered out — including the one
// message that mattered.
func TestOrdinaryRetriesAreNotReportedAsGivingUp(t *testing.T) {
	raw, _, key := deadQueue(t, "retrying")
	ctx := context.Background()

	var calls int
	raw.OnDeadLetter = func(string, string, int64, int, string) { calls++ }

	if _, _, err := raw.Enqueue(ctx, key, "retrying", []byte(`{}`),
		db.EnqueueOptions{MaxAttempts: 5}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Two failures out of a budget of five.
	for range 2 {
		msgs, err := raw.Claim(ctx, key, "retrying", 1, 30*time.Second)
		if err != nil || len(msgs) == 0 {
			t.Fatalf("consume: %v (%d msgs)", err, len(msgs))
		}
		if err := raw.Nack(ctx, key, msgs[0].ID, "temporary", 0); err != nil {
			t.Fatalf("nack: %v", err)
		}
	}

	if calls != 0 {
		t.Errorf("the hook fired %d time(s) for a message that is still being retried", calls)
	}
}
