package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrDuplicateMessage is returned when a dedup key matches a message that is
// still pending or processing.
var ErrDuplicateMessage = errors.New("duplicate message")

// Queue is an at-least-once durable queue backed by PostgreSQL.
//
// PostgreSQL rather than a dedicated broker because Core already depends on it:
// adding Redis or RabbitMQ would put a second stateful service in every
// deployment for a workload that is measured in hundreds of messages per
// second, not millions. SELECT ... FOR UPDATE SKIP LOCKED gives concurrent
// consumers without them contending on the same rows.
//
// Delivery is at-least-once, not exactly-once: a consumer that finishes its
// work and then dies before acknowledging will see the message again. Handlers
// must therefore be idempotent, which is a property worth designing for anyway.
type Queue struct {
	db  *sql.DB
	now func() time.Time
}

func NewQueue(db *sql.DB) *Queue {
	return &Queue{db: db, now: time.Now}
}

// SetClock overrides the time source. Test-only.
func (q *Queue) SetClock(now func() time.Time) { q.now = now }

// Message is one delivery attempt.
type Message struct {
	ID            int64
	Topic         string
	Payload       []byte
	Attempt       int
	MaxAttempts   int
	ParentTraceID string
}

// EnqueueOptions tunes a single publish.
type EnqueueOptions struct {
	Delay       time.Duration
	DedupKey    string
	Priority    int
	MaxAttempts int
	TraceID     string
}

// DefaultMaxAttempts is how many times a message is retried before it is
// parked as dead.
const DefaultMaxAttempts = 5

// Enqueue adds a message. When a dedup key matches something still in flight
// the existing message is kept and deduplicated is reported, rather than
// erroring: the caller's intent — "make sure this work happens once" — is
// already satisfied.
func (q *Queue) Enqueue(ctx context.Context, ownerKey, topic string, payload []byte, opts EnqueueOptions) (id int64, deduplicated bool, err error) {
	if topic == "" {
		return 0, false, fmt.Errorf("topic is required")
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	availableAt := q.now().Add(opts.Delay)

	var dedup any
	if opts.DedupKey != "" {
		dedup = opts.DedupKey
	}

	// ON CONFLICT DO NOTHING against the partial unique index makes
	// deduplication a property of the database rather than a check-then-insert
	// race between two replicas.
	const query = `
		INSERT INTO plugin_queue
			(owner_key, topic, payload, priority, max_attempts, available_at, dedup_key, parent_trace_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT DO NOTHING
		RETURNING id;`

	err = q.db.QueryRowContext(ctx, query,
		ownerKey, topic, payload, opts.Priority, maxAttempts, availableAt, dedup, opts.TraceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("enqueue: %w", err)
	}
	return id, false, nil
}

// Claim leases up to limit messages, making them invisible to other consumers
// until visibility elapses.
//
// The whole claim is one statement. Selecting and then updating in two steps
// would let two consumers claim the same row between them; doing it in a CTE
// with SKIP LOCKED means the database decides, and concurrent consumers simply
// step over each other's rows.
func (q *Queue) Claim(ctx context.Context, ownerKey, topic string, limit int, visibility time.Duration) ([]Message, error) {
	if limit <= 0 {
		limit = 1
	}
	if visibility <= 0 {
		visibility = 30 * time.Second
	}
	now := q.now()

	const query = `
		WITH claimed AS (
			SELECT id FROM plugin_queue
			WHERE owner_key = $1
			  AND topic = $2
			  AND status = 'pending'
			  AND available_at <= $3
			ORDER BY priority DESC, id
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		UPDATE plugin_queue q
		SET status = 'processing',
		    attempts = q.attempts + 1,
		    locked_until = $5,
		    updated_at = CURRENT_TIMESTAMP
		FROM claimed
		WHERE q.id = claimed.id
		RETURNING q.id, q.topic, q.payload, q.attempts, q.max_attempts, COALESCE(q.parent_trace_id, '');`

	rows, err := q.db.QueryContext(ctx, query, ownerKey, topic, now, limit, now.Add(visibility))
	if err != nil {
		return nil, fmt.Errorf("claim: %w", err)
	}
	defer rows.Close()

	var out []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.Topic, &m.Payload, &m.Attempt, &m.MaxAttempts, &m.ParentTraceID); err != nil {
			return nil, fmt.Errorf("scan claimed message: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Ack marks a message done. The owner key is part of the predicate so one
// plugin cannot acknowledge another's message by id.
func (q *Queue) Ack(ctx context.Context, ownerKey string, id int64) error {
	const query = `
		UPDATE plugin_queue
		SET status = 'done', locked_until = NULL, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND owner_key = $2 AND status = 'processing';`

	res, err := q.db.ExecContext(ctx, query, id, ownerKey)
	if err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Either it was already acknowledged, its lease expired and it was
		// redelivered, or it belongs to someone else. None of these should
		// fail the caller: the work is done either way, and reporting an error
		// would push handlers toward retrying work they already completed.
		return nil
	}
	return nil
}

// Nack returns a message for retry, or parks it as dead once its attempts are
// exhausted.
func (q *Queue) Nack(ctx context.Context, ownerKey string, id int64, reason string, retryAfter time.Duration) error {
	if retryAfter <= 0 {
		retryAfter = 0
	}
	const query = `
		UPDATE plugin_queue
		SET status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
		    available_at = $3,
		    locked_until = NULL,
		    last_error = $4,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND owner_key = $2 AND status = 'processing';`

	if _, err := q.db.ExecContext(ctx, query, id, ownerKey, q.now().Add(retryAfter), reason); err != nil {
		return fmt.Errorf("nack: %w", err)
	}
	return nil
}

// PendingDepth counts the messages waiting for each plugin.
//
// Grouped in one pass rather than queried per plugin: this runs on a timer for
// every plugin at once, and a per-plugin query would turn one scan into as
// many scans as there are plugins.
func (q *Queue) PendingDepth(ctx context.Context) (map[string]int64, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT owner_key, count(*) FROM plugin_queue
		  WHERE status IN ('pending', 'processing')
		  GROUP BY owner_key`)
	if err != nil {
		return nil, fmt.Errorf("queue depth: %w", err)
	}
	defer rows.Close()

	out := map[string]int64{}
	for rows.Next() {
		var key string
		var n int64
		if err := rows.Scan(&key, &n); err != nil {
			return nil, fmt.Errorf("scan queue depth: %w", err)
		}
		out[key] = n
	}
	return out, rows.Err()
}

// ReapExpired returns messages whose visibility timeout lapsed to the pending
// pool, so a consumer that crashed mid-handler does not strand its work.
func (q *Queue) ReapExpired(ctx context.Context) (int64, error) {
	const query = `
		UPDATE plugin_queue
		SET status = CASE WHEN attempts >= max_attempts THEN 'dead' ELSE 'pending' END,
		    locked_until = NULL,
		    last_error = COALESCE(last_error, 'visibility timeout expired'),
		    updated_at = CURRENT_TIMESTAMP
		WHERE status = 'processing' AND locked_until IS NOT NULL AND locked_until < $1;`

	res, err := q.db.ExecContext(ctx, query, q.now())
	if err != nil {
		return 0, fmt.Errorf("reap: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// QueueStats summarises a topic, for the console and for tests.
type QueueStats struct {
	Pending    int64
	Processing int64
	Done       int64
	Dead       int64
}

// Stats counts messages by status.
func (q *Queue) Stats(ctx context.Context, ownerKey, topic string) (QueueStats, error) {
	const query = `
		SELECT status, COUNT(*) FROM plugin_queue
		WHERE owner_key = $1 AND topic = $2
		GROUP BY status;`

	rows, err := q.db.QueryContext(ctx, query, ownerKey, topic)
	if err != nil {
		return QueueStats{}, fmt.Errorf("queue stats: %w", err)
	}
	defer rows.Close()

	var s QueueStats
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return QueueStats{}, err
		}
		switch status {
		case "pending":
			s.Pending = n
		case "processing":
			s.Processing = n
		case "done":
			s.Done = n
		case "dead":
			s.Dead = n
		}
	}
	return s, rows.Err()
}

// PurgeDone deletes completed messages older than the cutoff, so the table does
// not grow without bound.
func (q *Queue) PurgeDone(ctx context.Context, olderThan time.Duration) (int64, error) {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM plugin_queue WHERE status = 'done' AND updated_at < $1;`,
		q.now().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("purge: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
