package hostsvc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/taills/moduless/core/db"
)

// CMDSData adapts the Core-Managed Document Store to the DataBackend
// interface, adding the transaction plumbing.
//
// Every method takes the plugin key from its caller rather than from the
// request, and the key becomes part of the physical table name, so a plugin
// cannot reach another plugin's collections even by guessing their names.
type CMDSData struct {
	conn *sql.DB
	cmds *db.CMDSManager
	txs  *db.TxRegistry
}

func NewCMDSData(conn *sql.DB, cmds *db.CMDSManager, txs *db.TxRegistry) *CMDSData {
	return &CMDSData{conn: conn, cmds: cmds, txs: txs}
}

// execFor returns the executor for a transaction id. An empty id means
// autocommit, and yields a nil executor the store reads as "use the pool".
func (d *CMDSData) execFor(pluginKey, txID string) (dbExecer, error) {
	if txID == "" {
		return nil, nil
	}
	tx, err := d.txs.Lookup(pluginKey, txID)
	if err != nil {
		return nil, err
	}
	return tx, nil
}

// dbExecer mirrors the store's internal executor so a *sql.Tx can be passed
// through without this package importing database/sql details of its own.
type dbExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (d *CMDSData) Put(ctx context.Context, pluginKey, collection, docID string, data []byte, txID string, expectedVersion int64) (int64, error) {
	ex, err := d.execFor(pluginKey, txID)
	if err != nil {
		return 0, err
	}
	version, err := d.cmds.PutVersioned(ctx, ex, pluginKey, collection, docID, data, expectedVersion)
	if errors.Is(err, db.ErrVersionConflict) {
		return 0, err
	}
	return version, err
}

func (d *CMDSData) Get(ctx context.Context, pluginKey, collection, docID, txID string) ([]byte, int64, bool, error) {
	ex, err := d.execFor(pluginKey, txID)
	if err != nil {
		return nil, 0, false, err
	}
	return d.cmds.GetVersioned(ctx, ex, pluginKey, collection, docID)
}

func (d *CMDSData) Delete(ctx context.Context, pluginKey, collection, docID, txID string) (bool, error) {
	ex, err := d.execFor(pluginKey, txID)
	if err != nil {
		return false, err
	}
	return d.cmds.DeleteIn(ctx, ex, pluginKey, collection, docID)
}

func (d *CMDSData) Find(ctx context.Context, pluginKey, collection string, filters []Filter, limit, offset int, txID string) ([][]byte, error) {
	// Find keeps offset paging for source compatibility. Query is the one to
	// reach for in new code: offset makes the database walk and discard every
	// skipped row, so deep pages get slower, and rows shifting between
	// requests silently duplicate or skip entries.
	ex, err := d.execFor(pluginKey, txID)
	if err != nil {
		return nil, err
	}
	legacy := make([]db.Filter, 0, len(filters))
	for _, f := range filters {
		value := ""
		if len(f.Values) > 0 {
			value = f.Values[0]
		}
		legacy = append(legacy, db.Filter{Field: f.Field, Operator: f.Op, Value: value})
	}
	return d.cmds.Find(ctx, ex, pluginKey, collection, legacy, limit, offset)
}

func (d *CMDSData) Query(ctx context.Context, pluginKey, collection string, opts QueryOptions, txID string) (QueryResult, error) {
	// Reads run inside the caller's transaction when it gave one. Without
	// this, a plugin that writes a document and then queries for it in the
	// same transaction does not find it — the write is uncommitted and the
	// query is running on a different connection. Nothing reports an error;
	// the plugin simply gets an answer that is wrong.
	ex, err := d.execFor(pluginKey, txID)
	if err != nil {
		return QueryResult{}, err
	}

	preds := make([]db.Predicate, 0, len(opts.Filters))
	for _, f := range opts.Filters {
		preds = append(preds, db.Predicate{Field: f.Field, Op: f.Op, Values: f.Values})
	}
	sorts := make([]db.SortField, 0, len(opts.Sort))
	for _, s := range opts.Sort {
		sorts = append(sorts, db.SortField{Field: s.Field, Descending: s.Descending})
	}

	res, err := d.cmds.Query(ctx, ex, pluginKey, collection, db.QueryOptions{
		Predicates: preds,
		Sort:       sorts,
		Limit:      opts.Limit,
		Cursor:     opts.Cursor,
	})
	if err != nil {
		return QueryResult{}, err
	}
	return QueryResult{
		Documents:  res.Documents,
		NextCursor: res.NextCursor,
		HasMore:    res.HasMore,
	}, nil
}

func (d *CMDSData) Aggregate(ctx context.Context, pluginKey, collection string, opts AggregateOptions, txID string) ([]AggregateBucket, error) {
	ex, err := d.execFor(pluginKey, txID)
	if err != nil {
		return nil, err
	}

	preds := make([]db.Predicate, 0, len(opts.Filters))
	for _, f := range opts.Filters {
		preds = append(preds, db.Predicate{Field: f.Field, Op: f.Op, Values: f.Values})
	}

	buckets, err := d.cmds.Aggregate(ctx, ex, pluginKey, collection, db.AggregateOptions{
		Predicates: preds,
		Func:       opts.Func,
		Field:      opts.Field,
		GroupBy:    opts.GroupBy,
	})
	if err != nil {
		return nil, err
	}

	out := make([]AggregateBucket, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, AggregateBucket{Keys: b.Keys, Value: b.Value})
	}
	return out, nil
}

func (d *CMDSData) BatchWrite(ctx context.Context, pluginKey string, ops []WriteOp, txID string) (int, error) {
	ex, err := d.execFor(pluginKey, txID)
	if err != nil {
		return 0, err
	}
	batch := make([]db.BatchOp, 0, len(ops))
	for _, op := range ops {
		batch = append(batch, db.BatchOp{
			Delete:     op.Delete,
			Collection: op.Collection,
			DocID:      op.DocID,
			Data:       op.Data,
		})
	}
	return d.cmds.BatchWrite(ctx, ex, pluginKey, batch)
}

func (d *CMDSData) BeginTx(ctx context.Context, pluginKey string, timeout time.Duration) (string, error) {
	return d.txs.Begin(ctx, d.conn, pluginKey, timeout)
}

func (d *CMDSData) CommitTx(ctx context.Context, pluginKey, txID string) error {
	return d.txs.Commit(pluginKey, txID)
}

func (d *CMDSData) RollbackTx(ctx context.Context, pluginKey, txID string) error {
	return d.txs.Rollback(pluginKey, txID)
}

// ProvisionSchema creates the collections a plugin declared.
func (d *CMDSData) ProvisionSchema(pluginKey string, collections []db.CollectionSchema) error {
	if len(collections) == 0 {
		return nil
	}
	if err := d.cmds.ReconcileSchema(pluginKey, collections); err != nil {
		return fmt.Errorf("provision schema for %s: %w", pluginKey, err)
	}
	return nil
}
