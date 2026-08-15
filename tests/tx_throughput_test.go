package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
)

// How much the per-plugin transaction ceiling costs.
//
// The ceiling exists for a good reason: an open transaction holds a database
// connection, so without a per-plugin bound one plugin can take the whole pool
// and everything else — including Core's own queries — waits behind it. But
// the number was picked by argument ("four is generous for work that is
// supposed to be short"), not measured, and until recently it did not hold at
// all, so nobody had ever seen what it does to throughput.
//
// This measures the thing an operator actually cares about: completed
// reservations per second on a contended item, with the retry loop a correct
// plugin has to write anyway.
//
//	TEST_DATABASE_URL=... go test ./tests/ -run TestTransactionCeilingThroughput -v

// reserveOp is the inventory example's transaction, reduced to what matters:
// read a document, decide, write it back under optimistic locking.
func reserveOp(ctx context.Context, data *hostsvc.CMDSData, key, sku string) error {
	txID, err := data.BeginTx(ctx, key, 5*time.Second)
	if err != nil {
		return err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = data.RollbackTx(ctx, key, txID)
		}
	}()

	raw, version, found, err := data.Get(ctx, key, "items", sku, txID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no such item")
	}
	var item struct {
		OnHand   int `json:"on_hand"`
		Reserved int `json:"reserved"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	if item.OnHand-item.Reserved <= 0 {
		return errSoldOut
	}
	item.Reserved++

	body, _ := json.Marshal(item)
	if _, err := data.Put(ctx, key, "items", sku, body, txID, version); err != nil {
		return err
	}
	rollback = false
	return data.CommitTx(ctx, key, txID)
}

var errSoldOut = fmt.Errorf("sold out")

// runCeiling drives `workers` concurrent reservers for `duration` and reports
// what got through, with the retry a correct plugin writes.
func runCeiling(t *testing.T, ceiling, workers int, duration time.Duration, contended bool) (done, refused int64, took time.Duration) {
	t.Helper()

	handle := requireDB(t)
	txs := db.NewTxRegistry()
	txs.SetMaxOpen(ceiling)
	t.Cleanup(txs.Close)

	shape := "hot"
	if !contended {
		shape = "spread"
	}
	// A collection of this run's own. Reusing one across runs made the
	// benchmark contaminate itself: a few hundred thousand updates to the same
	// row leave that many dead tuples behind, and until autovacuum catches up
	// the next run measures the bloat as much as the ceiling. Two sweeps an
	// hour apart differed by 3x on absolute throughput for that reason alone.
	key := fmt.Sprintf("b%s%d%d", shape, ceiling, time.Now().UnixNano()%100000)
	data := hostsvc.NewCMDSData(handle, db.NewCMDSManager(handle), txs)
	if err := data.ProvisionSchema(key, []db.CollectionSchema{{Name: "items"}}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		_, _ = handle.Exec(fmt.Sprintf("DROP TABLE IF EXISTS ext_%s_items", key))
	})

	ctx := context.Background()
	// Effectively unlimited stock, so the run measures the machinery rather
	// than how fast it can sell out. One document when the point is
	// contention, one per worker when it is not.
	seed, _ := json.Marshal(map[string]int{"on_hand": 1 << 30, "reserved": 0})
	skus := make([]string, workers)
	for i := range skus {
		if contended {
			skus[i] = "hot"
		} else {
			skus[i] = fmt.Sprintf("sku-%d", i)
		}
		if _, err := data.Put(ctx, key, "items", skus[i], seed, "", 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	var (
		completed atomic.Int64
		gaveUp    atomic.Int64
		stop      atomic.Bool
		wg        sync.WaitGroup
	)
	start := time.Now()
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sku := skus[w]
			for !stop.Load() {
				var err error
				// The retry a correct plugin writes: a version conflict and a
				// full ceiling are both expected under contention.
				for attempt := range 12 {
					err = reserveOp(ctx, data, key, sku)
					if err == nil {
						break
					}
					retryable := isRetryable(err)
					if !retryable {
						break
					}
					if attempt < 11 {
						time.Sleep(time.Duration(attempt+1) * time.Millisecond)
					}
				}
				if err == nil {
					completed.Add(1)
				} else {
					gaveUp.Add(1)
				}
			}
		}()
	}
	time.Sleep(duration)
	stop.Store(true)
	wg.Wait()

	return completed.Load(), gaveUp.Load(), time.Since(start)
}

func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "version conflict") ||
		strings.Contains(msg, "too many open transactions")
}

// ceilings is what to sweep. Overridable so a single value can be re-measured
// without waiting for the whole sweep.
func ceilings() []int {
	if raw := os.Getenv("CEILINGS"); raw != "" {
		var out []int
		for _, f := range strings.Fields(raw) {
			n := 0
			_, _ = fmt.Sscanf(f, "%d", &n)
			if n > 0 {
				out = append(out, n)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []int{1, 2, 4, 8, 16, 25}
}

// The measurement. Off by default — it takes about a minute.
func TestTransactionCeilingThroughput(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE is not set")
	}
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	const (
		workers  = 32
		duration = 5 * time.Second
	)

	// Two shapes, because they answer different questions and only one of them
	// has ever been argued about.
	//
	// Contended is the worst case: every transaction fights for the same row.
	// Spread is the ordinary case: a plugin with many documents whose
	// transactions rarely meet. A ceiling that looks generous against one can
	// be the bottleneck against the other.
	for _, shape := range []struct {
		name      string
		contended bool
	}{
		{"one contended document", true},
		{"one document per worker", false},
	} {
		t.Logf("")
		t.Logf("%d concurrent reservers, %s, %s per ceiling", workers, shape.name, duration)
		t.Logf("%-8s %-10s %-10s", "ceiling", "done/s", "gave up")

		for _, ceiling := range ceilings() {
			done, refused, took := runCeiling(t, ceiling, workers, duration, shape.contended)
			t.Logf("%-8d %-10.0f %-10d", ceiling, float64(done)/took.Seconds(), refused)
		}
	}
}
