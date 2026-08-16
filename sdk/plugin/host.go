package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	pb "github.com/taills/moduless/proto/plugin"
)

// Every call below attaches the ambient trace id automatically, so work a
// plugin does on Core's behalf stays attributable to the request that caused
// it without the author threading anything through.

// --- Documents --------------------------------------------------------------

// DBClient is the Core-managed document store. Collections are declared in
// manifest.yaml and provisioned before the plugin starts; a plugin never
// connects to PostgreSQL itself.
type DBClient struct{ c pb.HostServicesClient }

// Put writes a document, encoding value as JSON. It returns the new version.
func (d *DBClient) Put(ctx context.Context, collection, id string, value any) (int64, error) {
	if d == nil || d.c == nil {
		return 0, ErrHostUnavailable
	}
	data, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("encode document: %w", err)
	}
	resp, err := d.c.Put(outgoing(ctx), &pb.PutRequest{Collection: collection, DocId: id, Data: data})
	if err != nil {
		return 0, hostErr(err)
	}
	return resp.GetVersion(), nil
}

// PutIfVersion writes only when the stored version still matches, which is how
// two replicas update the same document without silently overwriting each
// other. A mismatch returns an error matching ErrVersionConflict: re-read and
// retry.
//
//	for range maxAttempts {
//		found, version, err := sdk.DB.Get(ctx, "stock", id, &item)
//		…
//		if _, err = sdk.DB.PutIfVersion(ctx, "stock", id, item, version); err == nil {
//			break
//		}
//		if !errors.Is(err, sdk.ErrVersionConflict) {
//			return err
//		}
//	}
//
// Branch on the sentinel, not on a gRPC code. This comment used to name
// FailedPrecondition, which stopped being true when version conflicts moved to
// Aborted so that they could be told apart from an expired transaction — and
// FailedPrecondition now means exactly that, an expired transaction, where
// retrying is the one thing that cannot work.
func (d *DBClient) PutIfVersion(ctx context.Context, collection, id string, value any, expected int64) (int64, error) {
	if d == nil || d.c == nil {
		return 0, ErrHostUnavailable
	}
	data, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("encode document: %w", err)
	}
	resp, err := d.c.Put(outgoing(ctx), &pb.PutRequest{
		Collection: collection, DocId: id, Data: data, ExpectedVersion: expected,
	})
	if err != nil {
		return 0, hostErr(err)
	}
	return resp.GetVersion(), nil
}

// Get decodes a document into dest. It reports false when the document does
// not exist, which is not an error.
func (d *DBClient) Get(ctx context.Context, collection, id string, dest any) (found bool, version int64, err error) {
	if d == nil || d.c == nil {
		return false, 0, ErrHostUnavailable
	}
	resp, err := d.c.Get(outgoing(ctx), &pb.GetRequest{Collection: collection, DocId: id})
	if err != nil {
		return false, 0, hostErr(err)
	}
	if !resp.GetFound() {
		return false, 0, nil
	}
	if dest != nil {
		if err := json.Unmarshal(resp.GetData(), dest); err != nil {
			return true, resp.GetVersion(), fmt.Errorf("decode document: %w", err)
		}
	}
	return true, resp.GetVersion(), nil
}

// Delete removes a document.
func (d *DBClient) Delete(ctx context.Context, collection, id string) (bool, error) {
	if d == nil || d.c == nil {
		return false, ErrHostUnavailable
	}
	resp, err := d.c.Delete(outgoing(ctx), &pb.DeleteRequest{Collection: collection, DocId: id})
	if err != nil {
		return false, hostErr(err)
	}
	return resp.GetSuccess(), nil
}

// Where builds a query. Use it rather than assembling filters by hand:
//
//	var orders []Order
//	next, err := sdk.DB.Where("orders").
//		Eq("status", "open").
//		Gt("total", "100").
//		SortDesc("created_at").
//		Limit(50).
//		All(ctx, &orders)
func (d *DBClient) Where(collection string) *Query {
	if d == nil || d.c == nil {
		// Unbound: no Core, so this is the plugin's own `go test`. The query
		// still builds — Describe is the point of letting it — and the deferred
		// error surfaces if something tries to run it.
		return &Query{collection: collection, err: ErrHostUnavailable}
	}
	return &Query{c: d.c, collection: collection}
}

// Query is a fluent read. Pagination is keyset-based, so paging deep into a
// collection stays as fast as the first page and rows shifting underneath do
// not cause duplicates or gaps.
type Query struct {
	c          pb.HostServicesClient
	collection string
	filters    []*pb.Filter
	sort       []*pb.Sort
	limit      int32
	cursor     string
	err        error
}

// QueryFilter is one condition a Query has accumulated.
type QueryFilter struct {
	Field string
	// Op is the operator's name without its wire prefix: EQ, NE, GT, GTE, LT,
	// LTE, LIKE, IN, BETWEEN, IS_NULL, IS_NOT_NULL.
	//
	// Taken from the enum rather than a hand-written table of symbols, so a new
	// operator cannot quietly render as the wrong one.
	Op     string
	Values []string
}

// QuerySort is one ordering a Query has accumulated.
type QuerySort struct {
	Field      string
	Descending bool
}

// QueryDescription is what a Query has been built to ask for.
type QueryDescription struct {
	Collection string
	Filters    []QueryFilter
	Sort       []QuerySort
	Limit      int
	Cursor     string
}

// Describe reports what this Query would ask Core for, without asking.
//
// Query building is where a handler's decisions live — whether the author
// filter was applied, whether the cursor was carried, whether the page size is
// what it claims — and until this existed none of it could be checked without a
// running Core and a database. Faking the builder means reimplementing it;
// inspecting it does not.
//
// It reports the request, not the answer. Nothing here says what rows come
// back, and a test that needs that still needs the end-to-end path in tests/.
func (q *Query) Describe() QueryDescription {
	d := QueryDescription{
		Collection: q.collection,
		Limit:      int(q.limit),
		Cursor:     q.cursor,
	}
	for _, f := range q.filters {
		d.Filters = append(d.Filters, QueryFilter{
			Field:  f.GetField(),
			Op:     strings.TrimPrefix(f.GetOp().String(), "OP_"),
			Values: f.GetValues(),
		})
	}
	for _, s := range q.sort {
		d.Sort = append(d.Sort, QuerySort{Field: s.GetField(), Descending: s.GetDescending()})
	}
	return d
}

func (q *Query) add(field string, op pb.Operator, values ...string) *Query {
	q.filters = append(q.filters, &pb.Filter{Field: field, Op: op, Values: values})
	return q
}

func (q *Query) Eq(field, value string) *Query  { return q.add(field, pb.Operator_OP_EQ, value) }
func (q *Query) Ne(field, value string) *Query  { return q.add(field, pb.Operator_OP_NE, value) }
func (q *Query) Gt(field, value string) *Query  { return q.add(field, pb.Operator_OP_GT, value) }
func (q *Query) Gte(field, value string) *Query { return q.add(field, pb.Operator_OP_GTE, value) }
func (q *Query) Lt(field, value string) *Query  { return q.add(field, pb.Operator_OP_LT, value) }
func (q *Query) Lte(field, value string) *Query { return q.add(field, pb.Operator_OP_LTE, value) }
func (q *Query) Like(field, pattern string) *Query {
	return q.add(field, pb.Operator_OP_LIKE, pattern)
}
func (q *Query) In(field string, values ...string) *Query {
	return q.add(field, pb.Operator_OP_IN, values...)
}
func (q *Query) Between(field, low, high string) *Query {
	return q.add(field, pb.Operator_OP_BETWEEN, low, high)
}
func (q *Query) IsNull(field string) *Query    { return q.add(field, pb.Operator_OP_IS_NULL) }
func (q *Query) IsNotNull(field string) *Query { return q.add(field, pb.Operator_OP_IS_NOT_NULL) }

// Sort orders ascending. Every sort field must share a direction when paging
// with a cursor.
func (q *Query) Sort(field string) *Query {
	q.sort = append(q.sort, &pb.Sort{Field: field})
	return q
}

// SortDesc orders descending.
func (q *Query) SortDesc(field string) *Query {
	q.sort = append(q.sort, &pb.Sort{Field: field, Descending: true})
	return q
}

func (q *Query) Limit(n int) *Query         { q.limit = int32(n); return q }
func (q *Query) After(cursor string) *Query { q.cursor = cursor; return q }

// All decodes the page into dest, which must be a pointer to a slice, and
// returns the cursor for the next page (empty when there is none).
func (q *Query) All(ctx context.Context, dest any) (nextCursor string, err error) {
	if q.err != nil {
		return "", q.err
	}
	resp, err := q.c.Query(outgoing(ctx), &pb.QueryRequest{
		Collection: q.collection,
		Filters:    q.filters,
		Sort:       q.sort,
		Limit:      q.limit,
		Cursor:     q.cursor,
	})
	if err != nil {
		return "", hostErr(err)
	}
	if dest != nil {
		if err := decodeDocuments(resp.GetDocuments(), dest); err != nil {
			return "", err
		}
	}
	return resp.GetNextCursor(), nil
}

// Rows is All with the document ids alongside.
//
// ids[i] names dest[i]. Use it when the point of the query is to act on what
// it finds: Delete and PutIfVersion both take an id, and All returns only the
// decoded bodies — so "delete everything matching this filter", which is the
// ordinary reason to run a query, could not be written without duplicating the
// id inside the document body first.
//
// That gap was found by handing this guide to somebody who had never seen the
// SDK and asking them to write exactly that. They reported it as the one thing
// they could not do, and were right: Core reads these ids on every query and
// used to discard all but the last, which it kept for the cursor.
func (q *Query) Rows(ctx context.Context, dest any) (ids []string, nextCursor string, err error) {
	if q.err != nil {
		return nil, "", q.err
	}
	resp, err := q.c.Query(outgoing(ctx), &pb.QueryRequest{
		Collection: q.collection,
		Filters:    q.filters,
		Sort:       q.sort,
		Limit:      q.limit,
		Cursor:     q.cursor,
	})
	if err != nil {
		return nil, "", hostErr(err)
	}
	if dest != nil {
		if err := decodeDocuments(resp.GetDocuments(), dest); err != nil {
			return nil, "", err
		}
	}
	return resp.GetIds(), resp.GetNextCursor(), nil
}

// Count returns how many documents match, without transferring them.
func (q *Query) Count(ctx context.Context) (int64, error) {
	if q.err != nil {
		return 0, q.err
	}
	resp, err := q.c.Aggregate(outgoing(ctx), &pb.AggregateRequest{
		Collection: q.collection,
		Filters:    q.filters,
		Func:       pb.AggregateFunc_AGG_COUNT,
	})
	if err != nil {
		return 0, hostErr(err)
	}
	if len(resp.GetBuckets()) == 0 {
		return 0, nil
	}
	return int64(resp.GetBuckets()[0].GetValue()), nil
}

// Sum totals a field, optionally grouped. With no group the result has one
// entry keyed by the empty string.
func (q *Query) Sum(ctx context.Context, field string, groupBy ...string) (map[string]float64, error) {
	return q.aggregate(ctx, pb.AggregateFunc_AGG_SUM, field, groupBy)
}

// Avg, Min and Max mirror Sum.
func (q *Query) Avg(ctx context.Context, field string, groupBy ...string) (map[string]float64, error) {
	return q.aggregate(ctx, pb.AggregateFunc_AGG_AVG, field, groupBy)
}
func (q *Query) Min(ctx context.Context, field string, groupBy ...string) (map[string]float64, error) {
	return q.aggregate(ctx, pb.AggregateFunc_AGG_MIN, field, groupBy)
}
func (q *Query) Max(ctx context.Context, field string, groupBy ...string) (map[string]float64, error) {
	return q.aggregate(ctx, pb.AggregateFunc_AGG_MAX, field, groupBy)
}

func (q *Query) aggregate(ctx context.Context, fn pb.AggregateFunc, field string, groupBy []string) (map[string]float64, error) {
	if q.err != nil {
		return nil, q.err
	}
	resp, err := q.c.Aggregate(outgoing(ctx), &pb.AggregateRequest{
		Collection: q.collection,
		Filters:    q.filters,
		Func:       fn,
		Field:      field,
		GroupBy:    groupBy,
	})
	if err != nil {
		return nil, hostErr(err)
	}
	out := make(map[string]float64, len(resp.GetBuckets()))
	for _, b := range resp.GetBuckets() {
		key := ""
		if len(groupBy) > 0 {
			key = b.GetKeys()[groupBy[0]]
		}
		out[key] = b.GetValue()
	}
	return out, nil
}

func decodeDocuments(docs [][]byte, dest any) error {
	// The documents are already JSON; assembling an array avoids reflecting
	// over dest to discover its element type.
	buf := make([]byte, 0, 64)
	buf = append(buf, '[')
	for i, d := range docs {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, d...)
	}
	buf = append(buf, ']')

	if err := json.Unmarshal(buf, dest); err != nil {
		return fmt.Errorf("decode documents: %w", err)
	}
	return nil
}

// TxOps is what a transaction body may do.
//
// An interface rather than the concrete *TxClient, so that the code inside a
// transaction can be tested. TxClient holds an unexported gRPC client, so an
// author cannot build one, and a callback typed to it was unreachable without a
// running Core — which put every invariant a transaction exists to protect
// (read the level, refuse when short, write both documents) out of reach of an
// ordinary go test.
//
// *TxClient satisfies this, so nothing changes at the call site except the type
// named in the closure.
type TxOps interface {
	Put(ctx context.Context, collection, id string, value any) (int64, error)
	PutIfVersion(ctx context.Context, collection, id string, value any, expected int64) (int64, error)
	Get(ctx context.Context, collection, id string, dest any) (found bool, version int64, err error)
	Delete(ctx context.Context, collection, id string) error
}

// Tx runs fn inside a transaction, committing on success and rolling back on
// error or panic. Requires the "db:tx" permission.
//
// A transaction holds a database connection for as long as it is open, and
// Core rolls it back once its timeout passes, so keep the work inside short.
func (d *DBClient) Tx(ctx context.Context, timeout time.Duration, fn func(tx TxOps) error) error {
	if d == nil || d.c == nil {
		return ErrHostUnavailable
	}
	resp, err := d.c.BeginTx(outgoing(ctx), &pb.BeginTxRequest{
		TimeoutSeconds: wholeSeconds(timeout),
	})
	if err != nil {
		return hostErr(err)
	}
	tx := &TxClient{c: d.c, id: resp.GetTxId()}

	committed := false
	defer func() {
		if !committed {
			_, _ = d.c.RollbackTx(outgoing(ctx), &pb.TxRequest{TxId: tx.id})
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if _, err := d.c.CommitTx(outgoing(ctx), &pb.TxRequest{TxId: tx.id}); err != nil {
		return err
	}
	committed = true
	return nil
}

// TxClient is the document store inside a transaction.
type TxClient struct {
	c  pb.HostServicesClient
	id string
}

func (t *TxClient) Put(ctx context.Context, collection, id string, value any) (int64, error) {
	return t.put(ctx, collection, id, value, 0)
}

// PutIfVersion is Put with optimistic locking, inside the transaction. It
// fails rather than overwriting when the stored version has moved on.
func (t *TxClient) PutIfVersion(ctx context.Context, collection, id string, value any, expected int64) (int64, error) {
	return t.put(ctx, collection, id, value, expected)
}

func (t *TxClient) put(ctx context.Context, collection, id string, value any, expected int64) (int64, error) {
	if t == nil || t.c == nil {
		return 0, ErrHostUnavailable
	}
	data, err := json.Marshal(value)
	if err != nil {
		return 0, fmt.Errorf("encode document: %w", err)
	}
	resp, err := t.c.Put(outgoing(ctx), &pb.PutRequest{
		Collection: collection, DocId: id, Data: data, TxId: t.id,
		ExpectedVersion: expected,
	})
	if err != nil {
		return 0, hostErr(err)
	}
	return resp.GetVersion(), nil
}

// Get decodes a document into dest, reading inside the transaction.
//
// It returns the version for the same reason the non-transactional Get does:
// a read-modify-write needs it to call PutIfVersion. A transaction does not
// remove that need — two transactions can both read a row, and the second
// write proceeds against a row the first has already changed — so a Get that
// dropped the version quietly made optimistic locking unavailable to exactly
// the code most likely to want it.
func (t *TxClient) Get(ctx context.Context, collection, id string, dest any) (found bool, version int64, err error) {
	if t == nil || t.c == nil {
		return false, 0, ErrHostUnavailable
	}
	resp, err := t.c.Get(outgoing(ctx), &pb.GetRequest{Collection: collection, DocId: id, TxId: t.id})
	if err != nil {
		return false, 0, hostErr(err)
	}
	if !resp.GetFound() {
		return false, 0, nil
	}
	if dest != nil {
		if err := json.Unmarshal(resp.GetData(), dest); err != nil {
			return true, resp.GetVersion(), fmt.Errorf("decode document: %w", err)
		}
	}
	return true, resp.GetVersion(), nil
}

func (t *TxClient) Delete(ctx context.Context, collection, id string) error {
	if t == nil || t.c == nil {
		return ErrHostUnavailable
	}
	_, err := t.c.Delete(outgoing(ctx), &pb.DeleteRequest{Collection: collection, DocId: id, TxId: t.id})
	return err
}

// --- Queue ------------------------------------------------------------------

// QueueClient is the durable queue. Delivery is at-least-once: a handler that
// completes and then crashes before acknowledging will see its message again,
// so handlers must be idempotent.
type QueueClient struct{ c pb.HostServicesClient }

// EnqueueOption tunes a publish.
type EnqueueOption func(*pb.EnqueueRequest)

// wholeSeconds converts a duration to the whole seconds the wire carries,
// rounding up.
//
// Every duration this SDK sends crosses as an int32 of seconds, and every one
// of them used to truncate. That is the wrong direction for all three of them
// and for different reasons, which is why they now share this:
//
//   - a delay is "not before", so flooring publishes early — 600ms became no
//     delay at all;
//   - a lock ttl is "held at least this long", so flooring can expire a lease
//     while its owner still believes it holds the lock. And zero does not mean
//     zero: the host reads it as DefaultLockTTL, so a deliberately short lease
//     silently became a thirty-second one;
//   - a wait is "try for this long", and the host treats zero as do-not-wait,
//     so any sub-second wait was discarded rather than shortened.
//
// The same truncation is in the cache ttl, the download-token expiry and the
// transaction timeout, and there the shared rule is clearer than "round up":
// **a positive duration must never reach the wire as zero**, because every
// reader treats zero as "use my own default" or "no limit at all". A 500ms
// cache entry became one that never expires; a 500ms download link became the
// five-minute default; a 500ms transaction timeout became thirty seconds. In
// each case the caller picked a small number precisely to bound something, and
// got the unbounded case instead.
//
// Rounding up costs at most a second, and never turns a duration the caller
// chose into a different behaviour. Sub-second precision is simply not
// expressible here; what matters is that asking for it does not silently
// select the opposite.
func wholeSeconds(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	return int32((d + time.Second - 1) / time.Second)
}

// WithDelay holds a message back before it becomes deliverable.
//
// The wire carries whole seconds, so a duration that is not a whole number of
// them is rounded **up**. That direction is not a detail: the guarantee is
// "not before", and rounding down breaks it — truncation turned
// WithDelay(600*time.Millisecond) into no delay at all, and
// WithDelay(1500*time.Millisecond) into a message deliverable half a second
// early, neither with any error to say the delay had been discarded.
//
// A delay is a floor, never a schedule. The message becomes *eligible* after
// it, and is delivered whenever a consumer next asks.
func WithDelay(d time.Duration) EnqueueOption {
	return func(r *pb.EnqueueRequest) { r.DelaySeconds = wholeSeconds(d) }
}

// WithDedupKey suppresses a duplicate while an identical message is still in
// flight. The key frees up once that message completes.
func WithDedupKey(key string) EnqueueOption {
	return func(r *pb.EnqueueRequest) { r.DedupKey = key }
}

// WithPriority raises a message above others on the same topic.
func WithPriority(p int) EnqueueOption {
	return func(r *pb.EnqueueRequest) { r.Priority = int32(p) }
}

// WithMaxAttempts overrides how many times a message is retried before it is
// parked as dead.
func WithMaxAttempts(n int) EnqueueOption {
	return func(r *pb.EnqueueRequest) { r.MaxAttempts = int32(n) }
}

// Publish adds a message, encoding payload as JSON.
func (q *QueueClient) Publish(ctx context.Context, topic string, payload any, opts ...EnqueueOption) (id int64, deduplicated bool, err error) {
	if q == nil || q.c == nil {
		return 0, false, ErrHostUnavailable
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, false, fmt.Errorf("encode payload: %w", err)
	}
	req := &pb.EnqueueRequest{Topic: topic, Payload: data}
	for _, opt := range opts {
		opt(req)
	}
	resp, err := q.c.Enqueue(outgoing(ctx), req)
	if err != nil {
		return 0, false, hostErr(err)
	}
	return resp.GetMessageId(), resp.GetDeduplicated(), nil
}

// QueueMessage is one delivery.
type QueueMessage struct {
	ID          int64
	Topic       string
	Payload     []byte
	Attempt     int
	MaxAttempts int
	// ParentTraceID is the request that enqueued this work, which is how an
	// async job stays traceable back to what triggered it.
	ParentTraceID string
}

// Decode unmarshals the payload.
func (m *QueueMessage) Decode(dest any) error { return json.Unmarshal(m.Payload, dest) }

// Consume processes messages until ctx is cancelled.
//
// A handler returning nil acknowledges the message; returning an error sends
// it back for retry, and it is dead-lettered once its attempts run out.
// ConsumeOption tunes a subscription.
type ConsumeOption func(*pb.ConsumeRequest)

// WithVisibilityTimeout bounds how long a message stays claimed by a consumer
// that has stopped responding.
//
// It is the crash-recovery latency of this topic. A replica that dies holding
// a message does not nack — there is no shutdown to trigger one — so the
// message is invisible to every other replica until this lapses. Core's
// default is thirty seconds, and measured, that is exactly what a crash costs:
// 34s to recover with maintenance sampling 150 times faster than production.
//
// The pull the other way is what makes this the plugin's decision rather than
// Core's: the timeout has to exceed the longest a handler can run, or a
// message is redelivered while it is still being worked on. Only the plugin
// knows that number — and it can be larger than the default as easily as
// smaller. A handler that waits on a lock for thirty seconds before doing two
// seconds of work needs more than thirty, not less.
func WithVisibilityTimeout(d time.Duration) ConsumeOption {
	return func(r *pb.ConsumeRequest) { r.VisibilityTimeoutSeconds = wholeSeconds(d) }
}

func (q *QueueClient) Consume(ctx context.Context, topic string, handler func(context.Context, *QueueMessage) error, opts ...ConsumeOption) error {
	if q == nil || q.c == nil {
		return ErrHostUnavailable
	}
	// Prefetch 1, and deliberately not a knob.
	//
	// prefetch is how many *unacknowledged* messages this consumer may hold,
	// and the loop below calls the handler synchronously — one message at a
	// time, per process. Asking for more would hold messages this process is
	// not working on, and a claimed message is already out of every other
	// replica's reach with its visibility clock running, so the surplus would
	// starve the siblings and get redelivered as if the handler had failed.
	//
	// The way to process more at once is another replica, not a bigger number
	// here.
	req := &pb.ConsumeRequest{Topic: topic, Prefetch: 1}
	for _, opt := range opts {
		opt(req)
	}
	stream, err := q.c.Consume(outgoing(ctx), req)
	if err != nil {
		return err
	}

	for {
		msg, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil // the caller stopped consuming
			}
			return err
		}

		m := &QueueMessage{
			ID:            msg.GetMessageId(),
			Topic:         msg.GetTopic(),
			Payload:       msg.GetPayload(),
			Attempt:       int(msg.GetAttempt()),
			MaxAttempts:   int(msg.GetMaxAttempts()),
			ParentTraceID: msg.GetParentTraceId(),
		}

		// The delivery gets its own trace, linked to the request that enqueued
		// the work, so anything the handler does is attributable to both.
		msgCtx := withTrace(ctx, msg.GetTraceId())
		err = handler(msgCtx, m)

		// Report the outcome on a context that outlives this one.
		//
		// The usual reason a handler fails is that its context was cancelled —
		// Core is draining the plugin for an upgrade — and reporting that on
		// the same cancelled context means the call fails at exactly the
		// moment it matters. The message then sits claimed until maintenance
		// reaps it, which is up to a minute in Core's defaults, so every
		// deploy strands whatever was in flight instead of handing it to the
		// new version.
		//
		// Bounded, because a plugin being drained is on a deadline of its own
		// and Core kills it when that lapses.
		outcome, cancelOutcome := context.WithTimeout(
			withTrace(context.WithoutCancel(ctx), msg.GetTraceId()), 5*time.Second)
		if err != nil {
			_, _ = q.c.Nack(outgoing(outcome), &pb.NackRequest{MessageId: m.ID, Error: err.Error()})
			cancelOutcome()
			continue
		}
		_, ackErr := q.c.Ack(outgoing(outcome), &pb.AckRequest{MessageId: m.ID})
		cancelOutcome()
		if ackErr != nil {
			return ackErr
		}
	}
}

// --- Cache and locks --------------------------------------------------------

type CacheClient struct{ c pb.HostServicesClient }

func (c *CacheClient) Get(ctx context.Context, key string, dest any) (bool, error) {
	if c == nil || c.c == nil {
		return false, ErrHostUnavailable
	}
	resp, err := c.c.CacheGet(outgoing(ctx), &pb.CacheGetRequest{Key: key})
	if err != nil || !resp.GetFound() {
		return false, err
	}
	if dest != nil {
		return true, json.Unmarshal(resp.GetValue(), dest)
	}
	return true, nil
}

func (c *CacheClient) Set(ctx context.Context, key string, value any, ttl time.Duration) error {
	if c == nil || c.c == nil {
		return ErrHostUnavailable
	}
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode value: %w", err)
	}
	_, err = c.c.CacheSet(outgoing(ctx), &pb.CacheSetRequest{
		Key: key, Value: data, TtlSeconds: wholeSeconds(ttl),
	})
	return err
}

func (c *CacheClient) Delete(ctx context.Context, key string) error {
	if c == nil || c.c == nil {
		return ErrHostUnavailable
	}
	_, err := c.c.CacheDelete(outgoing(ctx), &pb.CacheDeleteRequest{Key: key})
	return err
}

// LockClient coordinates work across a plugin's replicas.
type LockClient struct{ c pb.HostServicesClient }

// Lease is a held lock. Release it when the work is done; if the process dies
// first the lease simply expires, so a crash cannot deadlock the system.
type Lease struct {
	c       pb.HostServicesClient
	name    string
	id      string
	Expires time.Time
}

// Acquire takes a lock, waiting up to wait for it to become free. It reports
// false when the lock is held by someone else.
func (l *LockClient) Acquire(ctx context.Context, name string, ttl, wait time.Duration) (*Lease, bool, error) {
	if l == nil || l.c == nil {
		return nil, false, ErrHostUnavailable
	}
	resp, err := l.c.AcquireLock(outgoing(ctx), &pb.AcquireLockRequest{
		Name:        name,
		TtlSeconds:  wholeSeconds(ttl),
		WaitSeconds: wholeSeconds(wait),
	})
	if err != nil || !resp.GetAcquired() {
		return nil, false, err
	}
	return &Lease{
		c: l.c, name: name, id: resp.GetLeaseId(),
		Expires: time.Unix(resp.GetExpiresAtUnix(), 0),
	}, true, nil
}

// Renew extends the lease. It reports false when the lease was lost, which
// means another holder may already be doing this work — stop rather than
// continue.
func (le *Lease) Renew(ctx context.Context, ttl time.Duration) (bool, error) {
	if le == nil || le.c == nil {
		return false, ErrHostUnavailable
	}
	resp, err := le.c.RenewLock(outgoing(ctx), &pb.LeaseRequest{
		Name: le.name, LeaseId: le.id, TtlSeconds: wholeSeconds(ttl),
	})
	if err != nil {
		return false, hostErr(err)
	}
	le.Expires = time.Unix(resp.GetExpiresAtUnix(), 0)
	return resp.GetAcquired(), nil
}

// Release frees the lock.
func (le *Lease) Release(ctx context.Context) error {
	if le == nil || le.c == nil {
		return ErrHostUnavailable
	}
	_, err := le.c.ReleaseLock(outgoing(ctx), &pb.LeaseRequest{Name: le.name, LeaseId: le.id})
	return err
}

// --- Files ------------------------------------------------------------------

type FilesClient struct{ c pb.HostServicesClient }

// Put stores a file and returns its id.
func (f *FilesClient) Put(ctx context.Context, filename, mimeType string, r io.Reader) (string, int64, error) {
	if f == nil || f.c == nil {
		return "", 0, ErrHostUnavailable
	}
	stream, err := f.c.PutFile(outgoing(ctx))
	if err != nil {
		return "", 0, err
	}

	buf := make([]byte, 256<<10)
	first := true
	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			chunk := &pb.PutFileChunk{Data: buf[:n]}
			if first {
				chunk.Filename = filename
				chunk.MimeType = mimeType
				first = false
			}
			if err := stream.Send(chunk); err != nil {
				return "", 0, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", 0, readErr
		}
	}
	if first {
		// Empty file: metadata still has to reach Core.
		if err := stream.Send(&pb.PutFileChunk{Filename: filename, MimeType: mimeType}); err != nil {
			return "", 0, err
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return "", 0, err
	}
	return resp.GetFileId(), resp.GetSize(), nil
}

// DownloadURL mints a short-lived URL the browser can fetch directly, so file
// bytes never pass back through the plugin.
func (f *FilesClient) DownloadURL(ctx context.Context, fileID, userID string, expiry time.Duration) (string, time.Time, error) {
	if f == nil || f.c == nil {
		return "", time.Time{}, ErrHostUnavailable
	}
	resp, err := f.c.GenerateDownloadToken(outgoing(ctx), &pb.DownloadTokenRequest{
		FileId: fileID, UserId: userID, ExpirySeconds: wholeSeconds(expiry),
	})
	if err != nil {
		return "", time.Time{}, hostErr(err)
	}
	return resp.GetUrl(), time.Unix(resp.GetExpiresAtUnix(), 0), nil
}

// Delete removes a file this plugin created.
func (f *FilesClient) Delete(ctx context.Context, fileID string) error {
	if f == nil || f.c == nil {
		return ErrHostUnavailable
	}
	_, err := f.c.DeleteFile(outgoing(ctx), &pb.FileRequest{FileId: fileID})
	return err
}

// --- Events -----------------------------------------------------------------

// EventClient is a best-effort broadcast. A subscriber that cannot keep up
// misses events; anything that must not be lost belongs on the queue.
type EventClient struct{ c pb.HostServicesClient }

func (e *EventClient) Publish(ctx context.Context, name string, payload any) error {
	if e == nil || e.c == nil {
		return ErrHostUnavailable
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	_, err = e.c.Publish(outgoing(ctx), &pb.PublishRequest{EventName: name, Data: data})
	return err
}

// Subscribe BLOCKS, delivering events until ctx is cancelled or the stream
// fails. Run it in its own goroutine:
//
//	go func() {
//		if err := sdk.Events.Subscribe(ctx, "billing:paid", onPaid); err != nil {
//			sdk.Log.Error(ctx, "subscription ended", "err", err.Error())
//		}
//	}()
//
// It sits next to Publish, which returns immediately, and the pairing invites
// the assumption that this one registers a callback and returns too. Calling it
// inline from main hangs the plugin before it serves anything.
//
// Use "otherplugin:event" to hear from another plugin, or a bare name for this
// plugin's own events.
func (e *EventClient) Subscribe(ctx context.Context, name string, handler func(context.Context, []byte)) error {
	if e == nil || e.c == nil {
		return ErrHostUnavailable
	}
	stream, err := e.c.Subscribe(outgoing(ctx), &pb.SubscribeRequest{EventName: name})
	if err != nil {
		return err
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		handler(withTrace(ctx, ev.GetTraceId()), ev.GetData())
	}
}

// --- Outbound HTTP ----------------------------------------------------------

// HTTPClient makes outbound requests through Core, which enforces the
// egress_allow list from the manifest. A plugin has no other network access.
type HTTPClient struct{ c pb.HostServicesClient }

// Do performs a request. The body of the response is fully read.
func (h *HTTPClient) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if h == nil || h.c == nil {
		return nil, ErrHostUnavailable
	}
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
	}

	headers := make(map[string]*pb.HeaderValues, len(req.Header))
	for k, vs := range req.Header {
		headers[k] = &pb.HeaderValues{Values: vs}
	}

	resp, err := h.c.Fetch(outgoing(ctx), &pb.FetchRequest{
		Method:  req.Method,
		Url:     req.URL.String(),
		Headers: headers,
		Body:    body,
	})
	if err != nil {
		return nil, hostErr(err)
	}

	out := &http.Response{
		StatusCode: int(resp.GetStatusCode()),
		Header:     headersFrom(resp.GetHeaders()),
		Body:       io.NopCloser(bytes.NewReader(resp.GetBody())),
		Request:    req,
	}
	return out, nil
}

// Get is a convenience wrapper.
func (h *HTTPClient) Get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return h.Do(ctx, req)
}

// --- Logging ----------------------------------------------------------------

// Logger sends structured records to Core, where they are interleaved with
// Core's own log and tagged with the trace id.
//
// Use this rather than fmt.Println: anything written to stdout corrupts the
// startup handshake.
type Logger struct{ c pb.HostServicesClient }

func (l *Logger) Debug(ctx context.Context, msg string, fields ...any) {
	l.log(ctx, pb.LogLevel_LOG_DEBUG, msg, fields)
}
func (l *Logger) Info(ctx context.Context, msg string, fields ...any) {
	l.log(ctx, pb.LogLevel_LOG_INFO, msg, fields)
}
func (l *Logger) Warn(ctx context.Context, msg string, fields ...any) {
	l.log(ctx, pb.LogLevel_LOG_WARN, msg, fields)
}
func (l *Logger) Error(ctx context.Context, msg string, fields ...any) {
	l.log(ctx, pb.LogLevel_LOG_ERROR, msg, fields)
}

// log accepts fields as alternating key/value pairs.
// log turns alternating key/value pairs into a field map.
//
// Values are any, not string, because the alternative is that every call site
// converts by hand — strconv.Itoa around a count, err.Error() around an error
// — and a plugin author following the structured-logging convention every
// other Go library uses would not expect to. Anything that is not already a
// string is formatted here instead.
func (l *Logger) log(ctx context.Context, level pb.LogLevel, msg string, fields []any) {
	kv := make(map[string]string, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		kv[asLogValue(fields[i])] = asLogValue(fields[i+1])
	}

	// Unbound: no Core, so this is the plugin's own `go test`. Logging must not
	// be the thing that decides whether code is testable — see logUnbound.
	if l == nil || l.c == nil {
		logUnbound(level, msg, kv)
		return
	}

	stream, err := l.c.Log(outgoing(ctx))
	if err != nil {
		return
	}
	_ = stream.Send(&pb.LogRecord{
		Level:              level,
		Message:            msg,
		Fields:             kv,
		TraceId:            TraceID(ctx),
		TimestampUnixNanos: time.Now().UnixNano(),
	})
	_, _ = stream.CloseAndRecv()
}

// logUnbound writes a record that has nowhere else to go.
//
// The host clients are nil until Core hands over the reverse connection in
// Configure, so under `go test` every sdk.Log call has a nil receiver. Before
// this, that was a segmentation fault, and it fell on exactly the code the
// documentation tells authors is easy to test:
//
//	if !allowed {
//	    sdk.Log.Warn(ctx, "rate limit exceeded", "bucket", key)   // SIGSEGV
//	    return sdk.Stop(429, body), nil
//	}
//
// Logging when refusing a request is the ordinary thing to write, so "filters
// are ordinary Go functions and test like one" was false for most real filters.
// Whether a function can be unit-tested should not be decided by whether it
// logs.
//
// stderr rather than silence: the author is running their own tests and their
// log lines are often what they are reading. stdout would be wrong even here —
// Core reads the startup handshake from the first stdout line, and a habit that
// works in tests but corrupts start-up is worse than no output.
func logUnbound(level pb.LogLevel, msg string, fields map[string]string) {
	var b strings.Builder
	b.WriteString("[")
	b.WriteString(strings.ToLower(strings.TrimPrefix(level.String(), "LOG_")))
	b.WriteString("] ")
	b.WriteString(msg)
	// Sorted so a test asserting on this output is not at the mercy of map
	// iteration order.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(" ")
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(fields[k])
	}
	fmt.Fprintln(os.Stderr, b.String())
}

// asLogValue renders one field value. Errors use their message rather than
// their Go representation, which is what a reader of the log wants.
func asLogValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case error:
		return t.Error()
	default:
		return fmt.Sprint(v)
	}
}

// Metric records a counter increment.
//
// Named without "Counter" because counting is the common case and this is the
// name plugins already call. Gauge and Histogram are its siblings.
func (l *Logger) Metric(ctx context.Context, name string, value float64, labels map[string]string) {
	l.record(ctx, pb.MetricKind_METRIC_COUNTER, name, value, labels)
}

// Gauge records a value that goes up and down — a queue depth, a cache size,
// a number of open connections.
func (l *Logger) Gauge(ctx context.Context, name string, value float64, labels map[string]string) {
	l.record(ctx, pb.MetricKind_METRIC_GAUGE, name, value, labels)
}

// Histogram records one observation of a distribution, such as a duration.
func (l *Logger) Histogram(ctx context.Context, name string, value float64, labels map[string]string) {
	l.record(ctx, pb.MetricKind_METRIC_HISTOGRAM, name, value, labels)
}

// record sends one measurement.
//
// Core has handled all three kinds from the start and the SDK offered only the
// counter, so two of its three branches could not be reached by any plugin
// written against this package: a plugin wanting to report a queue depth had
// to report it as a counter and let whoever read the metric work out that it
// was not one.
//
// The error is dropped deliberately, and only here. A measurement that cannot
// be delivered must not take down the work being measured, and there is
// nothing a caller could usefully do about it — which is the opposite of every
// other call in this file, where the error is the answer.
func (l *Logger) record(ctx context.Context, kind pb.MetricKind, name string, value float64, labels map[string]string) {
	// Same reasoning as log: a metric call inside otherwise pure logic must not
	// be what makes that logic untestable.
	if l == nil || l.c == nil {
		return
	}
	_, _ = l.c.RecordMetric(outgoing(ctx), &pb.MetricRequest{
		Name: name, Kind: kind, Value: value, Labels: labels,
	})
}
