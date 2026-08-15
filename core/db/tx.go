package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrVersionConflict is returned when an optimistic-locking write loses.
var ErrVersionConflict = errors.New("version conflict")

// ErrUnknownTx is returned for a transaction id that is not open, has expired,
// or belongs to another plugin.
var ErrUnknownTx = errors.New("unknown or expired transaction")

// ErrTooManyTx means a plugin is at its concurrent-transaction ceiling.
//
// A limit rather than a fault: the caller should back off and try again, and
// reporting it as an internal error left a plugin author unable to tell a
// busy moment from a broken Core.
var ErrTooManyTx = errors.New("too many open transactions")

// execer is the part of *sql.DB and *sql.Tx these operations need, so the same
// code path serves both autocommit and in-transaction writes.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DefaultTxTimeout bounds how long Core will hold a transaction open on a
// plugin's behalf.
const DefaultTxTimeout = 30 * time.Second

// MaxTxTimeout is the ceiling regardless of what a plugin requests.
const MaxTxTimeout = 5 * time.Minute

// MaxOpenTxPerPlugin bounds how many transactions one plugin may hold at once.
//
// Each holds a database connection for as long as it is open, so without a
// per-plugin bound one plugin can take the whole pool and every other plugin —
// and Core's own queries — waits behind it. A cap turns "everything stops"
// into "that plugin is told no", which is both recoverable and attributable.
//
// Eight, measured rather than argued. It was four, on the reasoning that
// "four is generous for work that is supposed to be short" — which was never
// checked against a running system, and was wrong by about a third.
//
// Thirty-two concurrent transactional reservations, completed per second
// (tests/tx_throughput_test.go, MEASURE=1):
//
//	ceiling      1     2     4     8    16    25
//	spread     713   920  1257  1683  1567   716
//	contended  410   346   273   187   132   128
//
// Spread — each transaction on its own document — is what a plugin normally
// does, and it peaks at eight. Past that the pool of twenty-five starts to
// run out: at sixteen the give-ups quadruple, and at twenty-five throughput
// halves because callers wait on connections that are all held.
//
// Contended — every transaction fighting for one row — gets worse as the
// ceiling rises, because the extra concurrency turns into row-lock waiting and
// version conflicts. That is a real cost of this change and it is accepted:
// a hot row is throttled by Postgres either way, and eight of twenty-five
// connections still leaves seventeen for every other plugin and for Core.
//
// A plugin needing more than eight concurrent transactions is describing a
// batch job, and a batch job wants one transaction rather than eight.
const MaxOpenTxPerPlugin = 8

type openTx struct {
	tx        *sql.Tx
	pluginKey string
	expiresAt time.Time
}

// TxRegistry tracks transactions a plugin has open.
//
// A transaction holds a database connection, so one abandoned by a plugin that
// crashed mid-write would pin that connection forever and eventually starve
// the pool. Every transaction therefore carries a deadline and is rolled back
// when it passes, whether or not the plugin ever comes back.
type TxRegistry struct {
	mu  sync.Mutex
	txs map[string]*openTx
	// maxOpen is the per-plugin ceiling. Zero means MaxOpenTxPerPlugin.
	maxOpen int

	// open counts admitted transactions per plugin, including one that has
	// been admitted but whose BeginTx has not returned yet.
	//
	// The count used to be derived by walking txs, which put the check and the
	// insert in two separate critical sections: thirty concurrent requests all
	// counted zero, all passed a limit of four, and all opened one. Measured
	// against the inventory example, seventeen were open at once against a
	// pool of twenty-five — so the ceiling that exists to keep one plugin from
	// taking the whole pool was doing nothing at exactly the moment it was
	// needed.
	open map[string]int
	now  func() time.Time

	stopOnce sync.Once
	stop     chan struct{}
}

func NewTxRegistry() *TxRegistry {
	return &TxRegistry{
		txs:  map[string]*openTx{},
		open: map[string]int{},
		now:  time.Now,
		stop: make(chan struct{}),
	}
}

// SetClock overrides the time source. Test-only.
func (r *TxRegistry) SetClock(now func() time.Time) { r.now = now }

// SetMaxOpen overrides the per-plugin ceiling.
//
// Core does not call this. It exists so the ceiling can be measured against
// throughput rather than argued about — see the benchmark in tests/ — and so a
// deployment that has changed the pool size has somewhere to put the matching
// change. Zero restores the default.
func (r *TxRegistry) SetMaxOpen(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maxOpen = n
}

// limit reports the effective ceiling. The caller holds mu.
func (r *TxRegistry) limit() int {
	if r.maxOpen > 0 {
		return r.maxOpen
	}
	return MaxOpenTxPerPlugin
}

// StartReaper rolls back expired transactions on an interval.
func (r *TxRegistry) StartReaper(interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-ticker.C:
				r.ReapExpired()
			}
		}
	}()
}

// Close stops the reaper and rolls back everything still open.
func (r *TxRegistry) Close() {
	r.stopOnce.Do(func() { close(r.stop) })

	r.mu.Lock()
	defer r.mu.Unlock()
	for id, t := range r.txs {
		_ = t.tx.Rollback()
		r.dropLocked(id, t)
	}
}

// Begin opens a transaction owned by pluginKey.
func (r *TxRegistry) Begin(ctx context.Context, db *sql.DB, pluginKey string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = DefaultTxTimeout
	}
	if timeout > MaxTxTimeout {
		timeout = MaxTxTimeout
	}

	// Checked and reserved in one critical section, before the connection is
	// taken: a plugin over its quota is refused rather than made to wait for
	// the pool, and — because the reservation happens under the same lock as
	// the check — concurrent callers cannot all pass a limit none of them has
	// reached yet.
	r.mu.Lock()
	limit := r.limit()
	if r.open[pluginKey] >= limit {
		open := r.open[pluginKey]
		r.mu.Unlock()
		return "", fmt.Errorf("%w: plugin %s already has %d transactions open, the limit is %d; "+
			"a transaction holds a database connection, so they must be short and few",
			ErrTooManyTx, pluginKey, open, limit)
	}
	r.open[pluginKey]++
	r.mu.Unlock()

	release := func() {
		r.mu.Lock()
		if r.open[pluginKey] > 0 {
			r.open[pluginKey]--
			if r.open[pluginKey] == 0 {
				delete(r.open, pluginKey)
			}
		}
		r.mu.Unlock()
	}

	// The transaction deliberately outlives the call that opened it: a plugin
	// begins one in one RPC, writes in several more, and commits in a last
	// one. Binding it to this request's context would have database/sql roll
	// it back the moment BeginTx returns — gRPC cancels a unary call's context
	// as soon as the handler does — so every later operation would fail with
	// "transaction has already been committed or rolled back" and the whole
	// cross-RPC transaction feature would be inoperable.
	//
	// What bounds it instead is this registry: expiresAt below, enforced by
	// Lookup and by the reaper. That is the intended lifetime control, and it
	// works whether the plugin commits, crashes or simply forgets.
	tx, err := db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		release()
		return "", fmt.Errorf("begin transaction: %w", err)
	}

	id := randomTxID()
	r.mu.Lock()
	r.txs[id] = &openTx{tx: tx, pluginKey: pluginKey, expiresAt: r.now().Add(timeout)}
	r.mu.Unlock()
	return id, nil
}

// Lookup resolves a transaction id for a plugin.
//
// The plugin key is checked, not merely recorded: transaction ids are handed
// to plugins, so without this check one plugin holding another's id could
// write inside its transaction.
func (r *TxRegistry) Lookup(pluginKey, txID string) (*sql.Tx, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.txs[txID]
	if !ok || t.pluginKey != pluginKey {
		return nil, ErrUnknownTx
	}
	if r.now().After(t.expiresAt) {
		_ = t.tx.Rollback()
		r.dropLocked(txID, t)
		return nil, ErrUnknownTx
	}
	return t.tx, nil
}

// Commit finalises a transaction.
func (r *TxRegistry) Commit(pluginKey, txID string) error {
	t, err := r.take(pluginKey, txID)
	if err != nil {
		return err
	}
	if err := t.tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Rollback discards a transaction.
func (r *TxRegistry) Rollback(pluginKey, txID string) error {
	t, err := r.take(pluginKey, txID)
	if err != nil {
		return err
	}
	if err := t.tx.Rollback(); err != nil {
		return fmt.Errorf("rollback: %w", err)
	}
	return nil
}

func (r *TxRegistry) take(pluginKey, txID string) (*openTx, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	t, ok := r.txs[txID]
	if !ok || t.pluginKey != pluginKey {
		return nil, ErrUnknownTx
	}
	r.dropLocked(txID, t)
	return t, nil
}

// dropLocked removes a transaction and releases its slot. The caller holds mu.
func (r *TxRegistry) dropLocked(txID string, t *openTx) {
	delete(r.txs, txID)
	if r.open[t.pluginKey] > 0 {
		r.open[t.pluginKey]--
		if r.open[t.pluginKey] == 0 {
			delete(r.open, t.pluginKey)
		}
	}
}

// ReapExpired rolls back every transaction past its deadline.
func (r *TxRegistry) ReapExpired() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	reaped := 0
	for id, t := range r.txs {
		if now.After(t.expiresAt) {
			_ = t.tx.Rollback()
			r.dropLocked(id, t)
			reaped++
		}
	}
	return reaped
}

// Open reports how many transactions are currently held.
func (r *TxRegistry) Open() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.txs)
}

func randomTxID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("tx-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// --- versioned writes -------------------------------------------------------

// PutVersioned writes a document with optional optimistic locking.
//
// When expectedVersion is non-zero the write only applies if the stored
// version still matches, and ErrVersionConflict is returned otherwise. This is
// what lets two replicas of the same plugin update a document concurrently
// without silently overwriting each other.
func (m *CMDSManager) PutVersioned(ctx context.Context, ex execer, extKey, collection, docID string, data []byte, expectedVersion int64) (int64, error) {
	table, err := tableName(extKey, collection)
	if err != nil {
		return 0, err
	}
	if ex == nil {
		ex = m.db
	}

	if expectedVersion == 0 {
		query := fmt.Sprintf(`
			INSERT INTO %s (id, data, version) VALUES ($1, $2, 1)
			ON CONFLICT (id) DO UPDATE
			SET data = EXCLUDED.data,
			    version = %s.version + 1,
			    updated_at = CURRENT_TIMESTAMP
			RETURNING version;`, table, table)
		var version int64
		if err := ex.QueryRowContext(ctx, query, docID, data).Scan(&version); err != nil {
			return 0, fmt.Errorf("put %s/%s: %w", table, docID, err)
		}
		return version, nil
	}

	query := fmt.Sprintf(`
		UPDATE %s SET data = $2, version = version + 1, updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND version = $3
		RETURNING version;`, table)
	var version int64
	err = ex.QueryRowContext(ctx, query, docID, data, expectedVersion).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		// Either the row is gone or someone else wrote first. Both mean the
		// caller's view is stale, which is the same thing to the caller.
		return 0, ErrVersionConflict
	}
	if err != nil {
		return 0, fmt.Errorf("put %s/%s: %w", table, docID, err)
	}
	return version, nil
}

// GetVersioned reads a document along with its current version.
func (m *CMDSManager) GetVersioned(ctx context.Context, ex execer, extKey, collection, docID string) ([]byte, int64, bool, error) {
	table, err := tableName(extKey, collection)
	if err != nil {
		return nil, 0, false, err
	}
	if ex == nil {
		ex = m.db
	}

	var data []byte
	var version int64
	query := fmt.Sprintf("SELECT data, version FROM %s WHERE id = $1;", table)
	err = ex.QueryRowContext(ctx, query, docID).Scan(&data, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("get %s/%s: %w", table, docID, err)
	}
	return data, version, true, nil
}

// DeleteIn removes a document, optionally inside a transaction.
func (m *CMDSManager) DeleteIn(ctx context.Context, ex execer, extKey, collection, docID string) (bool, error) {
	table, err := tableName(extKey, collection)
	if err != nil {
		return false, err
	}
	if ex == nil {
		ex = m.db
	}

	res, err := ex.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE id = $1;", table), docID)
	if err != nil {
		return false, fmt.Errorf("delete %s/%s: %w", table, docID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil // driver does not report it; the delete itself succeeded
	}
	return n > 0, nil
}

// BatchOp is one element of a batch write.
type BatchOp struct {
	Delete     bool
	Collection string
	DocID      string
	Data       []byte
}

// BatchWrite applies several writes.
//
// Without an explicit transaction it opens its own, so a batch is all-or-
// nothing: a plugin writing a document and its index entry together cannot end
// up with one of them. With a transaction id it joins that one instead, and
// the caller decides when to commit.
func (m *CMDSManager) BatchWrite(ctx context.Context, ex execer, extKey string, ops []BatchOp) (int, error) {
	if len(ops) == 0 {
		return 0, nil
	}

	ownTx := false
	if ex == nil {
		tx, err := m.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin batch: %w", err)
		}
		defer func() {
			if ownTx {
				_ = tx.Rollback()
			}
		}()
		ownTx = true
		ex = tx

		applied, err := m.applyBatch(ctx, ex, extKey, ops)
		if err != nil {
			return 0, err
		}
		ownTx = false
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit batch: %w", err)
		}
		return applied, nil
	}

	return m.applyBatch(ctx, ex, extKey, ops)
}

func (m *CMDSManager) applyBatch(ctx context.Context, ex execer, extKey string, ops []BatchOp) (int, error) {
	applied := 0
	for i, op := range ops {
		if op.Delete {
			if _, err := m.DeleteIn(ctx, ex, extKey, op.Collection, op.DocID); err != nil {
				return applied, fmt.Errorf("batch op %d: %w", i, err)
			}
		} else {
			if _, err := m.PutVersioned(ctx, ex, extKey, op.Collection, op.DocID, op.Data, 0); err != nil {
				return applied, fmt.Errorf("batch op %d: %w", i, err)
			}
		}
		applied++
	}
	return applied, nil
}
