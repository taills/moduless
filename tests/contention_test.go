package tests

import (
	"context"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
)

// Contention must not look like failure.
//
// The syncer example returns an error when another replica holds the account,
// so the message goes back on the queue. That is the obvious thing to write
// and it quietly spends a retry: attempts increments on every claim, and a
// message that has been claimed max_attempts times (5 by default) is moved to
// the dead-letter table.
//
// So work that never failed can be discarded for having been busy — and the
// dead-letter entry says "account is being synced by another replica", which
// reads like a diagnosis rather than the accounting artefact it is.
//
// The conditions are ordinary: work that takes longer than the lock wait. Here
// the wait is zero, which is the worst case and also what an author picks when
// they want a contender to give up rather than block a consumer.

func contendedSyncer(t *testing.T, lockWait, work string, replicas int) *db.Queue {
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

	cfg := map[string]string{"lock_wait_seconds": lockWait, "work_seconds": work}
	deps := hostsvc.Deps{
		Queue: hostsvc.NewPGQueue(db.NewQueue(handle)),
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
			DevMode:            true,
		})
		if err != nil {
			t.Fatalf("launching replica %d: %v", i, err)
		}
		t.Cleanup(inst.Kill)
	}
	return db.NewQueue(handle)
}

func TestContentionDoesNotDeadLetterGoodWork(t *testing.T) {
	// Work an order of magnitude longer than the wait, so a contender always
	// gives up rather than occasionally winning the race.
	q := contendedSyncer(t, "0", "1", 2)
	ctx := context.Background()

	const messages = 6
	for range messages {
		if _, _, err := q.Enqueue(ctx, "syncer", "accounts",
			[]byte(`{"account":"acct-hot"}`), db.EnqueueOptions{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	// Long enough for every message to be delivered several times over if
	// contention keeps bouncing them.
	deadline := time.Now().Add(45 * time.Second)
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

	t.Logf("%d messages for one account, two replicas, no lock wait: %+v", messages, last)

	if last.Pending != 0 || last.Processing != 0 {
		t.Fatalf("the queue did not drain in 45s (%+v); the messages are bouncing "+
			"between replicas rather than being processed", last)
	}
	if last.Dead > 0 {
		t.Errorf("%d of %d messages were dead-lettered. None of them failed — they "+
			"were claimed by a replica that could not get the account lock, and a "+
			"claim spends an attempt whether or not any work happened. Losing real "+
			"work to a retry budget it never meant to use is worse than waiting",
			last.Dead, messages)
	}
}
