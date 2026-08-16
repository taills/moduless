package tests

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	pb "github.com/taills/moduless/proto/plugin"
	"google.golang.org/grpc/codes"
)

// Reading inside a transaction.
//
// Put, Get, Delete and BatchWrite all resolved a transaction id into the
// executor that transaction is running on. Find, Query and Aggregate accepted
// the same parameter and dropped it — so a plugin that wrote a document and
// then queried for it in the same transaction did not find it. Nothing failed:
// the write was uncommitted and the query was running on a different
// connection, so the plugin simply got an answer that was wrong.
//
// The permission gate had drifted the same way. Put, Get, Delete, Query and
// BatchWrite all called requireTx, and Find and Aggregate did not, so a plugin
// without the db:tx grant could pass a transaction id to those two unchecked.
// It cost nothing while the id was being ignored, and would have become a way
// to read uncommitted data the moment somebody made these reads work — which
// is exactly what this change does.

func txData(t *testing.T, pluginKey string) *hostsvc.CMDSData {
	t.Helper()

	handle := requireDB(t)
	cmds := db.NewCMDSManager(handle)
	txs := db.NewTxRegistry()
	t.Cleanup(txs.Close)

	data := hostsvc.NewCMDSData(handle, cmds, txs)
	if err := data.ProvisionSchema(pluginKey, []db.CollectionSchema{{Name: "notes"}}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	return data
}

func putDoc(t *testing.T, data *hostsvc.CMDSData, key, id, title, txID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"title": title})
	if _, err := data.Put(context.Background(), key, "notes", id, body, txID, 0); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
}

// A document written inside a transaction is visible to a query in that same
// transaction. This is what "read your own writes" means, and a plugin that
// writes an index entry and then looks it up has no way to know it is not
// getting it.
func TestQuerySeesWritesInTheSameTransaction(t *testing.T) {
	data := txData(t, "txread")
	ctx := context.Background()

	txID, err := data.BeginTx(ctx, "txread", 0)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = data.RollbackTx(ctx, "txread", txID) }()

	putDoc(t, data, "txread", "a", "inside", txID)

	res, err := data.Query(ctx, "txread", "notes", hostsvc.QueryOptions{Limit: 10}, txID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Documents) != 1 {
		t.Fatalf("the query found %d document(s) inside the transaction that wrote one; "+
			"a plugin reading back its own uncommitted write gets a wrong answer, not an error",
			len(res.Documents))
	}
}

// The same for Aggregate: a count inside a transaction counts what that
// transaction has written.
func TestAggregateSeesWritesInTheSameTransaction(t *testing.T) {
	data := txData(t, "txagg")
	ctx := context.Background()

	txID, err := data.BeginTx(ctx, "txagg", 0)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = data.RollbackTx(ctx, "txagg", txID) }()

	putDoc(t, data, "txagg", "a", "one", txID)
	putDoc(t, data, "txagg", "b", "two", txID)

	buckets, err := data.Aggregate(ctx, "txagg", "notes",
		hostsvc.AggregateOptions{Func: "count"}, txID)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Value != 2 {
		t.Errorf("count inside the transaction = %v; want 2", buckets)
	}
}

// And Find, the older read, which had the same gap.
func TestFindSeesWritesInTheSameTransaction(t *testing.T) {
	data := txData(t, "txfind")
	ctx := context.Background()

	txID, err := data.BeginTx(ctx, "txfind", 0)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = data.RollbackTx(ctx, "txfind", txID) }()

	putDoc(t, data, "txfind", "a", "inside", txID)

	docs, err := data.Find(ctx, "txfind", "notes", nil, 10, 0, txID)
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(docs) != 1 {
		t.Errorf("Find returned %d document(s) inside the transaction that wrote one", len(docs))
	}
}

// The other direction, which is the reason transactions exist: a write that
// has not been committed is invisible to everyone else. A read that joined the
// wrong connection would fail this; a read that ignored isolation entirely
// would fail it too.
func TestUncommittedWritesAreInvisibleOutsideTheTransaction(t *testing.T) {
	data := txData(t, "txiso")
	ctx := context.Background()

	txID, err := data.BeginTx(ctx, "txiso", 0)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	putDoc(t, data, "txiso", "a", "pending", txID)

	// No transaction id: an ordinary read, on its own connection.
	res, err := data.Query(ctx, "txiso", "notes", hostsvc.QueryOptions{Limit: 10}, "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Documents) != 0 {
		t.Errorf("an uncommitted write is visible outside its transaction (%d documents)",
			len(res.Documents))
	}

	if err := data.RollbackTx(ctx, "txiso", txID); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	res, err = data.Query(ctx, "txiso", "notes", hostsvc.QueryOptions{Limit: 10}, "")
	if err != nil {
		t.Fatalf("query after rollback: %v", err)
	}
	if len(res.Documents) != 0 {
		t.Errorf("a rolled-back write survived (%d documents)", len(res.Documents))
	}
}

// Every read that accepts a transaction id checks the db:tx permission.
//
// Find and Aggregate did not, so a plugin holding only db could pass one. That
// was harmless while the id was ignored and is not harmless now.
func TestEveryTransactionalReadChecksThePermission(t *testing.T) {
	// db but not db:tx.
	s := hostsvc.New("nogrant", []string{"db"}, hostsvc.Deps{})
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Find", func() error {
			_, err := s.Find(ctx, &pb.FindRequest{Collection: "notes", TxId: "borrowed"})
			return err
		}},
		{"Query", func() error {
			_, err := s.Query(ctx, &pb.QueryRequest{Collection: "notes", TxId: "borrowed"})
			return err
		}},
		{"Aggregate", func() error {
			_, err := s.Aggregate(ctx, &pb.AggregateRequest{Collection: "notes", TxId: "borrowed"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatal("a plugin without db:tx passed a transaction id and was not refused")
			}
			// It must be refused for the permission, not because no database
			// is configured — this Deps is empty, so both refusals are
			// available and only one of them is the right one.
			if got := err.Error(); !strings.Contains(got, "db:tx") {
				t.Errorf("refused with %q, which does not name the missing permission", got)
			}
		})
	}
}

// The per-plugin transaction ceiling actually holds under concurrency.
//
// It did not. The count was derived by walking the open-transaction map in one
// critical section and the insert happened in another, so concurrent callers
// all counted zero, all passed a limit of four, and all opened one. Found by
// running the inventory example: thirty concurrent reservations against a
// limit of four produced log lines reading "already has 17 transactions open,
// the limit is 4" — seventeen of the pool's twenty-five connections held by
// one plugin, which is precisely what the ceiling exists to prevent.
func TestTransactionCeilingHoldsUnderConcurrency(t *testing.T) {
	data := txData(t, "txcap")
	ctx := context.Background()

	const attempts = 40
	var (
		mu      sync.Mutex
		granted []string
		refused int
		wg      sync.WaitGroup
	)
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := data.BeginTx(ctx, "txcap", 0)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				refused++
				return
			}
			granted = append(granted, id)
		}()
	}
	wg.Wait()

	t.Cleanup(func() {
		for _, id := range granted {
			_ = data.RollbackTx(ctx, "txcap", id)
		}
	})

	if len(granted) > db.MaxOpenTxPerPlugin {
		t.Errorf("%d transactions opened at once against a limit of %d; one plugin can take "+
			"the connection pool and everything else waits behind it",
			len(granted), db.MaxOpenTxPerPlugin)
	}
	if len(granted) == 0 {
		t.Error("nothing was granted; the limit is refusing everyone rather than capping")
	}
	if refused != attempts-len(granted) {
		t.Errorf("%d granted + %d refused != %d attempts", len(granted), refused, attempts)
	}
	t.Logf("%d granted, %d refused, limit %d", len(granted), refused, db.MaxOpenTxPerPlugin)
}

// Slots come back when a transaction ends, or the ceiling would be a
// one-time budget rather than a concurrency limit.
func TestTransactionSlotsAreReleased(t *testing.T) {
	data := txData(t, "txslots")
	ctx := context.Background()

	for round := range 3 {
		var ids []string
		for range db.MaxOpenTxPerPlugin {
			id, err := data.BeginTx(ctx, "txslots", 0)
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			ids = append(ids, id)
		}
		if _, err := data.BeginTx(ctx, "txslots", 0); err == nil {
			t.Fatalf("round %d: the limit did not refuse the extra transaction", round)
		}
		for _, id := range ids {
			if err := data.RollbackTx(ctx, "txslots", id); err != nil {
				t.Fatalf("rollback: %v", err)
			}
		}
	}
}

// Hitting the ceiling is retryable, and says so. It arrived as Internal, which
// a plugin cannot tell from a broken Core — so the only safe response was to
// give up.
func TestTransactionCeilingIsReportedAsRetryable(t *testing.T) {
	handle := requireDB(t)
	cmds := db.NewCMDSManager(handle)
	txs := db.NewTxRegistry()
	t.Cleanup(txs.Close)

	s := hostsvc.New("txcode", []string{"db", "db:tx"}, hostsvc.Deps{
		Data: hostsvc.NewCMDSData(handle, cmds, txs),
	})
	ctx := context.Background()

	for range db.MaxOpenTxPerPlugin {
		if _, err := s.BeginTx(ctx, &pb.BeginTxRequest{}); err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
	}

	_, err := s.BeginTx(ctx, &pb.BeginTxRequest{})
	if err == nil {
		t.Fatal("the ceiling did not refuse")
	}
	if got := codeOf(t, err); got != codes.ResourceExhausted {
		t.Errorf("code = %s, want ResourceExhausted; as %s a plugin cannot tell a busy "+
			"moment from a broken Core, and giving up is its only safe response", got, got)
	}
}

// A query result names its documents, against the real store.
//
// The fake in sdk/plugin proves the field travels; only this proves the ids
// are the ones Put used. Without that, "delete everything matching this
// filter" is unwritable — Delete takes an id and a query returned none.
func TestQueryReturnsTheIDsPutUsed(t *testing.T) {
	const key = "idsback"
	// txData provisions a "notes" collection, which is all this needs — the
	// collection name is not what is under test.
	data := txData(t, key)
	ctx := context.Background()

	want := map[string]bool{}
	for _, id := range []string{"rec-a", "rec-b", "rec-c"} {
		if _, err := data.Put(ctx, key, "notes", id,
			[]byte(`{"tenant":"acme"}`), "", 0); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
		want[id] = true
	}

	res, err := data.Query(ctx, key, "notes", hostsvc.QueryOptions{Limit: 10}, "")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(res.Documents) != 3 {
		t.Fatalf("%d documents, want 3", len(res.Documents))
	}
	if len(res.IDs) != len(res.Documents) {
		t.Fatalf("%d ids for %d documents; they are meant to line up by index",
			len(res.IDs), len(res.Documents))
	}
	for _, id := range res.IDs {
		if !want[id] {
			t.Errorf("query returned id %q, which was never written; the ids have to be "+
				"the ones Put used or Delete cannot take them", id)
		}
		delete(want, id)
	}
	if len(want) != 0 {
		t.Errorf("these written ids did not come back: %v", want)
	}

	// The point of having them: delete what the query found.
	for _, id := range res.IDs {
		ok, err := data.Delete(ctx, key, "notes", id, "")
		if err != nil || !ok {
			t.Errorf("deleting %s: ok=%v err=%v", id, ok, err)
		}
	}
	after, err := data.Query(ctx, key, "notes", hostsvc.QueryOptions{Limit: 10}, "")
	if err != nil {
		t.Fatalf("query after delete: %v", err)
	}
	if len(after.Documents) != 0 {
		t.Errorf("%d documents survived a delete driven by the query's own ids",
			len(after.Documents))
	}
}
