package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// queryTestDB provisions an isolated collection for one test.
func queryTestDB(t *testing.T, collection string) (*CMDSManager, string) {
	t.Helper()

	conn := testDB(t) // skips when TEST_DATABASE_URL is unset
	m := NewCMDSManager(conn)
	extKey := "qt"

	if err := m.ReconcileSchema(extKey, []CollectionSchema{{Name: collection}}); err != nil {
		t.Fatalf("ReconcileSchema: %v", err)
	}
	table, err := tableName(extKey, collection)
	if err != nil {
		t.Fatalf("tableName: %v", err)
	}
	if _, err := conn.Exec("TRUNCATE " + table); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return m, extKey
}

func putDoc(t *testing.T, m *CMDSManager, extKey, collection, id string, doc any) {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := m.PutVersioned(context.Background(), nil, extKey, collection, id, raw, 0); err != nil {
		t.Fatalf("put %s: %v", id, err)
	}
}

func idsOf(t *testing.T, docs [][]byte) []string {
	t.Helper()
	out := make([]string, 0, len(docs))
	for _, raw := range docs {
		var d struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out = append(out, d.Name)
	}
	return out
}

func TestQuerySortAndCursorPagination(t *testing.T) {
	m, ext := queryTestDB(t, "items")
	ctx := context.Background()

	for i := 1; i <= 7; i++ {
		putDoc(t, m, ext, "items", fmt.Sprintf("id-%d", i), map[string]any{
			"name": fmt.Sprintf("item-%d", i),
			"rank": i,
		})
	}

	// Page through with a limit of 3 and confirm every document appears once,
	// in order. Keyset pagination exists precisely so this stays correct.
	var seen []string
	cursor := ""
	pages := 0
	for {
		res, err := m.Query(ctx, nil, ext, "items", QueryOptions{
			Sort:   []SortField{{Field: "rank"}},
			Limit:  3,
			Cursor: cursor,
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		seen = append(seen, idsOf(t, res.Documents)...)
		pages++
		if !res.HasMore {
			break
		}
		if res.NextCursor == "" {
			t.Fatal("HasMore is set but no cursor was returned")
		}
		cursor = res.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}

	if len(seen) != 7 {
		t.Fatalf("saw %d documents across %d pages, want 7: %v", len(seen), pages, seen)
	}
	for i, name := range seen {
		want := fmt.Sprintf("item-%d", i+1)
		if name != want {
			t.Errorf("position %d = %s, want %s (full: %v)", i, name, want, seen)
		}
	}
}

func TestQueryDescendingCursor(t *testing.T) {
	m, ext := queryTestDB(t, "items")
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		putDoc(t, m, ext, "items", fmt.Sprintf("id-%d", i), map[string]any{
			"name": fmt.Sprintf("item-%d", i),
			"rank": i,
		})
	}

	res, err := m.Query(ctx, nil, ext, "items", QueryOptions{
		Sort:  []SortField{{Field: "rank", Descending: true}},
		Limit: 2,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := idsOf(t, res.Documents); len(got) != 2 || got[0] != "item-5" || got[1] != "item-4" {
		t.Fatalf("first page = %v, want [item-5 item-4]", got)
	}

	next, err := m.Query(ctx, nil, ext, "items", QueryOptions{
		Sort:   []SortField{{Field: "rank", Descending: true}},
		Limit:  2,
		Cursor: res.NextCursor,
	})
	if err != nil {
		t.Fatalf("Query page 2: %v", err)
	}
	if got := idsOf(t, next.Documents); len(got) != 2 || got[0] != "item-3" {
		t.Fatalf("second page = %v, want it to continue at item-3", got)
	}
}

// Mixed sort directions cannot use a row-value comparison, so they must be
// rejected rather than silently returning wrong pages.
func TestQueryRejectsMixedSortDirections(t *testing.T) {
	m, ext := queryTestDB(t, "items")

	_, err := m.Query(context.Background(), nil, ext, "items", QueryOptions{
		Sort: []SortField{{Field: "a"}, {Field: "b", Descending: true}},
	})
	if err == nil {
		t.Error("Query accepted mixed sort directions")
	}
}

func TestQueryPredicates(t *testing.T) {
	m, ext := queryTestDB(t, "items")
	ctx := context.Background()

	putDoc(t, m, ext, "items", "a", map[string]any{"name": "a", "status": "open", "rank": 1})
	putDoc(t, m, ext, "items", "b", map[string]any{"name": "b", "status": "closed", "rank": 5})
	putDoc(t, m, ext, "items", "c", map[string]any{"name": "c", "status": "open", "rank": 9})

	tests := []struct {
		name  string
		preds []Predicate
		want  int
	}{
		{name: "equality", preds: []Predicate{{Field: "status", Op: OpEq, Values: []string{"open"}}}, want: 2},
		{name: "inequality", preds: []Predicate{{Field: "status", Op: OpNe, Values: []string{"open"}}}, want: 1},
		{name: "IN", preds: []Predicate{{Field: "name", Op: OpIn, Values: []string{"a", "c"}}}, want: 2},
		{name: "empty IN matches nothing", preds: []Predicate{{Field: "name", Op: OpIn}}, want: 0},
		{name: "BETWEEN", preds: []Predicate{{Field: "rank", Op: OpBetween, Values: []string{"1", "5"}}}, want: 2},
		{name: "LIKE", preds: []Predicate{{Field: "status", Op: OpLike, Values: []string{"op%"}}}, want: 2},
		{name: "IS NULL", preds: []Predicate{{Field: "missing", Op: OpIsNull}}, want: 3},
		{name: "IS NOT NULL", preds: []Predicate{{Field: "status", Op: OpIsNotNull}}, want: 3},
		{
			name: "combined",
			preds: []Predicate{
				{Field: "status", Op: OpEq, Values: []string{"open"}},
				{Field: "rank", Op: OpGt, Values: []string{"5"}},
			},
			want: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := m.Query(ctx, nil, ext, "items", QueryOptions{Predicates: tc.preds, Limit: 50})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(res.Documents) != tc.want {
				t.Errorf("matched %d documents, want %d", len(res.Documents), tc.want)
			}
		})
	}
}

func TestQueryNestedJSONPath(t *testing.T) {
	m, ext := queryTestDB(t, "items")
	ctx := context.Background()

	putDoc(t, m, ext, "items", "a", map[string]any{
		"name":    "a",
		"profile": map[string]any{"address": map[string]any{"city": "Shanghai"}},
	})
	putDoc(t, m, ext, "items", "b", map[string]any{
		"name":    "b",
		"profile": map[string]any{"address": map[string]any{"city": "Beijing"}},
	})

	res, err := m.Query(ctx, nil, ext, "items", QueryOptions{
		Predicates: []Predicate{{Field: "profile.address.city", Op: OpEq, Values: []string{"Shanghai"}}},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Documents) != 1 {
		t.Fatalf("matched %d, want 1", len(res.Documents))
	}
}

// Field names reach SQL as identifiers, so anything that is not a plain
// identifier must be refused before it gets there.
func TestQueryRejectsInjectionAttempts(t *testing.T) {
	m, ext := queryTestDB(t, "items")
	ctx := context.Background()

	hostile := []string{
		"name'; DROP TABLE qt_items; --",
		"name) OR (1=1",
		"a.b'; SELECT 1; --",
		"",
		"1name",
	}

	for _, field := range hostile {
		t.Run(field, func(t *testing.T) {
			_, err := m.Query(ctx, nil, ext, "items", QueryOptions{
				Predicates: []Predicate{{Field: field, Op: OpEq, Values: []string{"x"}}},
			})
			if err == nil {
				t.Errorf("Query accepted hostile field %q", field)
			}
		})
	}

	_, err := m.Query(ctx, nil, ext, "items", QueryOptions{
		Predicates: []Predicate{{Field: "name", Op: "; DROP TABLE x; --", Values: []string{"a"}}},
	})
	if err == nil {
		t.Error("Query accepted an arbitrary operator")
	}
}

func TestAggregate(t *testing.T) {
	m, ext := queryTestDB(t, "items")
	ctx := context.Background()

	putDoc(t, m, ext, "items", "a", map[string]any{"team": "red", "score": 10})
	putDoc(t, m, ext, "items", "b", map[string]any{"team": "red", "score": 20})
	putDoc(t, m, ext, "items", "c", map[string]any{"team": "blue", "score": 7})

	total, err := m.Aggregate(ctx, nil, ext, "items", AggregateOptions{Func: AggCount})
	if err != nil {
		t.Fatalf("Aggregate count: %v", err)
	}
	if len(total) != 1 || total[0].Value != 3 {
		t.Fatalf("count = %+v, want a single bucket of 3", total)
	}

	grouped, err := m.Aggregate(ctx, nil, ext, "items", AggregateOptions{
		Func:    AggSum,
		Field:   "score",
		GroupBy: []string{"team"},
	})
	if err != nil {
		t.Fatalf("Aggregate sum: %v", err)
	}
	byTeam := map[string]float64{}
	for _, b := range grouped {
		byTeam[b.Keys["team"]] = b.Value
	}
	if byTeam["red"] != 30 || byTeam["blue"] != 7 {
		t.Errorf("grouped sums = %v, want red=30 blue=7", byTeam)
	}
}

// Two writers racing on one document must not silently lose an update.
func TestOptimisticLocking(t *testing.T) {
	m, ext := queryTestDB(t, "items")
	ctx := context.Background()

	v1, err := m.PutVersioned(ctx, nil, ext, "items", "doc", []byte(`{"n":1}`), 0)
	if err != nil {
		t.Fatalf("initial put: %v", err)
	}
	if v1 != 1 {
		t.Errorf("first version = %d, want 1", v1)
	}

	// Both readers see version 1.
	_, readVersion, found, err := m.GetVersioned(ctx, nil, ext, "items", "doc")
	if err != nil || !found {
		t.Fatalf("get: %v found=%v", err, found)
	}

	v2, err := m.PutVersioned(ctx, nil, ext, "items", "doc", []byte(`{"n":2}`), readVersion)
	if err != nil {
		t.Fatalf("first writer: %v", err)
	}
	if v2 != 2 {
		t.Errorf("second version = %d, want 2", v2)
	}

	// The second writer is still holding version 1 and must be told so rather
	// than overwriting what the first one wrote.
	_, err = m.PutVersioned(ctx, nil, ext, "items", "doc", []byte(`{"n":3}`), readVersion)
	if !errors.Is(err, ErrVersionConflict) {
		t.Errorf("stale write returned %v, want ErrVersionConflict", err)
	}

	data, _, _, _ := m.GetVersioned(ctx, nil, ext, "items", "doc")
	if string(data) != `{"n": 2}` && string(data) != `{"n":2}` {
		t.Errorf("stored document = %s, want the first writer's value", data)
	}
}

func TestTransactionCommitAndRollback(t *testing.T) {
	conn := testDB(t)
	m := NewCMDSManager(conn)
	ext := "qt"
	if err := m.ReconcileSchema(ext, []CollectionSchema{{Name: "tx"}}); err != nil {
		t.Fatalf("ReconcileSchema: %v", err)
	}
	table, _ := tableName(ext, "tx")
	if _, err := conn.Exec("TRUNCATE " + table); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	reg := NewTxRegistry()
	defer reg.Close()
	ctx := context.Background()

	// Rollback path.
	txID, err := reg.Begin(ctx, conn, "plugin-a", time.Minute)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, err := reg.Lookup("plugin-a", txID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if _, err := m.PutVersioned(ctx, tx, ext, "tx", "rolled-back", []byte(`{"n":1}`), 0); err != nil {
		t.Fatalf("put in tx: %v", err)
	}
	if err := reg.Rollback("plugin-a", txID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, _, found, _ := m.GetVersioned(ctx, nil, ext, "tx", "rolled-back"); found {
		t.Error("a rolled-back write is visible")
	}

	// Commit path.
	txID, err = reg.Begin(ctx, conn, "plugin-a", time.Minute)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	tx, _ = reg.Lookup("plugin-a", txID)
	if _, err := m.PutVersioned(ctx, tx, ext, "tx", "committed", []byte(`{"n":1}`), 0); err != nil {
		t.Fatalf("put in tx: %v", err)
	}
	if err := reg.Commit("plugin-a", txID); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if _, _, found, _ := m.GetVersioned(ctx, nil, ext, "tx", "committed"); !found {
		t.Error("a committed write is missing")
	}
	if reg.Open() != 0 {
		t.Errorf("%d transactions still open", reg.Open())
	}
}

// Transaction ids are handed to plugins, so one plugin must not be able to
// write inside another's transaction by presenting its id.
func TestTransactionIsBoundToItsPlugin(t *testing.T) {
	conn := testDB(t)
	reg := NewTxRegistry()
	defer reg.Close()

	txID, err := reg.Begin(context.Background(), conn, "plugin-a", time.Minute)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if _, err := reg.Lookup("plugin-b", txID); !errors.Is(err, ErrUnknownTx) {
		t.Errorf("another plugin resolved the transaction: %v", err)
	}
	if err := reg.Commit("plugin-b", txID); !errors.Is(err, ErrUnknownTx) {
		t.Errorf("another plugin committed the transaction: %v", err)
	}
	if _, err := reg.Lookup("plugin-a", txID); err != nil {
		t.Errorf("the owner can no longer use its own transaction: %v", err)
	}
}

// A plugin that crashes mid-transaction would otherwise pin a database
// connection until Core restarts.
func TestExpiredTransactionIsReaped(t *testing.T) {
	conn := testDB(t)
	reg := NewTxRegistry()
	defer reg.Close()

	now := time.Now()
	reg.SetClock(func() time.Time { return now })

	txID, err := reg.Begin(context.Background(), conn, "plugin-a", 10*time.Second)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if reg.Open() != 1 {
		t.Fatalf("open = %d, want 1", reg.Open())
	}

	now = now.Add(11 * time.Second)

	if got := reg.ReapExpired(); got != 1 {
		t.Errorf("reaped %d transactions, want 1", got)
	}
	if reg.Open() != 0 {
		t.Errorf("%d transactions survived the reaper", reg.Open())
	}
	if _, err := reg.Lookup("plugin-a", txID); !errors.Is(err, ErrUnknownTx) {
		t.Errorf("an expired transaction is still usable: %v", err)
	}
}

func TestBatchWriteIsAtomic(t *testing.T) {
	m, ext := queryTestDB(t, "batch")
	ctx := context.Background()

	applied, err := m.BatchWrite(ctx, nil, ext, []BatchOp{
		{Collection: "batch", DocID: "a", Data: []byte(`{"n":1}`)},
		{Collection: "batch", DocID: "b", Data: []byte(`{"n":2}`)},
	})
	if err != nil {
		t.Fatalf("BatchWrite: %v", err)
	}
	if applied != 2 {
		t.Errorf("applied = %d, want 2", applied)
	}

	// A batch naming a collection that does not exist must leave nothing
	// behind, or a plugin could end up with half its write applied.
	_, err = m.BatchWrite(ctx, nil, ext, []BatchOp{
		{Collection: "batch", DocID: "c", Data: []byte(`{"n":3}`)},
		{Collection: "nonexistent_collection", DocID: "d", Data: []byte(`{"n":4}`)},
	})
	if err == nil {
		t.Fatal("BatchWrite succeeded with an invalid collection")
	}
	if _, _, found, _ := m.GetVersioned(ctx, nil, ext, "batch", "c"); found {
		t.Error("a failed batch left a partial write behind")
	}
}

// A transaction holds a database connection for as long as it is open, so one
// plugin opening many is one plugin taking the pool. Everything else — other
// plugins, Core's own session lookups — then waits behind it until those
// transactions time out, which can be minutes.
//
// The quota turns that into a refusal aimed at whoever caused it.
func TestTransactionQuotaIsPerPlugin(t *testing.T) {
	conn := testDB(t)
	reg := NewTxRegistry()
	defer reg.Close()

	ctx := context.Background()

	var ids []string
	for i := range MaxOpenTxPerPlugin {
		id, err := reg.Begin(ctx, conn, "greedy", time.Minute)
		if err != nil {
			t.Fatalf("transaction %d of the allowance was refused: %v", i, err)
		}
		ids = append(ids, id)
	}

	// One past the quota.
	if _, err := reg.Begin(ctx, conn, "greedy", time.Minute); err == nil {
		t.Errorf("a plugin opened %d concurrent transactions; the pool has no defence against that",
			MaxOpenTxPerPlugin+1)
	} else {
		t.Logf("refused: %v", err)
	}

	// Another plugin is unaffected: the quota is per plugin, not global, so
	// one plugin's mistake does not become everyone's outage.
	other, err := reg.Begin(ctx, conn, "innocent", time.Minute)
	if err != nil {
		t.Errorf("a second plugin was refused because the first had used its quota: %v", err)
	} else {
		_ = reg.Rollback("innocent", other)
	}

	// And releasing one frees the allowance again.
	if err := reg.Rollback("greedy", ids[0]); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if id, err := reg.Begin(ctx, conn, "greedy", time.Minute); err != nil {
		t.Errorf("the allowance was not returned after a rollback: %v", err)
	} else {
		_ = reg.Rollback("greedy", id)
	}

	for _, id := range ids[1:] {
		_ = reg.Rollback("greedy", id)
	}
}
