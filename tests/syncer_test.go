package tests

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
)

// The syncer example, running for real.
//
// It exists to demonstrate the lock, and an example that compiles but does not
// actually exclude anything would teach the pattern while disproving it. So
// this runs the shipped binary, two replicas over one set of host
// capabilities, exactly as Core does.
//
// The observable is time. Two jobs for the same account must serialise, so
// they take about twice the work; two jobs for different accounts must not, so
// they take about one. If the lock did nothing, both cases would come out the
// same and fast — which is why both are measured rather than just the first.

// syncerWork must match work_seconds below: the assertion is a ratio between
// two elapsed times, and it is only meaningful if the work is the same in both.
const syncerWork = time.Second

func syncerReplicas(t *testing.T, handle *sql.DB, n int) {
	t.Helper()

	root := t.TempDir()
	installExampleAs(t, root, "syncer", "syncer", "../extension-example/syncer")
	binary := root + "/syncer/bin/syncer"

	cfg := map[string]string{
		"lock_wait_seconds": "30",
		"work_seconds":      "1",
	}
	// One set of capabilities for every replica, which is the part that makes
	// a cross-process lock a lock. launchReplica builds a fresh Deps per
	// instance and would show both replicas holding it.
	deps := hostsvc.Deps{
		Queue: hostsvc.NewPGQueue(db.NewQueue(handle)),
		Locks: hostsvc.NewMemoryLocks(),
		// So the example's own sdk.Log lines are visible: without Obs the host
		// refuses them and a plugin that is quietly doing nothing looks exactly
		// like one that is working.
		Obs: hostsvc.NewLogObservability("info"),
		Config: hostsvc.ConfigFunc(func(context.Context, string) (map[string]string, error) {
			return cfg, nil
		}),
	}

	for i := range n {
		inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
			Key:                "syncer",
			InstanceID:         "syncer-" + itoa(i),
			Version:            "1.0.0",
			BinaryPath:         binary,
			Checksum:           checksum(t, binary),
			HostImpl:           hostsvc.New("syncer", []string{"lock", "queue"}, deps),
			GrantedPermissions: []string{"lock", "queue"},
			// Both, for the reason redact_test records: OnConfigChanged is fed
			// from LaunchSpec.Config and GetConfig from Deps.Config, and a
			// plugin configured through only one of them behaves as if it were
			// not configured at all.
			Config:  cfg,
			Env:     []string{"PATH=/usr/bin:/bin"},
			Stderr:  os.Stderr,
			DevMode: true,
		})
		if err != nil {
			t.Fatalf("launching replica %d: %v", i, err)
		}
		t.Cleanup(inst.Kill)
	}
}

// publishAndWait queues one job per account and returns how long the whole set
// took to drain.
func publishAndWait(t *testing.T, q *db.Queue, accounts []string) time.Duration {
	t.Helper()

	ctx := context.Background()
	start := time.Now()
	for _, a := range accounts {
		if _, _, err := q.Enqueue(ctx, "syncer", "accounts",
			[]byte(`{"account":"`+a+`"}`), db.EnqueueOptions{}); err != nil {
			t.Fatalf("enqueue %s: %v", a, err)
		}
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		stats, err := q.Stats(ctx, "syncer", "accounts")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if stats.Pending == 0 && stats.Processing == 0 {
			return time.Since(start)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the queue never drained; the example's consumer is not running")
	return 0
}

func syncerQueue(t *testing.T) *db.Queue {
	t.Helper()

	handle := requireDB(t)
	if _, err := handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'syncer'`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	t.Cleanup(func() {
		_, _ = handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'syncer'`)
	})
	syncerReplicas(t, handle, 2)
	return db.NewQueue(handle)
}

// prefetch bounds how much work a consumer holds unacknowledged, not just how
// many rows one claim returns.
//
// It did not. The consume loop claimed, streamed and looped without waiting
// for an acknowledgement, so a single consumer took the whole backlog within
// milliseconds — which starved its replicas and, worse, started the visibility
// clock on messages it had not begun, so a long enough queue redelivered work
// nobody had touched.
func TestPrefetchBoundsUnacknowledgedMessages(t *testing.T) {
	handle := requireDB(t)
	if _, err := handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'syncer'`); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	t.Cleanup(func() { _, _ = handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'syncer'`) })
	syncerReplicas(t, handle, 1)
	q := db.NewQueue(handle)
	ctx := context.Background()

	for _, a := range []string{"a1", "a2", "a3", "a4"} {
		if _, _, err := q.Enqueue(ctx, "syncer", "accounts",
			[]byte(`{"account":"`+a+`"}`), db.EnqueueOptions{}); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}

	var maxProcessing int64
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		st, err := q.Stats(ctx, "syncer", "accounts")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if st.Processing > maxProcessing {
			maxProcessing = st.Processing
		}
		if st.Pending == 0 && st.Processing == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Logf("one replica, prefetch 1, four messages: at most %d claimed at once",
		maxProcessing)
	if maxProcessing > 1 {
		t.Errorf("%d messages were claimed at once against a prefetch of 1; a claimed "+
			"message is out of every other consumer's reach and its visibility clock "+
			"is already running, so claiming ahead of the work is not a buffer, it is "+
			"a queue nobody else can see into", maxProcessing)
	}
}

// A second replica takes half the work.
//
// This is what `replicas` is for in a queue-consuming plugin, and it did not
// happen: one replica absorbed everything and the other sat idle, so the
// setting bought nothing. Measured against the work rather than as a ratio,
// because both runs carry the consumer's poll interval.
func TestASecondReplicaSharesTheQueue(t *testing.T) {
	took := map[int]time.Duration{}
	for _, n := range []int{1, 2} {
		handle := requireDB(t)
		if _, err := handle.Exec(`DELETE FROM plugin_queue WHERE owner_key = 'syncer'`); err != nil {
			t.Fatalf("clearing: %v", err)
		}
		syncerReplicas(t, handle, n)
		q := db.NewQueue(handle)
		took[n] = publishAndWait(t, q, []string{"acct-a", "acct-b"})
		t.Logf("%d replica(s), two different accounts: %s", n, took[n].Round(10*time.Millisecond))
	}

	if took[1] < 2*syncerWork {
		t.Fatalf("one replica finished two %s jobs in %s, so it ran them at once and "+
			"there is no serial baseline to improve on", syncerWork, took[1])
	}
	if took[1]-took[2] < syncerWork/2 {
		t.Errorf("two replicas took %s against %s for one — no better, so the second "+
			"replica is not taking any of the work and `replicas` buys nothing for a "+
			"queue consumer", took[2], took[1])
	}
}

func TestSyncerSerialisesOneAccountAndParallelisesTwo(t *testing.T) {
	q := syncerQueue(t)

	// Different accounts: nothing to exclude, so the two replicas overlap.
	apart := publishAndWait(t, q, []string{"acct-a", "acct-b"})

	// The same account twice: the second waits for the first.
	together := publishAndWait(t, q, []string{"acct-same", "acct-same"})

	t.Logf("two accounts in parallel: %s; one account twice: %s (work is ~%s each)",
		apart.Round(10*time.Millisecond), together.Round(10*time.Millisecond), syncerWork)

	// Against the work itself, not against each other. Both measurements carry
	// the consumer's poll interval — a replica picks up its message up to half
	// a second after it is queued — so the parallel case lands near 1.5s
	// rather than 1s, and a ratio between the two understates a difference
	// that is really there. The claim is simpler than a ratio anyway: two
	// one-second jobs that cannot overlap take at least two seconds, and two
	// that can take less.
	if together < 2*syncerWork {
		t.Errorf("two jobs for one account took %s, under the %s they need if they "+
			"cannot overlap; both replicas were inside the critical section at once, "+
			"which is the failure the lock exists to prevent and the one that leaves "+
			"no trace", together, 2*syncerWork)
	}
	// The difference rather than an absolute, because both runs carry the same
	// poll overhead and a loaded machine inflates both together. An absolute
	// bound on the parallel case has half a second of headroom against a
	// half-second poll interval, which is the shape of a test that is green
	// alone and red under the full suite.
	if together-apart < syncerWork/2 {
		t.Errorf("two different accounts took %s against %s for the same account "+
			"twice — too close to say they overlapped. The lock name is per account, "+
			"so unrelated work must not queue behind it", apart, together)
	}
}
