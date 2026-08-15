package db

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// This file extends the document store with the operations a real application
// needs: sorting, keyset pagination, aggregation, batch writes and optimistic
// locking. The original four operations (Put/Get/Delete/Find) could only
// filter one field at a time with offset paging, which pushed anything
// non-trivial into the plugin's memory.
//
// Two invariants hold throughout, and are the reason this is safe to expose to
// third-party plugins:
//
//   - Every identifier — table, column, JSONB path segment — is validated
//     against an allow-list before it reaches SQL. Nothing a plugin sends is
//     ever interpolated unchecked.
//   - Every value is a bound parameter. Operators are chosen from a fixed set,
//     never taken from input.

// Operator names accepted in a Predicate.
const (
	OpEq        = "="
	OpNe        = "!="
	OpGt        = ">"
	OpGte       = ">="
	OpLt        = "<"
	OpLte       = "<="
	OpLike      = "LIKE"
	OpIn        = "IN"
	OpBetween   = "BETWEEN"
	OpIsNull    = "IS NULL"
	OpIsNotNull = "IS NOT NULL"
)

// Predicate is one query condition. Field may be a nested JSONB path such as
// "profile.address.city".
type Predicate struct {
	Field  string
	Op     string
	Values []string
}

// SortField orders results by a JSONB field.
type SortField struct {
	Field      string
	Descending bool
}

// QueryOptions describes a read.
type QueryOptions struct {
	Predicates []Predicate
	Sort       []SortField
	Limit      int

	// Cursor continues a previous query. Keyset pagination is used rather than
	// OFFSET because OFFSET makes the database walk and discard every skipped
	// row, so deep pages get linearly slower, and rows shifting between
	// requests silently duplicate or skip entries.
	Cursor string
}

// QueryResult is one page.
type QueryResult struct {
	Documents  [][]byte
	NextCursor string
	HasMore    bool
}

// DefaultQueryLimit caps a page when the caller does not choose.
const DefaultQueryLimit = 100

// MaxQueryLimit is the ceiling Core enforces regardless of what is asked for.
const MaxQueryLimit = 1000

// jsonPath builds the text-extraction expression for a possibly nested field.
// Every segment is validated, so the result cannot carry injected SQL.
func jsonPath(field string) (string, error) {
	if field == "" {
		return "", fmt.Errorf("field is required")
	}
	parts := strings.Split(field, ".")
	for _, p := range parts {
		if err := ValidateIdentifier(p); err != nil {
			return "", fmt.Errorf("field %q: %w", field, err)
		}
	}
	if len(parts) == 1 {
		return fmt.Sprintf("(data->>'%s')", parts[0]), nil
	}
	return fmt.Sprintf("(data#>>'{%s}')", strings.Join(parts, ",")), nil
}

// buildWhere renders the predicates into a WHERE fragment plus bound args.
func buildWhere(preds []Predicate, argOffset int) (string, []any, error) {
	if len(preds) == 0 {
		return "", nil, nil
	}
	clauses := make([]string, 0, len(preds))
	args := make([]any, 0, len(preds))
	n := argOffset

	for _, p := range preds {
		expr, err := jsonPath(p.Field)
		if err != nil {
			return "", nil, err
		}
		op := strings.ToUpper(strings.TrimSpace(p.Op))
		if op == "" {
			op = OpEq
		}

		switch op {
		case OpEq, OpNe, OpGt, OpGte, OpLt, OpLte, OpLike:
			if len(p.Values) != 1 {
				return "", nil, fmt.Errorf("operator %s needs exactly one value", op)
			}
			n++
			clauses = append(clauses, fmt.Sprintf("%s %s $%d", expr, op, n))
			args = append(args, p.Values[0])

		case OpIn:
			if len(p.Values) == 0 {
				// An empty IN matches nothing. Rendering it as a false
				// constant is clearer than an SQL syntax error, and matches
				// what the caller meant.
				clauses = append(clauses, "FALSE")
				continue
			}
			placeholders := make([]string, 0, len(p.Values))
			for _, v := range p.Values {
				n++
				placeholders = append(placeholders, fmt.Sprintf("$%d", n))
				args = append(args, v)
			}
			clauses = append(clauses, fmt.Sprintf("%s IN (%s)", expr, strings.Join(placeholders, ", ")))

		case OpBetween:
			if len(p.Values) != 2 {
				return "", nil, fmt.Errorf("BETWEEN needs exactly two values")
			}
			clauses = append(clauses, fmt.Sprintf("%s BETWEEN $%d AND $%d", expr, n+1, n+2))
			args = append(args, p.Values[0], p.Values[1])
			n += 2

		case OpIsNull:
			clauses = append(clauses, fmt.Sprintf("%s IS NULL", expr))

		case OpIsNotNull:
			clauses = append(clauses, fmt.Sprintf("%s IS NOT NULL", expr))

		default:
			return "", nil, fmt.Errorf("unsupported operator %q", p.Op)
		}
	}
	return " WHERE " + strings.Join(clauses, " AND "), args, nil
}

// cursorPayload is the opaque keyset token handed back to callers.
type cursorPayload struct {
	// Keys are the sort field values of the last row returned.
	Keys []string `json:"k"`
	// ID is the primary key of the last row, breaking ties.
	ID string `json:"i"`
}

func encodeCursor(c cursorPayload) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCursor(s string) (cursorPayload, error) {
	var c cursorPayload
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("malformed cursor")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("malformed cursor")
	}
	return c, nil
}

// Query reads a page of documents with sorting and keyset pagination.
//
// Results are always ordered by id as the final key. Without a unique
// tie-breaker, rows sharing a sort value could be returned twice or skipped
// entirely across pages.
func (m *CMDSManager) Query(ctx context.Context, extKey, collection string, opts QueryOptions) (QueryResult, error) {
	table, err := tableName(extKey, collection)
	if err != nil {
		return QueryResult{}, err
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultQueryLimit
	}
	if limit > MaxQueryLimit {
		limit = MaxQueryLimit
	}

	where, args, err := buildWhere(opts.Predicates, 0)
	if err != nil {
		return QueryResult{}, err
	}

	sortExprs := make([]string, 0, len(opts.Sort))
	descending := false
	for i, s := range opts.Sort {
		expr, err := jsonPath(s.Field)
		if err != nil {
			return QueryResult{}, err
		}
		if i == 0 {
			descending = s.Descending
		} else if s.Descending != descending {
			// Row-value comparison, which is what makes the keyset cursor a
			// single cheap predicate, requires one direction for all keys.
			return QueryResult{}, fmt.Errorf(
				"cursor pagination needs every sort field in the same direction; " +
					"use a single direction or drop the cursor")
		}
		sortExprs = append(sortExprs, expr)
	}

	direction := "ASC"
	comparison := ">"
	if descending {
		direction = "DESC"
		comparison = "<"
	}

	if opts.Cursor != "" {
		cur, err := decodeCursor(opts.Cursor)
		if err != nil {
			return QueryResult{}, err
		}
		if len(cur.Keys) != len(sortExprs) {
			return QueryResult{}, fmt.Errorf("cursor does not match the current sort")
		}

		lhs := append(append([]string{}, sortExprs...), "id")
		placeholders := make([]string, 0, len(cur.Keys)+1)
		for _, k := range cur.Keys {
			args = append(args, k)
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
		args = append(args, cur.ID)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))

		clause := fmt.Sprintf("(%s) %s (%s)",
			strings.Join(lhs, ", "), comparison, strings.Join(placeholders, ", "))
		if where == "" {
			where = " WHERE " + clause
		} else {
			where += " AND " + clause
		}
	}

	orderBy := make([]string, 0, len(sortExprs)+1)
	for _, e := range sortExprs {
		orderBy = append(orderBy, e+" "+direction)
	}
	orderBy = append(orderBy, "id "+direction)

	// Fetch one extra row to learn whether another page exists, without a
	// second COUNT query.
	args = append(args, limit+1)
	query := fmt.Sprintf("SELECT id, data FROM %s%s ORDER BY %s LIMIT $%d",
		table, where, strings.Join(orderBy, ", "), len(args))

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query %s: %w", table, err)
	}
	defer rows.Close()

	var (
		result QueryResult
		lastID string
		docs   []json.RawMessage
	)
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return QueryResult{}, fmt.Errorf("scan %s: %w", table, err)
		}
		if len(result.Documents) == limit {
			result.HasMore = true
			break
		}
		result.Documents = append(result.Documents, raw)
		docs = append(docs, raw)
		lastID = id
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, fmt.Errorf("iterate %s: %w", table, err)
	}

	if result.HasMore && lastID != "" {
		keys, err := sortKeysOf(docs[len(docs)-1], opts.Sort)
		if err != nil {
			return QueryResult{}, err
		}
		result.NextCursor = encodeCursor(cursorPayload{Keys: keys, ID: lastID})
	}
	return result, nil
}

// sortKeysOf extracts the sort field values from a document so they can be
// carried in the cursor.
func sortKeysOf(doc json.RawMessage, sort []SortField) ([]string, error) {
	if len(sort) == 0 {
		return nil, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(doc, &parsed); err != nil {
		return nil, fmt.Errorf("decode document for cursor: %w", err)
	}

	keys := make([]string, 0, len(sort))
	for _, s := range sort {
		keys = append(keys, lookupPath(parsed, strings.Split(s.Field, ".")))
	}
	return keys, nil
}

// lookupPath walks a decoded document, rendering the value the same way
// PostgreSQL's ->> operator would so the cursor comparison lines up.
func lookupPath(node any, path []string) string {
	cur := node
	for _, seg := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[seg]
	}
	switch v := cur.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		// PostgreSQL renders JSON numbers without a trailing ".0".
		if v == float64(int64(v)) {
			return fmt.Sprintf("%d", int64(v))
		}
		return fmt.Sprintf("%v", v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

// Aggregate function names.
const (
	AggCount = "COUNT"
	AggSum   = "SUM"
	AggAvg   = "AVG"
	AggMin   = "MIN"
	AggMax   = "MAX"
)

// AggregateOptions describes an aggregation.
type AggregateOptions struct {
	Predicates []Predicate
	Func       string
	Field      string // ignored for COUNT
	GroupBy    []string
}

// AggregateBucket is one group's result.
type AggregateBucket struct {
	Keys  map[string]string
	Value float64
}

// Aggregate runs a grouped aggregation, so a plugin can count or total rows
// without pulling them all into its own memory.
func (m *CMDSManager) Aggregate(ctx context.Context, extKey, collection string, opts AggregateOptions) ([]AggregateBucket, error) {
	table, err := tableName(extKey, collection)
	if err != nil {
		return nil, err
	}

	fn := strings.ToUpper(strings.TrimSpace(opts.Func))
	switch fn {
	case AggCount, AggSum, AggAvg, AggMin, AggMax:
	case "":
		fn = AggCount
	default:
		return nil, fmt.Errorf("unsupported aggregate function %q", opts.Func)
	}

	expr := "*"
	if fn != AggCount {
		path, err := jsonPath(opts.Field)
		if err != nil {
			return nil, fmt.Errorf("aggregate field: %w", err)
		}
		// Numeric aggregates need a cast; JSONB text extraction yields text.
		expr = fmt.Sprintf("(%s)::numeric", path)
	}

	groupExprs := make([]string, 0, len(opts.GroupBy))
	for _, g := range opts.GroupBy {
		path, err := jsonPath(g)
		if err != nil {
			return nil, fmt.Errorf("group by: %w", err)
		}
		groupExprs = append(groupExprs, path)
	}

	where, args, err := buildWhere(opts.Predicates, 0)
	if err != nil {
		return nil, err
	}

	selected := make([]string, 0, len(groupExprs)+1)
	selected = append(selected, groupExprs...)
	selected = append(selected, fmt.Sprintf("%s(%s)::float8", fn, expr))

	query := fmt.Sprintf("SELECT %s FROM %s%s", strings.Join(selected, ", "), table, where)
	if len(groupExprs) > 0 {
		query += " GROUP BY " + strings.Join(groupExprs, ", ")
	}

	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("aggregate %s: %w", table, err)
	}
	defer rows.Close()

	var buckets []AggregateBucket
	for rows.Next() {
		scanTargets := make([]any, 0, len(groupExprs)+1)
		groupVals := make([]sql.NullString, len(groupExprs))
		for i := range groupVals {
			scanTargets = append(scanTargets, &groupVals[i])
		}
		var value sql.NullFloat64
		scanTargets = append(scanTargets, &value)

		if err := rows.Scan(scanTargets...); err != nil {
			return nil, fmt.Errorf("scan aggregate: %w", err)
		}

		bucket := AggregateBucket{Value: value.Float64}
		if len(groupExprs) > 0 {
			bucket.Keys = make(map[string]string, len(groupExprs))
			for i, g := range opts.GroupBy {
				bucket.Keys[g] = groupVals[i].String
			}
		}
		buckets = append(buckets, bucket)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate aggregate: %w", err)
	}
	return buckets, nil
}
