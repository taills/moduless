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
	})
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
		return handleBusy(ctx, j)
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

// handleBusy puts contended work back without spending a retry.
//
// Returning an error would be the obvious thing and it silently discards work:
// the queue increments a message's attempt count when it is *claimed*, not
// when it fails, so a message that keeps being picked up by a replica that
// cannot get the lock burns its whole budget — five by default — and lands in
// the dead-letter table having never failed. Measured: six messages for one
// contended account, four dead within three seconds, each recorded as
// "account is being synced by another replica", which reads like a diagnosis
// of the work rather than the accounting artefact it is.
//
// So acknowledge this delivery and publish the work afresh. A new message
// carries a new budget, which is right — being busy is not a failure — and
// Requeues rides along so that genuinely stuck work still ends up somewhere
// visible instead of circulating forever.
func handleBusy(ctx context.Context, j job) error {
	if j.Requeues >= maxRequeues {
		sdk.Log.Error(ctx, "giving up on a permanently locked account",
			"account", j.Account, "requeues", j.Requeues)
		return errStuck
	}

	j.Requeues++
	// context.WithoutCancel: this runs on the way out of a handler whose
	// context may already be cancelled by a drain, and dropping the republish
	// there would lose the work entirely.
	if _, _, err := sdk.Queue.Publish(context.WithoutCancel(ctx), "accounts", j,
		sdk.WithDelay(time.Second)); err != nil {
		// Could not republish, so fall back to failing: at least the queue's
		// own retry will bring it back.
		return err
	}
	sdk.Log.Info(ctx, "account busy, requeued", "account", j.Account, "requeues", j.Requeues)
	return nil
}

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
