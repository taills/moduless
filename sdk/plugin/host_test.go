package sdk

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/taills/moduless/proto/plugin"
)

// The SDK is the only part of this framework a third-party author touches, and
// the query builder is the largest piece of logic in it. Everything here runs
// against a fake host: what is under test is the request the builder produces
// and what it makes of the reply, not the store behind it.

// fakeHost records the last request of each kind and returns canned replies.
// Embedding the interface means an unimplemented method panics loudly rather
// than silently returning a zero value.
type fakeHost struct {
	pb.HostServicesClient

	lastQuery     *pb.QueryRequest
	lastAggregate *pb.AggregateRequest
	lastPut       *pb.PutRequest
	lastEnqueue   *pb.EnqueueRequest
	lastAcquire   *pb.AcquireLockRequest
	lastRenew     *pb.LeaseRequest
	lastCacheSet  *pb.CacheSetRequest
	lastBeginTx   *pb.BeginTxRequest
	lastDownload  *pb.DownloadTokenRequest

	queryResp     *pb.QueryResponse
	aggregateResp *pb.AggregateResponse
	putResp       *pb.PutResponse
}

func (f *fakeHost) Query(_ context.Context, in *pb.QueryRequest, _ ...grpc.CallOption) (*pb.QueryResponse, error) {
	f.lastQuery = in
	if f.queryResp == nil {
		return &pb.QueryResponse{}, nil
	}
	return f.queryResp, nil
}

func (f *fakeHost) Aggregate(_ context.Context, in *pb.AggregateRequest, _ ...grpc.CallOption) (*pb.AggregateResponse, error) {
	f.lastAggregate = in
	if f.aggregateResp == nil {
		return &pb.AggregateResponse{}, nil
	}
	return f.aggregateResp, nil
}

func (f *fakeHost) Put(_ context.Context, in *pb.PutRequest, _ ...grpc.CallOption) (*pb.PutResponse, error) {
	f.lastPut = in
	if f.putResp == nil {
		return &pb.PutResponse{Version: 1}, nil
	}
	return f.putResp, nil
}

func newTestDB() (*DBClient, *fakeHost) {
	host := &fakeHost{}
	return &DBClient{c: host}, host
}

// Every operator must map to the right proto operator with the right number of
// values. A silent mismatch here is the worst kind: the query runs, returns
// plausible rows, and they are the wrong rows.
func TestQueryOperatorsMapToProto(t *testing.T) {
	tests := []struct {
		name   string
		build  func(*Query) *Query
		op     pb.Operator
		values []string
	}{
		{"Eq", func(q *Query) *Query { return q.Eq("status", "open") }, pb.Operator_OP_EQ, []string{"open"}},
		{"Ne", func(q *Query) *Query { return q.Ne("status", "shut") }, pb.Operator_OP_NE, []string{"shut"}},
		{"Gt", func(q *Query) *Query { return q.Gt("age", "18") }, pb.Operator_OP_GT, []string{"18"}},
		{"Gte", func(q *Query) *Query { return q.Gte("age", "18") }, pb.Operator_OP_GTE, []string{"18"}},
		{"Lt", func(q *Query) *Query { return q.Lt("age", "65") }, pb.Operator_OP_LT, []string{"65"}},
		{"Lte", func(q *Query) *Query { return q.Lte("age", "65") }, pb.Operator_OP_LTE, []string{"65"}},
		{"Like", func(q *Query) *Query { return q.Like("name", "%ann%") }, pb.Operator_OP_LIKE, []string{"%ann%"}},
		{"In", func(q *Query) *Query { return q.In("id", "a", "b", "c") }, pb.Operator_OP_IN, []string{"a", "b", "c"}},
		{"Between", func(q *Query) *Query { return q.Between("age", "18", "65") }, pb.Operator_OP_BETWEEN, []string{"18", "65"}},
		{"IsNull", func(q *Query) *Query { return q.IsNull("deleted") }, pb.Operator_OP_IS_NULL, nil},
		{"IsNotNull", func(q *Query) *Query { return q.IsNotNull("deleted") }, pb.Operator_OP_IS_NOT_NULL, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, host := newTestDB()
			if _, err := tc.build(db.Where("notes")).All(context.Background(), nil); err != nil {
				t.Fatalf("All: %v", err)
			}

			filters := host.lastQuery.GetFilters()
			if len(filters) != 1 {
				t.Fatalf("%d filters sent, want 1", len(filters))
			}
			if got := filters[0].GetOp(); got != tc.op {
				t.Errorf("operator = %v, want %v", got, tc.op)
			}
			if got := filters[0].GetValues(); len(got) != len(tc.values) {
				t.Fatalf("values = %v, want %v", got, tc.values)
			}
			for i, want := range tc.values {
				if filters[0].GetValues()[i] != want {
					t.Errorf("value %d = %q, want %q", i, filters[0].GetValues()[i], want)
				}
			}
		})
	}
}

// Chained conditions must accumulate in order, not replace each other. A
// builder that dropped all but the last would return far too many rows while
// looking like it worked.
func TestQueryChainsConditions(t *testing.T) {
	db, host := newTestDB()

	_, err := db.Where("notes").
		Eq("author", "ann").
		Gt("created", "2026-01-01").
		In("tag", "x", "y").
		SortDesc("created").
		Sort("title").
		Limit(25).
		After("cursor-abc").
		All(context.Background(), nil)
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	req := host.lastQuery
	if req.GetCollection() != "notes" {
		t.Errorf("collection = %q", req.GetCollection())
	}
	if n := len(req.GetFilters()); n != 3 {
		t.Fatalf("%d filters, want all 3 chained conditions", n)
	}
	if got := req.GetFilters()[0].GetField(); got != "author" {
		t.Errorf("first filter is on %q; conditions were reordered", got)
	}

	sorts := req.GetSort()
	if len(sorts) != 2 {
		t.Fatalf("%d sort keys, want 2", len(sorts))
	}
	if !sorts[0].GetDescending() || sorts[0].GetField() != "created" {
		t.Errorf("first sort = %v, want created descending", sorts[0])
	}
	if sorts[1].GetDescending() || sorts[1].GetField() != "title" {
		t.Errorf("second sort = %v, want title ascending", sorts[1])
	}

	if req.GetLimit() != 25 {
		t.Errorf("limit = %d, want 25", req.GetLimit())
	}
	if req.GetCursor() != "cursor-abc" {
		t.Errorf("cursor = %q, want it passed through for keyset paging", req.GetCursor())
	}
}

// A fresh Where must not inherit conditions from a previous one. Sharing state
// between queries would leak one caller's filters into another's results.
func TestQueriesAreIndependent(t *testing.T) {
	db, host := newTestDB()

	base := db.Where("notes")
	if _, err := base.Eq("author", "ann").All(context.Background(), nil); err != nil {
		t.Fatalf("All: %v", err)
	}

	if _, err := db.Where("notes").Eq("author", "bob").All(context.Background(), nil); err != nil {
		t.Fatalf("All: %v", err)
	}

	filters := host.lastQuery.GetFilters()
	if len(filters) != 1 {
		t.Fatalf("the second query carried %d filters; state leaked from the first", len(filters))
	}
	if got := filters[0].GetValues()[0]; got != "bob" {
		t.Errorf("second query filtered on %q, want bob", got)
	}
}

// Results must decode into the author's own struct type. This is the whole
// point of the fluent API: a plugin works with its own types, not with JSON.
func TestQueryDecodesIntoCallerTypes(t *testing.T) {
	type note struct {
		Title  string `json:"title"`
		Author string `json:"author"`
	}

	db, host := newTestDB()
	host.queryResp = &pb.QueryResponse{
		Documents: [][]byte{
			[]byte(`{"title":"first","author":"ann"}`),
			[]byte(`{"title":"second","author":"bob"}`),
		},
		NextCursor: "next-page",
	}

	var notes []note
	cursor, err := db.Where("notes").All(context.Background(), &notes)
	if err != nil {
		t.Fatalf("All: %v", err)
	}

	if len(notes) != 2 {
		t.Fatalf("decoded %d documents, want 2", len(notes))
	}
	if notes[0].Title != "first" || notes[1].Author != "bob" {
		t.Errorf("decoded %+v", notes)
	}
	if cursor != "next-page" {
		t.Errorf("cursor = %q, want the next page token; paging would stop after one page", cursor)
	}
}

// An empty page must produce an empty slice and an empty cursor, not an error.
// Paging code loops until the cursor is empty, so a spurious error or a
// non-empty cursor here loops forever.
func TestQueryEmptyPageEndsPaging(t *testing.T) {
	db, host := newTestDB()
	host.queryResp = &pb.QueryResponse{}

	var out []struct{}
	cursor, err := db.Where("notes").All(context.Background(), &out)
	if err != nil {
		t.Fatalf("an empty page returned an error: %v", err)
	}
	if cursor != "" {
		t.Errorf("cursor = %q on an empty page; a paging loop would never terminate", cursor)
	}
	if len(out) != 0 {
		t.Errorf("decoded %d documents from an empty page", len(out))
	}
}

// Count must reuse the query's filters. A count that ignored them would
// report the size of the whole collection, which looks reasonable and is
// wrong.
func TestCountCarriesFilters(t *testing.T) {
	db, host := newTestDB()
	host.aggregateResp = &pb.AggregateResponse{
		Buckets: []*pb.AggregateResponse_Bucket{{Value: 42}},
	}

	n, err := db.Where("notes").Eq("author", "ann").Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 42 {
		t.Errorf("count = %d, want 42", n)
	}

	req := host.lastAggregate
	if req.GetFunc() != pb.AggregateFunc_AGG_COUNT {
		t.Errorf("aggregate func = %v, want COUNT", req.GetFunc())
	}
	if len(req.GetFilters()) != 1 {
		t.Fatalf("count sent %d filters; it would count the whole collection", len(req.GetFilters()))
	}
}

// Counting nothing is zero, not an error. The host returns no buckets when
// nothing matched.
func TestCountOfNothingIsZero(t *testing.T) {
	db, host := newTestDB()
	host.aggregateResp = &pb.AggregateResponse{}

	n, err := db.Where("notes").Eq("author", "nobody").Count(context.Background())
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

// Grouped aggregates must come back keyed by group, and an ungrouped one keyed
// by the empty string, as documented.
func TestAggregateGrouping(t *testing.T) {
	db, host := newTestDB()
	host.aggregateResp = &pb.AggregateResponse{
		Buckets: []*pb.AggregateResponse_Bucket{
			{Keys: map[string]string{"author": "ann"}, Value: 10},
			{Keys: map[string]string{"author": "bob"}, Value: 32},
		},
	}

	got, err := db.Where("notes").Sum(context.Background(), "words", "author")
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	if got["ann"] != 10 || got["bob"] != 32 {
		t.Errorf("grouped sum = %v", got)
	}
	if req := host.lastAggregate; req.GetField() != "words" {
		t.Errorf("aggregate field = %q, want words", req.GetField())
	}
	if req := host.lastAggregate; len(req.GetGroupBy()) != 1 || req.GetGroupBy()[0] != "author" {
		t.Errorf("group by = %v, want [author]", host.lastAggregate.GetGroupBy())
	}
}

// Put must serialise the caller's value as JSON, since that is what the
// document store holds.
func TestPutSerialisesValue(t *testing.T) {
	db, host := newTestDB()

	type note struct {
		Title string `json:"title"`
	}
	version, err := db.Put(context.Background(), "notes", "n1", note{Title: "hello"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if version != 1 {
		t.Errorf("version = %d, want the value the host returned", version)
	}

	req := host.lastPut
	if req.GetCollection() != "notes" || req.GetDocId() != "n1" {
		t.Errorf("put addressed %s/%s", req.GetCollection(), req.GetDocId())
	}

	var round note
	if err := json.Unmarshal(req.GetData(), &round); err != nil {
		t.Fatalf("the stored bytes are not JSON: %v", err)
	}
	if round.Title != "hello" {
		t.Errorf("stored %+v, want the value passed in", round)
	}
}

// User returns nil for an unauthenticated request, and a plugin reading the
// caller is the most ordinary thing there is. Every accessor therefore has to
// tolerate nil — a panic inside a plugin kills the process, so one anonymous
// request would otherwise take the plugin down.
func TestUserAccessorsAreNilSafe(t *testing.T) {
	var absent *UserContext

	if got := absent.Name(); got != "" {
		t.Errorf("Name() = %q on a nil user", got)
	}
	if got := absent.ID(); got != "" {
		t.Errorf("ID() = %q on a nil user", got)
	}
	if absent.Authenticated() {
		t.Error("Authenticated() is true for a nil user")
	}
	if absent.HasRole("admin") {
		t.Error("HasRole() is true for a nil user")
	}

	present := &UserContext{UserID: "7", Username: "ann", Roles: []string{"admin"}}
	if present.Name() != "ann" || present.ID() != "7" {
		t.Errorf("accessors returned %q/%q for a present user", present.Name(), present.ID())
	}
	if !present.Authenticated() || !present.HasRole("admin") {
		t.Error("a present user reports as unauthenticated or roleless")
	}
	if present.HasRole("nope") {
		t.Error("HasRole matched a role the user does not hold")
	}
}

// The transactional client mirrors the non-transactional one.
//
// TxClient.Get used to return only (found, error), dropping the version, so a
// read-modify-write inside a transaction could not use PutIfVersion — and
// PutIfVersion did not exist on TxClient at all. The asymmetry read as "you
// do not need optimistic locking inside a transaction", which is not true:
// two transactions can both read a row, and the second write proceeds against
// a row the first has already changed.
//
// Checked by signature rather than by behaviour: what went wrong was the
// shape, and a shape is what this can hold still.
func TestTxClientMirrorsTheDBClient(t *testing.T) {
	var (
		db *DBClient
		tx *TxClient
	)

	// Both Gets: (ctx, collection, id, dest) -> (found, version, error).
	var dbGet func(context.Context, string, string, any) (bool, int64, error) = db.Get
	var txGet func(context.Context, string, string, any) (bool, int64, error) = tx.Get
	_, _ = dbGet, txGet

	// Both Puts, plain and optimistic.
	var dbPut func(context.Context, string, string, any) (int64, error) = db.Put
	var txPut func(context.Context, string, string, any) (int64, error) = tx.Put
	_, _ = dbPut, txPut

	var dbCAS func(context.Context, string, string, any, int64) (int64, error) = db.PutIfVersion
	var txCAS func(context.Context, string, string, any, int64) (int64, error) = tx.PutIfVersion
	_, _ = dbCAS, txCAS
}

func (f *fakeHost) Enqueue(_ context.Context, in *pb.EnqueueRequest, _ ...grpc.CallOption) (*pb.EnqueueResponse, error) {
	f.lastEnqueue = in
	return &pb.EnqueueResponse{MessageId: 1}, nil
}

func newTestQueue() (*QueueClient, *fakeHost) {
	host := &fakeHost{}
	return &QueueClient{c: host}, host
}

// What the publish options put on the wire.
//
// None of these had a test, in Core or here — WithDelay, WithPriority and
// WithMaxAttempts are referenced nowhere outside their own declarations. They
// are also the whole author-facing surface of the queue's scheduling
// behaviour, and the test that measured delayed delivery this week went
// through db.Queue directly, so it would not have noticed if the SDK dropped
// the option on the floor.
func TestPublishOptionsReachTheRequest(t *testing.T) {
	tests := []struct {
		name string
		opt  EnqueueOption
		want func(*pb.EnqueueRequest) bool
		desc string
	}{
		{
			name: "delay of whole seconds",
			opt:  WithDelay(90 * time.Second),
			want: func(r *pb.EnqueueRequest) bool { return r.GetDelaySeconds() == 90 },
			desc: "delay_seconds = 90",
		},
		{
			name: "priority",
			opt:  WithPriority(5),
			want: func(r *pb.EnqueueRequest) bool { return r.GetPriority() == 5 },
			desc: "priority = 5",
		},
		{
			name: "max attempts",
			opt:  WithMaxAttempts(3),
			want: func(r *pb.EnqueueRequest) bool { return r.GetMaxAttempts() == 3 },
			desc: "max_attempts = 3",
		},
		{
			name: "dedup key",
			opt:  WithDedupKey("nightly-2026-08-16"),
			want: func(r *pb.EnqueueRequest) bool { return r.GetDedupKey() == "nightly-2026-08-16" },
			desc: `dedup_key = "nightly-2026-08-16"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, host := newTestQueue()
			if _, _, err := q.Publish(t.Context(), "jobs", map[string]int{"n": 1}, tc.opt); err != nil {
				t.Fatalf("Publish: %v", err)
			}
			if host.lastEnqueue == nil {
				t.Fatal("no enqueue reached the host")
			}
			if !tc.want(host.lastEnqueue) {
				t.Errorf("the option did not produce %s: %+v", tc.desc, host.lastEnqueue)
			}
		})
	}
}

// A delay shorter than a second is not the same as no delay.
//
// WithDelay takes a time.Duration, which measures nanoseconds, and puts it on
// the wire as whole seconds. Truncating means WithDelay(600*time.Millisecond)
// arrives as zero — the message becomes deliverable at once, and nothing tells
// the author their delay was dropped. A duration-shaped API that silently
// floors to seconds is worse than one that took seconds in the first place,
// because the type promises precision it discards.
//
// Rounding up rather than down is the direction that keeps the promise: the
// contract for a delayed message is "not before", so a 600ms delay honoured as
// one second is still correct, and honoured as zero is not.
func TestASubSecondDelayIsNotDiscarded(t *testing.T) {
	for _, d := range []time.Duration{time.Millisecond, 600 * time.Millisecond, 1500 * time.Millisecond} {
		q, host := newTestQueue()
		if _, _, err := q.Publish(t.Context(), "jobs", nil, WithDelay(d)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		got := host.lastEnqueue.GetDelaySeconds()
		onWire := time.Duration(got) * time.Second
		t.Logf("WithDelay(%s) → delay_seconds=%d (%s)", d, got, onWire)

		// The contract is "not before", so the wire value has to be at least
		// what was asked for. Flooring fails this in both directions that
		// matter: 600ms becomes no delay at all, and 1.5s becomes a message
		// deliverable half a second early.
		if onWire < d {
			t.Errorf("WithDelay(%s) put %s on the wire: the message becomes deliverable "+
				"before the caller asked, and nothing reported that the delay was "+
				"rounded away", d, onWire)
		}
	}
}

func (f *fakeHost) AcquireLock(_ context.Context, in *pb.AcquireLockRequest, _ ...grpc.CallOption) (*pb.AcquireLockResponse, error) {
	f.lastAcquire = in
	return &pb.AcquireLockResponse{Acquired: true, LeaseId: "lease-1"}, nil
}

func (f *fakeHost) RenewLock(_ context.Context, in *pb.LeaseRequest, _ ...grpc.CallOption) (*pb.AcquireLockResponse, error) {
	f.lastRenew = in
	return &pb.AcquireLockResponse{Acquired: true}, nil
}

func newTestLocks() (*LockClient, *fakeHost) {
	host := &fakeHost{}
	return &LockClient{c: host}, host
}

// The lock durations, as they reach the host.
//
// The lock backend and its RPC layer have four tests between them. The SDK
// wrapper — the part a plugin author actually calls — had none, and it is
// where the durations are converted.
func TestLockDurationsReachTheRequest(t *testing.T) {
	locks, host := newTestLocks()

	lease, ok, err := locks.Acquire(t.Context(), "nightly", 30*time.Second, 5*time.Second)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	if got := host.lastAcquire.GetTtlSeconds(); got != 30 {
		t.Errorf("ttl_seconds = %d, want 30", got)
	}
	if got := host.lastAcquire.GetWaitSeconds(); got != 5 {
		t.Errorf("wait_seconds = %d, want 5", got)
	}

	if _, err := lease.Renew(t.Context(), 45*time.Second); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if got := host.lastRenew.GetTtlSeconds(); got != 45 {
		t.Errorf("renew ttl_seconds = %d, want 45", got)
	}
	if got := host.lastRenew.GetLeaseId(); got != "lease-1" {
		t.Errorf("renew carried lease id %q, want the one Acquire returned", got)
	}
}

// A sub-second TTL is not the same as no TTL.
//
// Same truncation as WithDelay had, and it lands somewhere counter-intuitive:
// a zero ttl_seconds is not a lock that expires at once, it is the host's
// DefaultLockTTL of thirty seconds. So an author asking for a half-second
// lease — short on purpose, so that a crashed holder frees it quickly — gets
// one sixty times longer, and nothing says so.
func TestASubSecondLockTTLIsNotSilentlyChanged(t *testing.T) {
	for _, ttl := range []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 1500 * time.Millisecond} {
		locks, host := newTestLocks()
		if _, _, err := locks.Acquire(t.Context(), "job", ttl, 0); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		onWire := time.Duration(host.lastAcquire.GetTtlSeconds()) * time.Second
		t.Logf("Acquire ttl=%s → ttl_seconds=%d (%s)", ttl, host.lastAcquire.GetTtlSeconds(), onWire)

		if onWire < ttl {
			t.Errorf("ttl %s reached the host as %s: shorter than asked means the lease "+
				"can expire while its holder believes it still owns the lock, and zero "+
				"does not mean zero — the host reads it as its own default", ttl, onWire)
		}
	}
}

// A sub-second wait is not the same as not waiting.
//
// wait_seconds of zero makes the host return immediately rather than poll, so
// Acquire(…, 500*time.Millisecond) does not wait at all — it reports the lock
// as taken the instant it finds it held. A caller that wrote a short wait
// deliberately, to smooth over a contended moment, gets none of it.
func TestASubSecondLockWaitStillWaits(t *testing.T) {
	locks, host := newTestLocks()
	if _, _, err := locks.Acquire(t.Context(), "job", time.Minute, 500*time.Millisecond); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	got := host.lastAcquire.GetWaitSeconds()
	t.Logf("Acquire wait=500ms → wait_seconds=%d", got)
	if got < 1 {
		t.Errorf("a 500ms wait reached the host as %d seconds; the host treats zero as "+
			"do-not-wait, so the caller's wait was discarded rather than rounded", got)
	}
}

func (f *fakeHost) CacheSet(_ context.Context, in *pb.CacheSetRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.lastCacheSet = in
	return &emptypb.Empty{}, nil
}

func (f *fakeHost) BeginTx(_ context.Context, in *pb.BeginTxRequest, _ ...grpc.CallOption) (*pb.BeginTxResponse, error) {
	f.lastBeginTx = in
	return &pb.BeginTxResponse{TxId: "tx-1"}, nil
}

func (f *fakeHost) GenerateDownloadToken(_ context.Context, in *pb.DownloadTokenRequest, _ ...grpc.CallOption) (*pb.DownloadTokenResponse, error) {
	f.lastDownload = in
	return &pb.DownloadTokenResponse{Url: "/download/x"}, nil
}

// No positive duration may reach the wire as zero.
//
// Every duration this SDK sends crosses as whole seconds, and every conversion
// truncated. Zero is not "a very short time" to any of the readers — each one
// treats it as "use my own default" or "no limit", so asking for a small bound
// selected the unbounded case:
//
//	cache ttl 500ms      → an entry that never expires
//	lock ttl 500ms       → the host's thirty-second default
//	lock wait 500ms      → do not wait at all
//	tx timeout 500ms     → the thirty-second default
//	download expiry 500ms→ the five-minute default
//	publish delay 600ms  → deliverable immediately
//
// This walks every entry point rather than testing the conversion helper,
// because the helper being right is not the property that matters — a seventh
// call site that forgets to use it is the failure this has to catch.
func TestNoPositiveDurationBecomesZeroOnTheWire(t *testing.T) {
	const tiny = time.Millisecond

	t.Run("cache ttl", func(t *testing.T) {
		host := &fakeHost{}
		cache := &CacheClient{c: host}
		if err := cache.Set(t.Context(), "k", []byte("v"), tiny); err != nil {
			t.Fatalf("Set: %v", err)
		}
		if got := host.lastCacheSet.GetTtlSeconds(); got < 1 {
			t.Errorf("ttl_seconds = %d; a cache entry the caller wanted to expire is "+
				"stored with no expiry at all", got)
		}
	})

	t.Run("transaction timeout", func(t *testing.T) {
		host := &fakeHost{}
		db := &DBClient{c: host}
		if err := db.Tx(t.Context(), tiny, func(*TxClient) error { return nil }); err != nil {
			t.Fatalf("Tx: %v", err)
		}
		if got := host.lastBeginTx.GetTimeoutSeconds(); got < 1 {
			t.Errorf("timeout_seconds = %d; the transaction gets the host's default "+
				"instead of the short bound the caller asked for, and holds a pooled "+
				"connection for it", got)
		}
	})

	t.Run("download expiry", func(t *testing.T) {
		host := &fakeHost{}
		files := &FilesClient{c: host}
		if _, _, err := files.DownloadURL(t.Context(), "file-1", "user-1", tiny); err != nil {
			t.Fatalf("DownloadURL: %v", err)
		}
		if got := host.lastDownload.GetExpirySeconds(); got < 1 {
			t.Errorf("expiry_seconds = %d; a deliberately short-lived download link "+
				"gets the five-minute default", got)
		}
	})

	t.Run("publish delay", func(t *testing.T) {
		q, host := newTestQueue()
		if _, _, err := q.Publish(t.Context(), "jobs", nil, WithDelay(tiny)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
		if got := host.lastEnqueue.GetDelaySeconds(); got < 1 {
			t.Errorf("delay_seconds = %d; the message is deliverable at once", got)
		}
	})

	t.Run("lock ttl and wait", func(t *testing.T) {
		locks, host := newTestLocks()
		if _, _, err := locks.Acquire(t.Context(), "job", tiny, tiny); err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		if got := host.lastAcquire.GetTtlSeconds(); got < 1 {
			t.Errorf("ttl_seconds = %d; the lease gets the host's default", got)
		}
		if got := host.lastAcquire.GetWaitSeconds(); got < 1 {
			t.Errorf("wait_seconds = %d; the caller's wait is discarded", got)
		}
	})
}

func (f *fakeHost) CommitTx(_ context.Context, _ *pb.TxRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}

func (f *fakeHost) RollbackTx(_ context.Context, _ *pb.TxRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
