package hostsvc

import (
	"context"
	"io"
	"time"
)

// The backends below are the seams between the gRPC surface plugins see and
// Core's own subsystems. Keeping them as interfaces means the permission
// checks and wire translation can be tested without a database, an object
// store, or a network.
//
// A nil backend makes its capability report Unavailable, which is how Core
// runs in configurations without PostgreSQL or object storage.

// --- Data -------------------------------------------------------------------

// Filter is one query predicate. Field is a JSONB path; Core validates every
// segment against an identifier allow-list before it reaches SQL.
type Filter struct {
	Field  string
	Op     string
	Values []string
}

type Sort struct {
	Field      string
	Descending bool
}

type QueryOptions struct {
	Filters []Filter
	Sort    []Sort
	Limit   int
	Cursor  string
}

type QueryResult struct {
	Documents  [][]byte
	NextCursor string
	HasMore    bool
}

type AggregateOptions struct {
	Filters []Filter
	Func    string
	Field   string
	GroupBy []string
}

type AggregateBucket struct {
	Keys  map[string]string
	Value float64
}

// WriteOp is one element of a batch.
type WriteOp struct {
	Delete     bool
	Collection string
	DocID      string
	Data       []byte
}

// DataBackend is the Core-managed document store. Every method takes the
// plugin key explicitly rather than reading it from context, so a caller
// cannot omit it by accident.
type DataBackend interface {
	Put(ctx context.Context, pluginKey, collection, docID string, data []byte, txID string, expectedVersion int64) (int64, error)
	Get(ctx context.Context, pluginKey, collection, docID, txID string) (data []byte, version int64, found bool, err error)
	Delete(ctx context.Context, pluginKey, collection, docID, txID string) (bool, error)
	Find(ctx context.Context, pluginKey, collection string, filters []Filter, limit, offset int, txID string) ([][]byte, error)
	Query(ctx context.Context, pluginKey, collection string, opts QueryOptions, txID string) (QueryResult, error)
	Aggregate(ctx context.Context, pluginKey, collection string, opts AggregateOptions, txID string) ([]AggregateBucket, error)
	BatchWrite(ctx context.Context, pluginKey string, ops []WriteOp, txID string) (int, error)

	BeginTx(ctx context.Context, pluginKey string, timeout time.Duration) (txID string, err error)
	CommitTx(ctx context.Context, pluginKey, txID string) error
	RollbackTx(ctx context.Context, pluginKey, txID string) error
}

// --- Queue ------------------------------------------------------------------

type EnqueueOptions struct {
	Delay       time.Duration
	DedupKey    string
	Priority    int
	MaxAttempts int
	TraceID     string
}

// Message is one delivery attempt.
type Message struct {
	ID            int64
	Topic         string
	Payload       []byte
	Attempt       int
	MaxAttempts   int
	ParentTraceID string
}

// QueueBackend is a durable, at-least-once queue. Consume blocks, delivering
// messages until the context is cancelled.
type QueueBackend interface {
	Enqueue(ctx context.Context, pluginKey, topic string, payload []byte, opts EnqueueOptions) (id int64, deduplicated bool, err error)
	Consume(ctx context.Context, pluginKey, topic string, prefetch int, visibility time.Duration, deliver func(Message) error) error
	Ack(ctx context.Context, pluginKey string, id int64) error
	Nack(ctx context.Context, pluginKey string, id int64, reason string, retryAfter time.Duration) error
}

// --- Cache and locks --------------------------------------------------------

type CacheBackend interface {
	Get(pluginKey, key string) ([]byte, bool)
	Set(pluginKey, key string, value []byte, ttl time.Duration)
	Delete(pluginKey, key string)
}

// Lease identifies one holder of a lock. Presenting it on renew and release is
// what stops a process that already lost its lease on timeout from releasing
// the lock the next holder now owns.
type Lease struct {
	ID        string
	ExpiresAt time.Time
}

type LockBackend interface {
	Acquire(ctx context.Context, pluginKey, name string, ttl, wait time.Duration) (Lease, bool, error)
	Renew(pluginKey, name, leaseID string, ttl time.Duration) (Lease, bool)
	Release(pluginKey, name, leaseID string)
}

// --- Config -----------------------------------------------------------------

// ConfigBackend serves the admin-managed settings for a plugin.
type ConfigBackend interface {
	Get(ctx context.Context, pluginKey string) (map[string]string, error)
}

// --- Files ------------------------------------------------------------------

type FileMeta struct {
	FileID   string
	Filename string
	Size     int64
	MimeType string
	Found    bool
}

type FileBackend interface {
	Put(ctx context.Context, pluginKey, filename, mimeType string, r io.Reader) (fileID string, size int64, err error)
	Delete(ctx context.Context, pluginKey, fileID string) error
	GenerateDownloadToken(ctx context.Context, pluginKey, fileID, userID string, expiry time.Duration) (url string, expiresAt time.Time, err error)
	Metadata(ctx context.Context, pluginKey, fileID string) (FileMeta, error)
}

// --- Events -----------------------------------------------------------------

// Event is a best-effort broadcast between plugins. Anything that must not be
// lost belongs on the durable queue instead.
type Event struct {
	Name            string
	Data            []byte
	SourcePluginKey string
	TraceID         string
}

type EventBackend interface {
	Publish(pluginKey string, ev Event) error
	Subscribe(ctx context.Context, pluginKey, eventName string, deliver func(Event) error) error
}

// --- Outbound HTTP ----------------------------------------------------------

type EgressRequest struct {
	Method  string
	URL     string
	Headers map[string][]string
	Body    []byte
	Timeout time.Duration
	TraceID string
}

type EgressResponse struct {
	StatusCode int
	Headers    map[string][]string
	Body       []byte
}

// EgressBackend proxies outbound HTTP on a plugin's behalf, enforcing the
// hostname allow-list from its manifest. Plugins have no direct network
// access, so this is the only way out.
type EgressBackend interface {
	Fetch(ctx context.Context, pluginKey string, req EgressRequest) (EgressResponse, error)
}

// --- Observability ----------------------------------------------------------

type LogRecord struct {
	Level     string
	Message   string
	Fields    map[string]string
	TraceID   string
	Timestamp time.Time
}

type Metric struct {
	Name   string
	Kind   string
	Value  float64
	Labels map[string]string
}

type ObservabilityBackend interface {
	Log(pluginKey string, rec LogRecord)
	RecordMetric(pluginKey string, m Metric)
}
