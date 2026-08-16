package tests

import (
	"context"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
)

// Dead letters have to be reachable.
//
// The console shows a count — "已放弃 4 条" — and that is the whole of it.
// There is no way to see which messages, why they were given up on, or to put
// one back. The data is all there in plugin_queue: the payload, the last
// error, how many attempts it took.
//
// This is not a hypothetical gap. Working out why four of six messages died in
// tests/contention_test.go meant querying PostgreSQL by hand, which is exactly
// what an operator would have to do and may not be able to.

func deadLetterQueue(t *testing.T) *db.Queue {
	t.Helper()

	handle := requireDB(t)
	if _, err := handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'dlq'`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'dlq'`)
	})
	return db.NewQueue(handle)
}

// kill drives a message all the way to dead through the ordinary path, so what
// is listed is what the queue really produces rather than a row written by the
// test.
func kill(t *testing.T, q *db.Queue, topic, payload, reason string) int64 {
	t.Helper()

	ctx := context.Background()
	id, _, err := q.Enqueue(ctx, "dlq", topic, []byte(payload),
		db.EnqueueOptions{MaxAttempts: 2})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for range 2 {
		msgs, err := q.Claim(ctx, "dlq", topic, 1, time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(msgs) == 0 {
			t.Fatalf("nothing to claim while driving %d to dead", id)
		}
		if err := q.Nack(ctx, "dlq", msgs[0].ID, reason, 0); err != nil {
			t.Fatalf("nack: %v", err)
		}
	}
	return id
}

func TestDeadLettersCanBeListed(t *testing.T) {
	q := deadLetterQueue(t)
	ctx := context.Background()

	id := kill(t, q, "sync", `{"account":"acct-1"}`, "upstream returned 500")
	kill(t, q, "sync", `{"account":"acct-2"}`, "malformed response")

	dead, err := q.Dead(ctx, "dlq", 10)
	if err != nil {
		t.Fatalf("Dead: %v", err)
	}
	if len(dead) != 2 {
		t.Fatalf("listed %d dead messages, want 2", len(dead))
	}

	var found *db.DeadMessage
	for i := range dead {
		if dead[i].ID == id {
			found = &dead[i]
		}
	}
	if found == nil {
		t.Fatalf("the message driven to dead is not in the list: %+v", dead)
	}

	// Each field earns its place: without the payload there is nothing to
	// judge, without the error there is nothing to diagnose, and without the
	// attempt count a message that failed once looks like one that failed
	// five times.
	if string(found.Payload) != `{"account":"acct-1"}` {
		t.Errorf("payload = %q; a dead letter with no payload cannot be judged or "+
			"replayed", found.Payload)
	}
	if found.LastError != "upstream returned 500" {
		t.Errorf("last_error = %q, want the reason the queue recorded", found.LastError)
	}
	if found.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", found.Attempts)
	}
	if found.Topic != "sync" {
		t.Errorf("topic = %q", found.Topic)
	}
}

// One plugin must not see another's.
func TestDeadLettersAreScopedToTheirPlugin(t *testing.T) {
	q := deadLetterQueue(t)
	ctx := context.Background()
	kill(t, q, "sync", `{"account":"acct-1"}`, "boom")

	dead, err := q.Dead(ctx, "somebody-else", 10)
	if err != nil {
		t.Fatalf("Dead: %v", err)
	}
	if len(dead) != 0 {
		t.Errorf("another plugin's dead letters were listed (%d); the payload is the "+
			"plugin's data and the queue is one shared table", len(dead))
	}
}

func TestADeadLetterCanBeRetried(t *testing.T) {
	q := deadLetterQueue(t)
	ctx := context.Background()

	id := kill(t, q, "sync", `{"account":"acct-1"}`, "upstream returned 500")

	if err := q.RetryDead(ctx, "dlq", id); err != nil {
		t.Fatalf("RetryDead: %v", err)
	}

	// Deliverable again...
	msgs, err := q.Claim(ctx, "dlq", "sync", 1, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("the retried message was not deliverable (%d claimed); a retry that "+
			"does not put it back in the pending pool is a no-op with a success "+
			"message", len(msgs))
	}
	if string(msgs[0].Payload) != `{"account":"acct-1"}` {
		t.Errorf("payload = %q; the work has to come back intact", msgs[0].Payload)
	}

	// ...and with room to actually run. A retry that returns the message with
	// its attempts already spent dies again on the first nack, which looks
	// like the retry silently doing nothing.
	if msgs[0].Attempt >= msgs[0].MaxAttempts {
		t.Errorf("attempt %d of %d after a retry; the budget was not reset, so this "+
			"message is dead again as soon as anything goes wrong",
			msgs[0].Attempt, msgs[0].MaxAttempts)
	}
}

func TestRetryingSomebodyElsesDeadLetterDoesNothing(t *testing.T) {
	q := deadLetterQueue(t)
	ctx := context.Background()
	id := kill(t, q, "sync", `{"account":"acct-1"}`, "boom")

	if err := q.RetryDead(ctx, "somebody-else", id); err == nil {
		t.Error("retrying another plugin's dead letter succeeded; the owner key is " +
			"the only thing separating two plugins' rows in one table")
	}

	dead, err := q.Dead(ctx, "dlq", 10)
	if err != nil {
		t.Fatalf("Dead: %v", err)
	}
	if len(dead) != 1 {
		t.Errorf("the message is no longer dead (%d listed) after somebody else "+
			"retried it", len(dead))
	}
}
