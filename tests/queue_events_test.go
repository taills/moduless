package tests

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/event"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// Queue and events, end to end: a plugin in its own process reaching back
// through Core into a real PostgreSQL queue and a real event bus.
//
// The layers below have their own tests. What only shows up here is whether a
// plugin can actually use them — the reverse channel, the permission gate, the
// streaming call and the plugin's own SDK all sit between the plugin and the
// behaviour those tests verify.
//
//	TEST_DATABASE_URL='postgres://...' go test ./tests/ -run Queue

func requireDB(t *testing.T) *sql.DB {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	handle, err := db.InitDB(url)
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return handle
}

// queueStack launches a plugin wired to a real queue and a real event bus.
func queueStack(t *testing.T, key string, granted []string, config map[string]string) (*pluginhost.Instance, *event.EventBus) {
	t.Helper()

	handle := requireDB(t)
	bus := event.NewEventBus()

	cfg := hostsvc.NewStaticConfig()
	cfg.Set(key, config)

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        key,
		InstanceID: key + "-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New(key, granted, hostsvc.Deps{
			Config: cfg,
			Queue:  hostsvc.NewPGQueue(db.NewQueue(handle)),
			Events: hostsvc.NewBusEvents(bus),
			Cache:  hostsvc.NewMemoryCache(100),
			Locks:  hostsvc.NewMemoryLocks(),
		}),
		GrantedPermissions: granted,
		Config:             config,
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             os.Stderr,
	})
	if err != nil {
		t.Fatalf("launch %s: %v", key, err)
	}
	t.Cleanup(inst.Kill)
	return inst, bus
}

func callPlugin(t *testing.T, inst *pluginhost.Instance, path, query string) *pb.HttpResponse {
	t.Helper()
	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet,
		Path:   path,
		Query:  query,
	})
	if err != nil {
		t.Fatalf("calling %s: %v", path, err)
	}
	return resp
}

// The whole round trip: a plugin enqueues a message and consumes it back,
// across the process boundary in both directions.
func TestQueueRoundTripFromPlugin(t *testing.T) {
	inst, _ := queueStack(t, "qplugin", []string{"queue"}, nil)

	resp := callPlugin(t, inst, "/queue", "")
	if resp.GetStatusCode() != 200 {
		t.Fatalf("queue round trip failed: %s", resp.GetBody())
	}

	got := string(resp.GetBody())
	t.Logf("plugin reported: %s", got)
	if !strings.Contains(got, "queued work") {
		t.Errorf("the message came back as %q, want the payload that was sent", got)
	}
	if !strings.Contains(got, "attempt=1") {
		t.Errorf("first delivery reported %q, want attempt=1", got)
	}
}

// The queue capability must be refused without the permission, like every
// other capability. A queue is durable state; a plugin writing to it
// undeclared is exactly what the permission list exists to make visible.
func TestQueueRequiresPermission(t *testing.T) {
	inst, _ := queueStack(t, "qplugin", nil, nil) // no grants

	resp := callPlugin(t, inst, "/queue", "")
	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin used the queue without declaring the permission")
	}
	if body := string(resp.GetBody()); !strings.Contains(body, "queue") {
		t.Errorf("the refusal does not name the missing permission: %q", body)
	}
}

// Queues are namespaced per plugin: two plugins using the same topic name must
// not see each other's work. Getting this wrong would have one plugin silently
// consuming another's jobs, and the symptom would appear in the victim.
func TestQueueIsolatedBetweenPlugins(t *testing.T) {
	handle := requireDB(t)
	queue := db.NewQueue(handle)

	ctx := context.Background()
	topic := fmt.Sprintf("shared-%d", time.Now().UnixNano())

	if _, _, err := queue.Enqueue(ctx, "plugin-a", topic, []byte("for a"), db.EnqueueOptions{}); err != nil {
		t.Fatalf("enqueue for a: %v", err)
	}

	// Plugin B claims from the same topic name and must find nothing.
	msgs, err := queue.Claim(ctx, "plugin-b", topic, 10, 30*time.Second)
	if err != nil {
		t.Fatalf("claim for b: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("plugin-b claimed %d message(s) from plugin-a's topic %q", len(msgs), topic)
	}

	// And A still has its message.
	msgs, err = queue.Claim(ctx, "plugin-a", topic, 10, 30*time.Second)
	if err != nil {
		t.Fatalf("claim for a: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("plugin-a claimed %d message(s), want its own 1", len(msgs))
	}
}

// A message enqueued before a plugin restarts must still be there afterwards.
// This is the difference between the durable queue and the event bus, and the
// reason the queue exists at all.
func TestQueueSurvivesPluginRestart(t *testing.T) {
	handle := requireDB(t)
	queue := db.NewQueue(handle)

	ctx := context.Background()
	topic := fmt.Sprintf("durable-%d", time.Now().UnixNano())

	if _, _, err := queue.Enqueue(ctx, "restarter", topic, []byte("survive me"), db.EnqueueOptions{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// A plugin process comes and goes without touching the message.
	inst, _ := queueStack(t, "restarter", []string{"queue"}, nil)
	inst.Kill()
	inst2, _ := queueStack(t, "restarter", []string{"queue"}, nil)
	_ = inst2

	msgs, err := queue.Claim(ctx, "restarter", topic, 10, 30*time.Second)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("%d message(s) after a restart, want the enqueued 1", len(msgs))
	}
	if got := string(msgs[0].Payload); got != "survive me" {
		t.Errorf("payload = %q", got)
	}
}

// One plugin publishing an event and another hearing it, each in its own
// process. This is the only sanctioned way for plugins to talk to each other,
// so if it does not work the alternative is plugins reaching into each other's
// tables.
func TestEventsCrossPluginBoundary(t *testing.T) {
	handle := requireDB(t)
	bus := event.NewEventBus()

	launch := func(key string, granted []string, config map[string]string) *pluginhost.Instance {
		t.Helper()
		cfg := hostsvc.NewStaticConfig()
		cfg.Set(key, config)

		inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
			Key:        key,
			InstanceID: key + "-0",
			Version:    "1.0.0",
			BinaryPath: pluginBinary,
			Checksum:   checksum(t, pluginBinary),
			HostImpl: hostsvc.New(key, granted, hostsvc.Deps{
				Config: cfg,
				Queue:  hostsvc.NewPGQueue(db.NewQueue(handle)),
				Events: hostsvc.NewBusEvents(bus),
			}),
			GrantedPermissions: granted,
			Config:             config,
			Env:                []string{"PATH=/usr/bin:/bin"},
			Stderr:             os.Stderr,
		})
		if err != nil {
			t.Fatalf("launch %s: %v", key, err)
		}
		t.Cleanup(inst.Kill)
		return inst
	}

	// The listener subscribes to the publisher's event.
	listener := launch("listener", []string{"events"},
		map[string]string{"subscribe_to": "publisher:thing.happened"})
	publisher := launch("publisher", []string{"events"}, nil)

	// Give the subscription time to reach the bus before publishing: an event
	// bus is not a queue, and anything sent before a subscriber arrives is
	// gone.
	deadline := time.Now().Add(5 * time.Second)
	for bus.SubscriberCount("publisher:thing.happened") == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if bus.SubscriberCount("publisher:thing.happened") == 0 {
		t.Fatal("the listener never subscribed")
	}

	if resp := callPlugin(t, publisher, "/publish", "hello-from-publisher"); resp.GetStatusCode() != 200 {
		t.Fatalf("publish failed: %s", resp.GetBody())
	}

	// The listener records what it heard.
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got := string(callPlugin(t, listener, "/received", "").GetBody())
		if strings.Contains(got, "hello-from-publisher") {
			t.Logf("listener received: %q", got)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the listener never heard the publisher's event")
}

// Events must be refused without the permission too.
func TestEventsRequirePermission(t *testing.T) {
	inst, _ := queueStack(t, "silent", nil, nil)

	resp := callPlugin(t, inst, "/publish", "nobody-should-hear-this")
	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin published an event without declaring the events permission")
	}
	if body := string(resp.GetBody()); !strings.Contains(body, "events") {
		t.Errorf("the refusal does not name the missing permission: %q", body)
	}
}

// A plugin that dies with a transaction open must not pin a database
// connection for ever. Core holds the real transaction on the plugin's behalf,
// so nothing about the dead process will release it — only Core's own expiry
// will.
func TestTransactionSurvivesPluginDeath(t *testing.T) {
	handle := requireDB(t)

	cmds := db.NewCMDSManager(handle)
	txs := db.NewTxRegistry()
	defer txs.Close()

	data := hostsvc.NewCMDSData(handle, cmds, txs)
	if err := data.ProvisionSchema("txcrash", []db.CollectionSchema{{Name: "notes"}}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := handle.Exec("TRUNCATE ext_txcrash_notes"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	cfg := hostsvc.NewStaticConfig()
	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        "txcrash",
		InstanceID: "txcrash-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New("txcrash", []string{"db", "db:tx"}, hostsvc.Deps{
			Config: cfg,
			Data:   data,
		}),
		GrantedPermissions: []string{"db", "db:tx"},
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             os.Stderr,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer inst.Kill()

	// The plugin opens a transaction, writes, and exits without committing.
	// The call itself fails because the process goes away mid-RPC, which is
	// the point.
	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/tx-then-crash",
	})
	if err == nil && resp.GetStatusCode() != 0 {
		t.Logf("plugin answered instead of dying: %d %s", resp.GetStatusCode(), resp.GetBody())
	}

	deadline := time.Now().Add(15 * time.Second)
	for !inst.ProcessExited() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !inst.ProcessExited() {
		t.Fatal("the plugin did not die as the test intended")
	}

	// Wait past the transaction's own timeout, then reap.
	time.Sleep(2500 * time.Millisecond)
	reaped := txs.ReapExpired()
	t.Logf("reaped %d abandoned transaction(s)", reaped)
	if reaped == 0 {
		t.Error("no transaction was reaped; the dead plugin's transaction is still holding a connection")
	}

	// The uncommitted write must not be visible: an abandoned transaction is
	// rolled back, not silently committed.
	var count int
	if err := handle.QueryRow(`SELECT count(*) FROM ext_txcrash_notes`).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 0 {
		t.Errorf("%d row(s) written inside an abandoned transaction survived; it was committed rather than rolled back", count)
	}

	// And the pool is healthy afterwards.
	if err := handle.Ping(); err != nil {
		t.Errorf("the database is unusable after reaping: %v", err)
	}
}

// The ordinary path the crash test found by accident: a transaction spanning
// several RPCs. Begin in one call, write in two more, commit in a fourth.
//
// This is what a plugin's sdk.DB.Tx does underneath, and it was completely
// inoperable — the transaction was bound to the context of the call that
// opened it, and gRPC cancels that as soon as the handler returns, so
// database/sql rolled it back before the plugin could use it. Nothing below
// this level noticed, because a unit test calling Begin with
// context.Background() has no cancellation to trip over.
func TestTransactionSpansMultipleCalls(t *testing.T) {
	handle := requireDB(t)

	cmds := db.NewCMDSManager(handle)
	txs := db.NewTxRegistry()
	defer txs.Close()

	data := hostsvc.NewCMDSData(handle, cmds, txs)
	if err := data.ProvisionSchema("txspan", []db.CollectionSchema{{Name: "notes"}}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if _, err := handle.Exec("TRUNCATE ext_txspan_notes"); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        "txspan",
		InstanceID: "txspan-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New("txspan", []string{"db", "db:tx"}, hostsvc.Deps{
			Config: hostsvc.NewStaticConfig(),
			Data:   data,
		}),
		GrantedPermissions: []string{"db", "db:tx"},
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             os.Stderr,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	defer inst.Kill()

	resp := callPlugin(t, inst, "/tx-commit", "")
	if resp.GetStatusCode() != 200 {
		t.Fatalf("a transaction across several calls failed: %s", resp.GetBody())
	}

	var count int
	if err := handle.QueryRow(`SELECT count(*) FROM ext_txspan_notes`).Scan(&count); err != nil {
		t.Fatalf("counting rows: %v", err)
	}
	if count != 2 {
		t.Errorf("%d row(s) committed, want both writes from the transaction", count)
	}
}

// At-least-once delivery, which is the entire reason the durable queue exists.
//
// A plugin that receives a message and dies before acknowledging it must not
// have consumed that work. Treating delivery as completion would lose the job
// in exactly the situation the queue is there to survive.
//
// Note what this depends on. Claim only takes rows in 'pending', so a message
// leased to a consumer that vanished stays in 'processing' until the
// maintenance loop returns it. At-least-once is a property of that loop
// running, not of the queue table on its own — Core starts it at a 30s
// interval, which is also the worst-case redelivery delay on top of the
// visibility timeout.
func TestQueueRedeliversAfterConsumerCrash(t *testing.T) {
	handle := requireDB(t)
	queue := db.NewQueue(handle)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The fixture hard-codes this topic, so earlier runs leave rows behind and
	// a later run would claim one of those instead — reporting attempt 1 for a
	// message no consumer had ever crashed on.
	if _, err := handle.ExecContext(ctx,
		`DELETE FROM plugin_queue WHERE owner_key = 'crasher' AND topic = 'crashtest'`); err != nil {
		t.Fatalf("clearing earlier runs: %v", err)
	}

	hostsvc.NewPGQueue(queue).StartMaintenance(ctx, 200*time.Millisecond, time.Hour)

	if _, _, err := queue.Enqueue(ctx, "crasher", "crashtest",
		[]byte("work that must survive"), db.EnqueueOptions{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// A plugin receives it and dies without acking.
	inst, _ := queueStack(t, "crasher", []string{"queue"}, nil)
	_, _ = inst.Client.HandleHTTP(ctx, &pb.HttpRequest{
		Method: http.MethodGet, Path: "/queue-consume-crash",
	})

	deadline := time.Now().Add(15 * time.Second)
	for !inst.ProcessExited() && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !inst.ProcessExited() {
		t.Fatal("the plugin did not die as the test intended")
	}

	var again []db.Message
	var err error
	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		again, err = queue.Claim(ctx, "crasher", "crashtest", 10, 30*time.Second)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if len(again) > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(again) != 1 {
		t.Fatalf("the message was never redelivered after its consumer died; " +
			"work handed to a plugin that crashed has been lost")
	}
	t.Logf("redelivered on attempt %d: %s", again[0].Attempt, again[0].Payload)

	if string(again[0].Payload) != "work that must survive" {
		t.Errorf("payload = %q", again[0].Payload)
	}
	if again[0].Attempt < 2 {
		t.Errorf("attempt = %d on a redelivery, want it counted as a retry; without that "+
			"a message that always crashes its consumer would never reach the dead letter",
			again[0].Attempt)
	}
}

// The other half: while a lease is still held, nobody else gets the message.
// At-least-once must not become at-least-twice-simultaneously, or two workers
// do the same job at once.
func TestQueueDoesNotRedeliverWhileLeased(t *testing.T) {
	handle := requireDB(t)
	queue := db.NewQueue(handle)
	ctx := context.Background()

	// No maintenance loop here on purpose: this is about the lease itself.
	if _, err := handle.ExecContext(ctx,
		`DELETE FROM plugin_queue WHERE owner_key = 'leaseholder' AND topic = 'leased'`); err != nil {
		t.Fatalf("clearing earlier runs: %v", err)
	}
	if _, _, err := queue.Enqueue(ctx, "leaseholder", "leased",
		[]byte("one job"), db.EnqueueOptions{}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	first, err := queue.Claim(ctx, "leaseholder", "leased", 10, 30*time.Second)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first claim got %d message(s)", len(first))
	}

	second, err := queue.Claim(ctx, "leaseholder", "leased", 10, 30*time.Second)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("a leased message was handed to a second consumer; two workers would run the same job")
	}
}

// The backlog bound, end to end against a real queue.
//
// Depth is measured on a timer rather than counted on every enqueue: counting
// rows per write would make the common path pay for a rare failure, and a
// producer that has run away is not stopped meaningfully better by catching it
// a few seconds sooner.
func TestQueueDepthIsMeasuredAndEnforced(t *testing.T) {
	handle := requireDB(t)
	queue := db.NewQueue(handle)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := handle.ExecContext(ctx,
		`DELETE FROM plugin_queue WHERE owner_key IN ('depthy', 'bystander')`); err != nil {
		t.Fatalf("clearing earlier runs: %v", err)
	}

	pq := hostsvc.NewPGQueue(queue)
	pq.MaxDepth = 5
	pq.StartMaintenance(ctx, 100*time.Millisecond, time.Hour)

	// Fill past the limit. These go in below it, so they are accepted.
	for i := range 6 {
		if _, _, err := pq.Enqueue(ctx, "depthy", "work",
			[]byte(fmt.Sprintf("job %d", i)), hostsvc.EnqueueOptions{}); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	// Wait for maintenance to notice.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && pq.Depth("depthy") < 5 {
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("measured depth: %d", pq.Depth("depthy"))

	if _, _, err := pq.Enqueue(ctx, "depthy", "work", []byte("one too many"),
		hostsvc.EnqueueOptions{}); err == nil {
		t.Error("a plugin over its backlog limit was allowed to enqueue more")
	} else {
		t.Logf("refused: %v", err)
	}

	// A different plugin is unaffected — the bound is per plugin, so one
	// runaway producer does not stop everyone else's work.
	if _, _, err := pq.Enqueue(ctx, "bystander", "work", []byte("fine"),
		hostsvc.EnqueueOptions{}); err != nil {
		t.Errorf("an unrelated plugin was refused because of another's backlog: %v", err)
	}
}
