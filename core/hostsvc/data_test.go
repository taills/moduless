package hostsvc

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	_ "github.com/lib/pq"

	"github.com/taills/moduless/core/db"
	pb "github.com/taills/moduless/proto/plugin"
)

// dataTestDeps builds a real document store, skipping when no database is
// configured. Follows the existing TEST_DATABASE_URL convention.
func dataTestDeps(t *testing.T) (Deps, func()) {
	t.Helper()

	connStr := os.Getenv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping host data-service integration test")
	}
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("database unreachable: %v", err)
	}

	cmds := db.NewCMDSManager(conn)
	txs := db.NewTxRegistry()
	data := NewCMDSData(conn, cmds, txs)

	// Both plugins declare the same collection name on purpose: the isolation
	// test below depends on the names colliding logically while the physical
	// tables do not.
	for _, key := range []string{"plug_a", "plug_b"} {
		if err := data.ProvisionSchema(key, []db.CollectionSchema{{Name: "notes"}}); err != nil {
			t.Fatalf("provision %s: %v", key, err)
		}
		table := "ext_" + key + "_notes"
		if _, err := conn.Exec("TRUNCATE " + table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}

	return Deps{Data: data}, func() {
		txs.Close()
		conn.Close()
	}
}

func TestDataRequiresPermission(t *testing.T) {
	deps, cleanup := dataTestDeps(t)
	defer cleanup()

	s := New("plug_a", nil, deps)
	ctx := context.Background()

	calls := map[string]func() error{
		"Put": func() error {
			_, err := s.Put(ctx, &pb.PutRequest{Collection: "notes", DocId: "a", Data: []byte(`{}`)})
			return err
		},
		"Get": func() error {
			_, err := s.Get(ctx, &pb.GetRequest{Collection: "notes", DocId: "a"})
			return err
		},
		"Query": func() error {
			_, err := s.Query(ctx, &pb.QueryRequest{Collection: "notes"})
			return err
		},
		"Aggregate": func() error {
			_, err := s.Aggregate(ctx, &pb.AggregateRequest{Collection: "notes"})
			return err
		},
		"BatchWrite": func() error {
			_, err := s.BatchWrite(ctx, &pb.BatchWriteRequest{})
			return err
		},
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			if got := status.Code(call()); got != codes.PermissionDenied {
				t.Errorf("code = %v, want PermissionDenied", got)
			}
		})
	}
}

// Holding "db" is not enough to open a transaction: a transaction pins a
// database connection for as long as it is open, so it is a heavier grant than
// a single statement and is declared separately.
func TestTransactionsNeedTheirOwnPermission(t *testing.T) {
	deps, cleanup := dataTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	dbOnly := New("plug_a", []string{PermDB}, deps)

	if got := status.Code(mustErr(dbOnly.BeginTx(ctx, &pb.BeginTxRequest{}))); got != codes.PermissionDenied {
		t.Errorf("BeginTx code = %v, want PermissionDenied", got)
	}
	// Passing a transaction id on an ordinary write is the same escalation by
	// another route, so it is gated identically.
	_, err := dbOnly.Put(ctx, &pb.PutRequest{Collection: "notes", DocId: "a", Data: []byte(`{}`), TxId: "borrowed"})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("Put with a tx id: code = %v, want PermissionDenied", got)
	}

	full := New("plug_a", []string{PermDB, PermDBTx}, deps)
	resp, err := full.BeginTx(ctx, &pb.BeginTxRequest{TimeoutSeconds: 30})
	if err != nil {
		t.Fatalf("BeginTx with the permission: %v", err)
	}
	if resp.GetTxId() == "" {
		t.Error("BeginTx returned an empty transaction id")
	}
	if _, err := full.RollbackTx(ctx, &pb.TxRequest{TxId: resp.GetTxId()}); err != nil {
		t.Errorf("RollbackTx: %v", err)
	}
}

// Two plugins using the identical collection and document id must not see each
// other's data. The plugin key is part of the physical table name, so this
// holds even though neither plugin sends a key on the wire.
func TestDataIsIsolatedBetweenPlugins(t *testing.T) {
	deps, cleanup := dataTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	a := New("plug_a", []string{PermDB}, deps)
	b := New("plug_b", []string{PermDB}, deps)

	if _, err := a.Put(ctx, &pb.PutRequest{
		Collection: "notes", DocId: "shared", Data: []byte(`{"owner":"a"}`),
	}); err != nil {
		t.Fatalf("plug_a Put: %v", err)
	}

	got, err := b.Get(ctx, &pb.GetRequest{Collection: "notes", DocId: "shared"})
	if err != nil {
		t.Fatalf("plug_b Get: %v", err)
	}
	if got.GetFound() {
		t.Errorf("plug_b read plug_a's document: %s", got.GetData())
	}

	mine, err := a.Get(ctx, &pb.GetRequest{Collection: "notes", DocId: "shared"})
	if err != nil || !mine.GetFound() {
		t.Fatalf("plug_a lost its own document: %v found=%v", err, mine.GetFound())
	}

	// A write from the other plugin must not clobber it either.
	if _, err := b.Put(ctx, &pb.PutRequest{
		Collection: "notes", DocId: "shared", Data: []byte(`{"owner":"b"}`),
	}); err != nil {
		t.Fatalf("plug_b Put: %v", err)
	}
	after, _ := a.Get(ctx, &pb.GetRequest{Collection: "notes", DocId: "shared"})
	if string(after.GetData()) == `{"owner":"b"}` {
		t.Error("plug_b overwrote plug_a's document")
	}
}

func TestVersionConflictIsReportedDistinctly(t *testing.T) {
	deps, cleanup := dataTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	s := New("plug_a", []string{PermDB}, deps)

	first, err := s.Put(ctx, &pb.PutRequest{Collection: "notes", DocId: "v", Data: []byte(`{"n":1}`)})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	stale := first.GetVersion()

	if _, err := s.Put(ctx, &pb.PutRequest{
		Collection: "notes", DocId: "v", Data: []byte(`{"n":2}`), ExpectedVersion: stale,
	}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	// The stale writer must get a code that means "re-read and retry" rather
	// than a generic Internal it cannot act on.
	_, err = s.Put(ctx, &pb.PutRequest{
		Collection: "notes", DocId: "v", Data: []byte(`{"n":3}`), ExpectedVersion: stale,
	})
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Errorf("code = %v, want FailedPrecondition (err: %v)", got, err)
	}
}

func TestQueryThroughTheServiceLayer(t *testing.T) {
	deps, cleanup := dataTestDeps(t)
	defer cleanup()

	ctx := context.Background()
	s := New("plug_a", []string{PermDB}, deps)

	for i, body := range []string{`{"rank":1,"tag":"x"}`, `{"rank":2,"tag":"y"}`, `{"rank":3,"tag":"x"}`} {
		if _, err := s.Put(ctx, &pb.PutRequest{
			Collection: "notes", DocId: string(rune('a' + i)), Data: []byte(body),
		}); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	res, err := s.Query(ctx, &pb.QueryRequest{
		Collection: "notes",
		Filters:    []*pb.Filter{{Field: "tag", Op: pb.Operator_OP_EQ, Values: []string{"x"}}},
		Sort:       []*pb.Sort{{Field: "rank", Descending: true}},
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.GetDocuments()) != 2 {
		t.Fatalf("matched %d documents, want 2", len(res.GetDocuments()))
	}

	agg, err := s.Aggregate(ctx, &pb.AggregateRequest{
		Collection: "notes",
		Func:       pb.AggregateFunc_AGG_SUM,
		Field:      "rank",
		GroupBy:    []string{"tag"},
	})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	byTag := map[string]float64{}
	for _, b := range agg.GetBuckets() {
		byTag[b.GetKeys()["tag"]] = b.GetValue()
	}
	if byTag["x"] != 4 || byTag["y"] != 2 {
		t.Errorf("grouped sums = %v, want x=4 y=2", byTag)
	}
}

// A bad cursor is the caller's mistake and must be reported as such, so a
// plugin author sees a usable message instead of an opaque internal error.
func TestMalformedCursorIsInvalidArgument(t *testing.T) {
	deps, cleanup := dataTestDeps(t)
	defer cleanup()

	s := New("plug_a", []string{PermDB}, deps)
	_, err := s.Query(context.Background(), &pb.QueryRequest{
		Collection: "notes",
		Cursor:     "not-a-real-cursor",
	})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
}

func mustErr[T any](_ T, err error) error { return err }
