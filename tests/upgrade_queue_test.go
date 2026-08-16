package tests

import (
	"context"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
)

// What a deploy costs a message that is being worked on.
//
// Two headline features crossing: a plugin is upgraded while one of its queue
// consumers is halfway through a message. The message is claimed and
// unacknowledged, so at-least-once says it comes back — the question nobody
// had measured is *when*.
//
// If the old process is killed without acknowledging, the message is invisible
// until its visibility timeout lapses, which defaults to thirty seconds. Every
// deploy would then stall the work in flight for half a minute, and an
// operator watching the queue would see it stop rather than slow down.
//
// If instead the shutdown reaches the handler, the SDK nacks on the way out
// and the new version picks the message up immediately — one repeated unit of
// work, which is what at-least-once costs and no more.

func upgradeQueueStack(t *testing.T) (*db.Queue, hostsvc.Deps, string) {
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

	cfg := map[string]string{"lock_wait_seconds": "30", "work_seconds": "2"}

	// Maintenance deliberately far out of reach.
	//
	// Core runs it every 30 seconds and it is what returns messages whose
	// consumer vanished — so with it running, any result here would only show
	// that the reaper works. The question is whether an upgrade needs the
	// reaper at all, or whether the shutdown reaches the handler and the SDK
	// nacks on the way out. Ten minutes answers that: anything that comes back
	// promptly came back because it was nacked.
	//
	// The first version of this test simply did not start maintenance, which
	// is not the same thing — it looked like the queue had lost the message
	// forever, and the fault was the test diverging from main.go.
	pq := hostsvc.NewPGQueue(db.NewQueue(handle))
	maintCtx, stopMaint := context.WithCancel(context.Background())
	t.Cleanup(stopMaint)
	pq.StartMaintenance(maintCtx, 10*time.Minute, time.Hour)

	deps := hostsvc.Deps{
		Queue: pq,
		Locks: hostsvc.NewMemoryLocks(),
		Config: hostsvc.ConfigFunc(func(context.Context, string) (map[string]string, error) {
			return cfg, nil
		}),
		Obs: hostsvc.NewLogObservability("info"),
	}
	return db.NewQueue(handle), deps, root + "/syncer/bin/syncer"
}

func launchSyncer(t *testing.T, deps hostsvc.Deps, binary, instanceID, version string) *pluginhost.Instance {
	t.Helper()

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:                "syncer",
		InstanceID:         instanceID,
		Version:            version,
		BinaryPath:         binary,
		Checksum:           checksum(t, binary),
		HostImpl:           hostsvc.New("syncer", []string{"lock", "queue"}, deps),
		GrantedPermissions: []string{"lock", "queue"},
		Config:             map[string]string{"lock_wait_seconds": "30", "work_seconds": "2"},
		Env:                []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatalf("launching %s: %v", instanceID, err)
	}
	t.Cleanup(inst.Kill)
	return inst
}

func TestAnUpgradeMidMessageDoesNotStallTheQueue(t *testing.T) {
	const work = 2 * time.Second

	q, deps, binary := upgradeQueueStack(t)
	v1 := launchSyncer(t, deps, binary, "syncer-v1", "1.0.0")

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "syncer", Instances: []*pluginhost.Instance{v1},
	})

	ctx := context.Background()
	start := time.Now()
	if _, _, err := q.Enqueue(ctx, "syncer", "accounts",
		[]byte(`{"account":"acct-upgrade"}`), db.EnqueueOptions{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Wait until the message is actually being worked on, so the upgrade lands
	// mid-message rather than before the consumer noticed it. Asserting on the
	// timing of an upgrade that arrived first would prove nothing.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st, err := q.Stats(ctx, "syncer", "accounts")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if st.Processing == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st, _ := q.Stats(ctx, "syncer", "accounts"); st.Processing != 1 {
		t.Fatalf("the message was never picked up (%+v); there is nothing to interrupt", st)
	}

	// Halfway through the work, swap in a new version.
	time.Sleep(work / 2)
	v2 := launchSyncer(t, deps, binary, "syncer-v2", "2.0.0")
	swapAt := time.Since(start)
	reg.Swap(ctx, pluginhost.Registration{
		Key: "syncer", Instances: []*pluginhost.Instance{v2},
	}, 5*time.Second)

	// Now: how long until the work is actually finished?
	drainDeadline := time.Now().Add(60 * time.Second)
	var drained time.Duration
	for time.Now().Before(drainDeadline) {
		st, err := q.Stats(ctx, "syncer", "accounts")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if st.Pending == 0 && st.Processing == 0 {
			drained = time.Since(start)
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if drained == 0 {
		t.Fatal("the message never completed within a minute of the upgrade")
	}

	t.Logf("upgrade committed %s in; the message finished %s after it was queued "+
		"(one unit of work is %s)", swapAt.Round(10*time.Millisecond),
		drained.Round(10*time.Millisecond), work)

	// The redelivery has to come from the shutdown, not from the visibility
	// timeout. One repeated unit of work is what at-least-once costs; waiting
	// out a thirty-second lease is a stalled queue on every deploy.
	if drained > 3*work {
		t.Errorf("the message took %s to finish across an upgrade, against %s of work. "+
			"That is the visibility timeout expiring, not the handler being told to "+
			"stop: every deploy would freeze whatever was in flight until the lease "+
			"lapsed", drained, work)
	}
}
