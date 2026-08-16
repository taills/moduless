package tests

import (
	"context"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
)

// A replica that dies holding a message, which is the half an upgrade does not
// cover.
//
// An upgrade is polite: Core asks the plugin to drain, the handler is told to
// stop, and the SDK reports the message on the way out so the new version
// picks it up in the same second. A crash is none of that. There is no
// shutdown, so nothing nacks, and the message stays claimed until maintenance
// notices its lease has lapsed.
//
// That is the designed fallback and it had never been measured end to end. The
// number matters because it is how long a queue stalls when a worker dies, and
// because a plugin author cannot currently change it: the visibility timeout
// is whatever Core defaults to, since the SDK never sends one.

func crashableSyncer(t *testing.T, work string, maintenance time.Duration) (*db.Queue, func() *pluginhost.Instance) {
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
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	pq.StartMaintenance(ctx, maintenance, time.Hour)

	cfg := map[string]string{"lock_wait_seconds": "30", "work_seconds": work}
	deps := hostsvc.Deps{
		Queue: pq,
		Locks: hostsvc.NewMemoryLocks(),
		Config: hostsvc.ConfigFunc(func(context.Context, string) (map[string]string, error) {
			return cfg, nil
		}),
		Obs: hostsvc.NewLogObservability("info"),
	}

	n := 0
	launch := func() *pluginhost.Instance {
		inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
			Key:                "syncer",
			InstanceID:         "syncer-" + itoa(n),
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
			t.Fatalf("launching: %v", err)
		}
		n++
		t.Cleanup(inst.Kill)
		return inst
	}
	return db.NewQueue(handle), launch
}

func TestAMessageSurvivesTheReplicaHoldingIt(t *testing.T) {
	// The slowest test here, and irreducibly so: what it measures is a lease
	// lapsing, and the example asks for a 35s lease. Skippable rather than
	// shortened, because a lease short enough to be quick would not be the one
	// the example ships with.
	if testing.Short() {
		t.Skip("waits out a 35s visibility timeout")
	}

	// Maintenance sampling far faster than Core's 30s, so what is measured is
	// the lease lapsing rather than the sweep interval on top of it.
	q, launch := crashableSyncer(t, "2", 200*time.Millisecond)
	victim := launch()
	standby := launch()
	_ = standby

	ctx := context.Background()
	if _, _, err := q.Enqueue(ctx, "syncer", "accounts",
		[]byte(`{"account":"acct-crash"}`), db.EnqueueOptions{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Wait until somebody is working on it, then kill whoever it is. Killing
	// before the message is picked up would measure nothing.
	deadline := time.Now().Add(15 * time.Second)
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
		t.Fatalf("the message was never picked up (%+v)", st)
	}

	killed := time.Now()
	victim.Kill()
	standby.Kill() // both, so nothing can finish it before the lease lapses

	// Bring a fresh replica up immediately, the way a supervisor would.
	launch()

	// How long until the work is done by somebody else?
	//
	// The example asks for a visibility timeout of busyHoldWait + maxSync + 5s
	// = 35s, because its handler can legitimately block that long waiting for
	// a contended account. That number *is* the crash-recovery latency, which
	// is the trade the example makes explicitly.
	recoverBy := time.Now().Add(75 * time.Second)
	var recovered time.Duration
	for time.Now().Before(recoverBy) {
		st, err := q.Stats(ctx, "syncer", "accounts")
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if st.Done > 0 {
			recovered = time.Since(killed)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if recovered == 0 {
		t.Fatal("the message was never redelivered after its holder died; a crash " +
			"loses the work permanently, which is not at-least-once")
	}
	t.Logf("a replica died holding a message; another finished it %s later "+
		"(the example asks for a 35s visibility timeout)",
		recovered.Round(100*time.Millisecond))
	if recovered > 60*time.Second {
		t.Errorf("recovery took %s, well past the visibility timeout the example asks "+
			"for; the lease is not what returned the message and something slower is",
			recovered)
	}
}
