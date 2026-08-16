package tests

import (
	"context"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
)

// The backlog limit, and what it does to work that is being put back.
//
// Two things meet here. Core refuses an enqueue once a plugin's backlog
// reaches its limit — the queue is one shared table, so one plugin's pile-up
// is everyone's. And the syncer example, having learned not to spend a retry
// on contention, puts contended work back by publishing it afresh.
//
// Publishing afresh is an enqueue. So a full queue turns the safe path back
// into the unsafe one: the republish is refused, the handler falls back to
// failing, and the message resumes burning the attempts that dead-letter it.
//
// The limit is also read from a cached depth that maintenance samples on a
// timer, so how fast the burst arrives decides whether the limit is noticed at
// all. Both halves are measured here.

func depthLimitedSyncer(t *testing.T, maxDepth int64, sample time.Duration, replicas int) (*db.Queue, *hostsvc.PGQueue) {
	t.Helper()

	handle := requireDB(t)
	if _, err := handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'syncer'`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'syncer'`)
	})

	root := t.TempDir()
	installExampleAs(t, root, "syncer", "syncer", "../extension-example/syncer")
	binary := root + "/syncer/bin/syncer"

	pq := hostsvc.NewPGQueue(db.NewQueue(handle))
	pq.MaxDepth = maxDepth
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	pq.StartMaintenance(ctx, sample, time.Hour)

	cfg := map[string]string{"lock_wait_seconds": "0", "work_seconds": "1"}
	deps := hostsvc.Deps{
		Queue: pq,
		Locks: hostsvc.NewMemoryLocks(),
		Config: hostsvc.ConfigFunc(func(context.Context, string) (map[string]string, error) {
			return cfg, nil
		}),
		Obs: hostsvc.NewLogObservability("info"),
	}

	for i := range replicas {
		inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
			Key:                "syncer",
			InstanceID:         "syncer-" + itoa(i),
			Version:            "1.0.0",
			BinaryPath:         binary,
			Checksum:           checksum(t, binary),
			HostImpl:           hostsvc.New("syncer", []string{"lock", "queue"}, deps),
			GrantedPermissions: []string{"lock", "queue"},
			Config:             cfg,
			Env:                []string{"PATH=/usr/bin:/bin"},
		})
		if err != nil {
			t.Fatalf("launching replica %d: %v", i, err)
		}
		t.Cleanup(inst.Kill)
	}
	return db.NewQueue(handle), pq
}

// With the limit visible, does contended work still survive?
func TestAFullQueueDoesNotDeadLetterContendedWork(t *testing.T) {
	// Sampled often, so the limit is genuinely in force rather than lagging
	// behind the burst.
	q, _ := depthLimitedSyncer(t, 3, 50*time.Millisecond, 2)
	ctx := context.Background()

	const messages = 6
	for range messages {
		if _, _, err := q.Enqueue(ctx, "syncer", "accounts",
			[]byte(`{"account":"acct-hot"}`), db.EnqueueOptions{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	deadline := time.Now().Add(60 * time.Second)
	var last db.QueueStats
	for time.Now().Before(deadline) {
		st, err := q.Stats(ctx, "syncer", "accounts")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		last = st
		if st.Pending == 0 && st.Processing == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Logf("%d messages for one contended account, backlog limit 3: %+v", messages, last)

	if last.Pending != 0 || last.Processing != 0 {
		t.Fatalf("the queue did not drain (%+v)", last)
	}
	if last.Dead > 0 {
		t.Errorf("%d of %d messages were dead-lettered under a backlog limit. The "+
			"limit is there to stop a pile-up, not to discard work that is only "+
			"waiting its turn: putting the message back has to stay possible when "+
			"the queue is full, or the safe path silently becomes the unsafe one "+
			"exactly when the system is under pressure", last.Dead, messages)
	}
}

// Does the limit fire at all against a burst faster than its sampling?
//
// checkDepth reads a depth that maintenance refreshes on a timer — 30 seconds
// in Core. A burst that arrives inside one interval is measured against a
// backlog that was empty when it was sampled, so the limit does not apply to
// the case it most exists for.
func TestTheBacklogLimitLagsABurst(t *testing.T) {
	q, _ := depthLimitedSyncer(t, 3, time.Minute, 0)
	ctx := context.Background()

	accepted := 0
	for range 20 {
		if _, _, err := q.Enqueue(ctx, "syncer", "accounts",
			[]byte(`{"account":"burst"}`), db.EnqueueOptions{}); err == nil {
			accepted++
		}
	}
	t.Logf("backlog limit 3, sampled once a minute: %d of 20 enqueues accepted", accepted)

	// Recorded rather than asserted as a fault: sampling is what makes the
	// check cheap, and Core is explicit that the limit is a backstop against a
	// sustained pile-up rather than a rate limiter. What matters is that the
	// number is written down, because "the queue is capped at N" reads like a
	// guarantee and is not one.
	if accepted <= 3 {
		t.Logf("the limit held against the burst; it is tighter than the sampling " +
			"interval suggested")
	}
}
