package db

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// identifierRE guards every dynamically-built SQL identifier. Extension keys,
// collection names and field names all originate from manifests, so we still
// validate to make table/index/JSON-path construction injection-proof.
var identifierRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)

// ValidateIdentifier ensures a name is a safe SQL/JSON identifier.
func ValidateIdentifier(name string) error {
	if !identifierRE.MatchString(name) {
		return fmt.Errorf("invalid identifier %q: must match [a-zA-Z][a-zA-Z0-9_]*", name)
	}
	return nil
}

// Filter expresses a single CMDS query predicate against a JSONB field.
type Filter struct {
	Field    string
	Operator string // "=", ">", "<", "LIKE"
	Value    string
}

// Index declares a (possibly composite, possibly unique) JSONB index.
type Index struct {
	Fields []string
	Unique bool
}

// CollectionSchema declares one document collection and its indexes.
type CollectionSchema struct {
	Name    string
	Indexes []Index
}

// CMDSManager is the Core-Managed Document Store backed by PostgreSQL JSONB.
type CMDSManager struct {
	db *sql.DB
}

func NewCMDSManager(db *sql.DB) *CMDSManager {
	return &CMDSManager{db: db}
}

// tableName builds and validates the physically-isolated table name.
func tableName(extKey, collection string) (string, error) {
	if err := ValidateIdentifier(extKey); err != nil {
		return "", fmt.Errorf("extension key: %w", err)
	}
	if err := ValidateIdentifier(collection); err != nil {
		return "", fmt.Errorf("collection: %w", err)
	}
	return fmt.Sprintf("ext_%s_%s", extKey, collection), nil
}

func indexName(extKey, collection string, fields []string) string {
	return fmt.Sprintf("idx_%s_%s_%s", extKey, collection, strings.Join(fields, "_"))
}

// jsonPathExpr builds the immutable text expression used in index/query.
func jsonPathExpr(field string) (string, error) {
	if err := ValidateIdentifier(field); err != nil {
		return "", fmt.Errorf("field: %w", err)
	}
	return fmt.Sprintf("(data->>'%s')", field), nil
}

// ReconcileSchema provisions tables, creates declared indexes and drops stale
// ones (those carrying our naming prefix but no longer declared).
func (m *CMDSManager) ReconcileSchema(extKey string, collections []CollectionSchema) error {
	for _, col := range collections {
		table, err := tableName(extKey, col.Name)
		if err != nil {
			return err
		}

		createSQL := fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s (
				id VARCHAR(100) PRIMARY KEY,
				data JSONB NOT NULL,
				version BIGINT NOT NULL DEFAULT 1,
				created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
			);`, table)
		if _, err := m.db.Exec(createSQL); err != nil {
			return fmt.Errorf("create table %s: %w", table, err)
		}

		// Tables provisioned before optimistic locking existed have no version
		// column. Adding it here rather than in a numbered migration is
		// deliberate: these tables are created on demand per plugin, so a
		// static migration could not know their names.
		alterSQL := fmt.Sprintf(
			`ALTER TABLE %s ADD COLUMN IF NOT EXISTS version BIGINT NOT NULL DEFAULT 1;`, table)
		if _, err := m.db.Exec(alterSQL); err != nil {
			return fmt.Errorf("add version column to %s: %w", table, err)
		}

		desired := make(map[string]struct{})
		for _, idx := range col.Indexes {
			if len(idx.Fields) == 0 {
				continue
			}
			exprs := make([]string, 0, len(idx.Fields))
			for _, f := range idx.Fields {
				expr, err := jsonPathExpr(f)
				if err != nil {
					return err
				}
				exprs = append(exprs, expr)
			}
			name := indexName(extKey, col.Name, idx.Fields)
			desired[name] = struct{}{}

			unique := ""
			if idx.Unique {
				unique = "UNIQUE "
			}
			idxSQL := fmt.Sprintf(`CREATE %sINDEX IF NOT EXISTS %s ON %s (%s);`,
				unique, name, table, strings.Join(exprs, ", "))
			if _, err := m.db.Exec(idxSQL); err != nil {
				return fmt.Errorf("create index %s: %w", name, err)
			}
		}

		if err := m.dropStaleIndexes(extKey, col.Name, table, desired); err != nil {
			return err
		}
	}
	return nil
}

// dropStaleIndexes removes indexes prefixed for this collection that are no
// longer declared in the manifest.
func (m *CMDSManager) dropStaleIndexes(extKey, collection, table string, desired map[string]struct{}) error {
	prefix := fmt.Sprintf("idx_%s_%s_", extKey, collection)
	rows, err := m.db.Query(
		`SELECT indexname FROM pg_indexes WHERE tablename = $1 AND indexname LIKE $2;`,
		table, prefix+"%",
	)
	if err != nil {
		return fmt.Errorf("list indexes for %s: %w", table, err)
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if _, ok := desired[name]; !ok {
			stale = append(stale, name)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, name := range stale {
		if !identifierRE.MatchString(name) {
			continue // defensive: never DROP an unexpected identifier
		}
		if _, err := m.db.Exec(fmt.Sprintf(`DROP INDEX IF EXISTS %s;`, name)); err != nil {
			return fmt.Errorf("drop stale index %s: %w", name, err)
		}
	}
	return nil
}

// Put upserts a document.
func (m *CMDSManager) Put(extKey, collection, docID string, data []byte) error {
	table, err := tableName(extKey, collection)
	if err != nil {
		return err
	}
	query := fmt.Sprintf(`
		INSERT INTO %s (id, data, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (id)
		DO UPDATE SET data = EXCLUDED.data, updated_at = NOW();`, table)
	if _, err := m.db.Exec(query, docID, data); err != nil {
		return fmt.Errorf("put %s/%s: %w", collection, docID, err)
	}
	return nil
}

// Get returns a document's JSON bytes; ok is false when not found.
func (m *CMDSManager) Get(extKey, collection, docID string) ([]byte, bool, error) {
	table, err := tableName(extKey, collection)
	if err != nil {
		return nil, false, err
	}
	query := fmt.Sprintf(`SELECT data FROM %s WHERE id = $1;`, table)
	var data []byte
	err = m.db.QueryRow(query, docID).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get %s/%s: %w", collection, docID, err)
	}
	return data, true, nil
}

// Delete removes a document by id.
func (m *CMDSManager) Delete(extKey, collection, docID string) error {
	table, err := tableName(extKey, collection)
	if err != nil {
		return err
	}
	if _, err := m.db.Exec(fmt.Sprintf(`DELETE FROM %s WHERE id = $1;`, table), docID); err != nil {
		return fmt.Errorf("delete %s/%s: %w", collection, docID, err)
	}
	return nil
}

// Find returns documents matching all filters with pagination.
func (m *CMDSManager) Find(ctx context.Context, ex execer, extKey, collection string, filters []Filter, limit, offset int) ([][]byte, error) {
	table, err := tableName(extKey, collection)
	if err != nil {
		return nil, err
	}
	if ex == nil {
		ex = m.db
	}

	var whereClauses []string
	var args []interface{}
	argIdx := 1

	for _, f := range filters {
		expr, err := jsonPathExpr(f.Field)
		if err != nil {
			return nil, err
		}
		op := normalizeOperator(f.Operator)
		whereClauses = append(whereClauses, fmt.Sprintf("%s %s $%d", expr, op, argIdx))
		args = append(args, f.Value)
		argIdx++
	}

	where := ""
	if len(whereClauses) > 0 {
		where = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}

	query := fmt.Sprintf(`SELECT data FROM %s %s ORDER BY created_at LIMIT $%d OFFSET $%d;`,
		table, where, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := ex.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("find %s: %w", collection, err)
	}
	defer rows.Close()

	results := make([][]byte, 0)
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, data)
	}
	return results, rows.Err()
}

// normalizeOperator whitelists allowed operators, defaulting to equality.
func normalizeOperator(op string) string {
	switch op {
	case ">", "<", ">=", "<=", "LIKE", "!=":
		return op
	default:
		return "="
	}
}
