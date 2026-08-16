// Command syncer is an example plugin that serialises work across its own
// replicas with a host lock.
//
// The other five examples never take a lock, because nothing they do needs
// one. This is the case that does: two replicas of this plugin consume the
// same queue, and two messages for one account can land on both at once. The
// external system this stands in for rejects concurrent writes per account —
// as most do — so the work has to be serialised, and it has to be serialised
// across processes rather than inside one.
//
//	CGO_ENABLED=0 go build -o syncer/bin/plugin ./extension-example/syncer
//	cp extension-example/syncer/manifest.yaml syncer/
//	PLUGIN_DIR=$(pwd) go run ./core
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

// settings is what an operator configures. Guarded because Core pushes changes
// from a background goroutine while work is being processed.
var settings struct {
	sync.RWMutex
	lockWait time.Duration
	work     time.Duration
}

func current() (lockWait, work time.Duration) {
	settings.RLock()
	defer settings.RUnlock()
	return settings.lockWait, settings.work
}

// job is what gets queued: which account to pull.
//
// Requeues counts how many times this work has been put back for contention
// rather than for failing. It has to travel in the payload because the queue's
// own attempt counter cannot tell the two apart — see handleBusy.
type job struct {
	Account  string `json:"account"`
	Requeues int    `json:"requeues,omitempty"`
}

// maxRequeues bounds the shuffling. An account locked this many times running
// is not busy, it is stuck, and the message should be allowed to fail visibly.
const maxRequeues = 20

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sync", enqueue)

	settings.lockWait = 5 * time.Second
	settings.work = 2 * time.Second

	sdk.Serve(sdk.Config{
		Handler: mux,
		OnConfigChanged: func(cfg map[string]string) {
			settings.Lock()
			defer settings.Unlock()
			if n, err := strconv.Atoi(cfg["lock_wait_seconds"]); err == nil {
				settings.lockWait = time.Duration(n) * time.Second
			}
			if n, err := strconv.Atoi(cfg["work_seconds"]); err == nil {
				settings.work = time.Duration(n) * time.Second
			}
		},
		OnReady: consume,
	})
}

// enqueue accepts an account and puts it on the queue.
func enqueue(w http.ResponseWriter, r *http.Request) {
	var in job
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Account == "" {
		http.Error(w, "account is required", http.StatusBadRequest)
		return
	}
	if _, _, err := sdk.Queue.Publish(r.Context(), "accounts", in); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// consume runs on every replica. Both are consuming the same topic, which is
// the point: Core hands each message to whichever asks first, and two messages
// for one account can be in flight on two processes at the same moment.
func consume(ctx context.Context) {
	// Say so. A consumer that silently fails to start looks identical to one
	// with nothing to do, and the difference only shows up as work piling up
	// on whichever replica did start.
	sdk.Log.Info(ctx, "consumer starting", "replica", sdk.InstanceID())

	// Longer than the default, not shorter.
	//
	// The visibility timeout is how long a message stays claimed by a replica
	// that has stopped answering, so a short one recovers a crash quickly —
	// but it must still exceed the longest this handler can run, or a message
	// is redelivered while somebody is still working on it. Here that worst
	// case is busyHoldWait spent waiting for a contended account plus the sync
	// itself, which is already past Core's thirty-second default.
	//
	// The cost is stated plainly: a replica that dies mid-sync leaves its
	// message untouchable for this long. That is the trade, and only a plugin
	// knows which side of it to be on.
	visibility := busyHoldWait + maxSync + 5*time.Second

	err := sdk.Queue.Consume(ctx, "accounts", func(ctx context.Context, m *sdk.QueueMessage) error {
		var j job
		if err := m.Decode(&j); err != nil {
			// Undecodable payloads never become decodable. Returning nil acks
			// the message rather than retrying it into the dead-letter table.
			sdk.Log.Error(ctx, "undecodable job", "id", m.ID, "err", err)
			return nil
		}
		sdk.Log.Info(ctx, "received", "id", m.ID, "account", j.Account, "replica", sdk.InstanceID())
		return syncAccount(ctx, j)
	}, sdk.WithVisibilityTimeout(visibility))
	if err != nil && !errors.Is(err, context.Canceled) {
		sdk.Log.Error(ctx, "consumer stopped", "err", err)
	}
}

// syncAccount does the work under a lock named for the account.
//
// The name is the unit of exclusion, so it is the account rather than
// something global: two replicas syncing *different* accounts should both
// proceed. A single lock named "sync" would be correct and would also make the
// second replica pointless.
func syncAccount(ctx context.Context, j job) error {
	account := j.Account
	lockWait, work := current()

	// ttl covers the work with room to spare. If this process dies holding the
	// lock, the lease is what lets another replica take over — too short and
	// somebody steals it from a live holder, too long and a crash blocks the
	// account until it expires.
	ttl := work + 30*time.Second

	lease, ok, err := sdk.Locks.Acquire(ctx, "account:"+account, ttl, lockWait)
	if err != nil {
		return err
	}
	if !ok {
		lease, ok, err = handleBusy(ctx, j, ttl)
		if err != nil || !ok {
			return err
		}
	}
	defer func() {
		if err := lease.Release(context.WithoutCancel(ctx)); err != nil {
			// Not fatal: the lease expires on its own. Worth logging because a
			// release that keeps failing means every account is held for its
			// full ttl instead of the seconds it needed.
			sdk.Log.Warn(ctx, "releasing lock", "account", account, "err", err)
		}
	}()

	sdk.Log.Info(ctx, "syncing", "account", account, "replica", sdk.InstanceID())
	start := time.Now()

	// Work longer than the lease has to renew, or the lock silently becomes
	// available while this is still using it. Renew reporting false means the
	// lease is already gone and somebody else may be on this account — stop
	// rather than finish, because finishing is what would double-write.
	done := make(chan error, 1)
	go func() { done <- doWork(ctx, account, work) }()

	renew := time.NewTicker(ttl / 3)
	defer renew.Stop()
	for {
		select {
		case err := <-done:
			sdk.Log.Metric(ctx, "syncer_account_synced", 1, map[string]string{"account": account})
			sdk.Log.Histogram(ctx, "syncer_sync_seconds", time.Since(start).Seconds(), nil)
			return err
		case <-renew.C:
			held, err := lease.Renew(ctx, ttl)
			if err != nil || !held {
				sdk.Log.Error(ctx, "lost the lock mid-sync", "account", account, "err", err)
				return errLostLock
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// handleBusy deals with an account another replica is holding.
//
// Two ways out, and which one is right depends on whether the queue has room.
//
// Putting the work back is the first choice, because returning an error is not
// free: the queue increments a message's attempt count when it is *claimed*,
// not when it fails, so a message bounced between contending replicas burns
// its whole budget — five by default — and lands in the dead-letter table
// having never failed. Measured: six messages for one hot account, four dead
// within three seconds, each recorded as "another replica is syncing this",
// which reads like a diagnosis of the work rather than the accounting artefact
// it is. Republishing gives the work a fresh budget, which is right, because
// being busy is not a failure.
//
// But a republish is an enqueue, and Core refuses those once the plugin's
// backlog is at its limit — one shared table, so a pile-up is everyone's. That
// turned the safe path back into the unsafe one exactly under pressure:
// measured again with a limit of three, four of six dead. And it should:
// adding to a backlog that is already over its limit is the wrong move, and
// the message is *already* in the queue.
//
// So when there is nowhere to put it, hold on and wait for the lock instead.
// Blocking here is backpressure — it costs neither a retry nor a slot in the
// backlog, and it is what a full queue is asking for.
func handleBusy(ctx context.Context, j job, ttl time.Duration) (*sdk.Lease, bool, error) {
	if j.Requeues < maxRequeues {
		j.Requeues++
		// context.WithoutCancel: this runs on the way out of a handler whose
		// context may already be cancelled by a drain, and dropping the
		// republish there would lose the work entirely.
		if _, _, err := sdk.Queue.Publish(context.WithoutCancel(ctx), "accounts", j,
			sdk.WithDelay(time.Second)); err == nil {
			sdk.Log.Info(ctx, "account busy, requeued",
				"account", j.Account, "requeues", j.Requeues)
			return nil, false, nil
		}
	}

	sdk.Log.Info(ctx, "account busy and nowhere to requeue; waiting for the lock",
		"account", j.Account, "requeues", j.Requeues)
	lease, ok, err := sdk.Locks.Acquire(ctx, "account:"+j.Account, ttl, busyHoldWait)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, errStuck
	}
	return lease, true, nil
}

// maxSync is the longest a single sync is expected to take. The subscription
// is opened once, so it is sized against this ceiling rather than against
// whatever work_seconds happens to say at the time.
const maxSync = 20 * time.Second

// busyHoldWait bounds the fallback wait for a contended account.
//
// Kept short on purpose, and the reason is not obvious: this number is part of
// how long the handler can run, and the handler's worst case is what the
// visibility timeout has to exceed — which is in turn how long a crashed
// replica strands its message. Waiting longer here buys smoother behaviour
// under contention and pays for it in crash-recovery latency. The two cannot
// both be small.
const busyHoldWait = 10 * time.Second

// doWork stands in for the external call.
func doWork(ctx context.Context, account string, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var (
	errStuck    = errors.New("account stayed locked across every requeue")
	errLostLock = errors.New("lost the account lock mid-sync")
)
