# Core-Managed Document Store (CMDS) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the Core-Managed Document Store (CMDS) in Core, providing schemaless JSONB document storage, dynamic table/index generation, declarative database migrations, and type-safe database SDK APIs for Go.

**Architecture:** Core acts as the centralized database gateway, managing PostgreSQL 18 connection pools. When extensions register, Core reads `manifest.yaml` to dynamically provision tables (`ext_<extKey>_<collection>`) and apply declarative JSONB migrations. Extensions read/write via Go SDK's gRPC client mapping to `DatabaseService`.

**Tech Stack:** Go, PostgreSQL 18, sqlc, go-migrate, gRPC.

## Global Constraints

- PostgreSQL 18
- Extensions have no direct DB credentials or connections. All queries tunnel through gRPC.
- Strict data isolation: Extensions can only access tables prefixed with `ext_<extension_key>_`.

---

### Task 1: DB Migration Setup & Connection Pool (Core)

Configure go-migrate to initialize core system tables (users, roles, migrations tracking) and setup sqlc for type-safe system DB access.

**Files:**
- Create: `db/migrations/000001_init_system.up.sql`
- Create: `db/migrations/000001_init_system.down.sql`
- Create: `db/sqlc.yaml`
- Create: `db/query.sql`
- Create: `core/db/postgres.go`

**Interfaces:**
- Produces: `core/db.InitDB(connStr string) (*sql.DB, error)`

- [ ] **Step 1: Create PostgreSQL Migration files**

Create `db/migrations/000001_init_system.up.sql`:
```sql
CREATE TABLE IF NOT EXISTS system_users (
    id SERIAL PRIMARY KEY,
    username VARCHAR(50) UNIQUE NOT NULL,
    password_hash VARCHAR(255) NOT NULL,
    role VARCHAR(50) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS extension_versions (
    extension_key VARCHAR(100) PRIMARY KEY,
    version VARCHAR(50) NOT NULL,
    updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);
```

Create `db/migrations/000001_init_system.down.sql`:
```sql
DROP TABLE IF EXISTS extension_versions;
DROP TABLE IF EXISTS system_users;
```

- [ ] **Step 2: Create `db/sqlc.yaml` for compiling code**

```yaml
version: "2"
sql:
  - schema: "db/migrations"
    queries: "db/query.sql"
    gen:
      go:
        package: "db"
        out: "core/db/sqlc"
```

- [ ] **Step 3: Create `db/query.sql` queries**

```sql
-- name: GetExtensionVersion :one
SELECT version FROM extension_versions WHERE extension_key = $1;

-- name: UpdateExtensionVersion :exec
INSERT INTO extension_versions (extension_key, version, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (extension_key) 
DO UPDATE SET version = EXCLUDED.version, updated_at = NOW();
```

- [ ] **Step 4: Create `core/db/postgres.go` for running migrations and opening pool**

```go
package db

import (
	"database/sql"
	"embed"
	"log"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	_ "github.com/lib/pq"
)

func InitDB(connStr string) (*sql.DB, error) {
	db, err := sql.Open("postgres", connStr)
	if err != nil {
		return nil, err
	}

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return nil, err
	}

	m, err := migrate.NewWithDatabaseInstance(
		"file://db/migrations",
		"postgres", driver)
	if err != nil {
		return nil, err
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return nil, err
	}

	return db, nil
}
```

- [ ] **Step 5: Compile sqlc queries and commit**

Run: `sqlc generate` (ensure sqlc is installed)
Expected: Code generated in `core/db/sqlc/`
Run: `git add db/ core/db/ && git commit -m "feat: setup pg18, sqlc, and go-migrate base"`

---

### Task 2: Core-Managed Document Store (CMDS) Manager

Implement the dynamic table builder, index reconciler, and JSONB queries inside the Core DB service.

**Files:**
- Create: `core/db/cmds.go`
- Create: `core/db/cmds_test.go`

**Interfaces:**
- Produces: `cmds.NewCMDSManager(db *sql.DB) *CMDSManager`
  - `cmds.ReconcileSchema(extKey string, manifest *ExtensionManifest) error`
  - `cmds.Put(extKey, collection, docID string, data []byte) error`
  - `cmds.Get(extKey, collection, docID string) ([]byte, bool, error)`
  - `cmds.Find(extKey, collection string, filters []Filter, limit, offset int) ([][]byte, error)`

- [ ] **Step 1: Write `core/db/cmds.go`**

```go
package db

import (
	"database/sql"
	"fmt"
	"strings"
)

type Filter struct {
	Field    string
	Operator string
	Value    string
}

type CollectionSchema struct {
	Name    string
	Indexes []string
}

type CMDSManager struct {
	db *sql.DB
}

func NewCMDSManager(db *sql.DB) *CMDSManager {
	return &CMDSManager{db: db}
}

func (m *CMDSManager) ReconcileSchema(extKey string, collections []CollectionSchema) error {
	for _, col := range collections {
		tableName := fmt.Sprintf("ext_%s_%s", extKey, col.Name)
		
		// 1. Create table
		query := fmt.Sprintf(`
			CREATE TABLE IF NOT EXISTS %s (
				id VARCHAR(100) PRIMARY KEY,
				data JSONB NOT NULL,
				created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP,
				updated_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
			);`, tableName)
		if _, err := m.db.Exec(query); err != nil {
			return err
		}

		// 2. Reconcile Indexes
		for _, idxField := range col.Indexes {
			indexName := fmt.Sprintf("idx_%s_%s_%s", extKey, col.Name, idxField)
			idxQuery := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s ON %s ((data->>'%s'));`, indexName, tableName, idxField)
			if _, err := m.db.Exec(idxQuery); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *CMDSManager) Put(extKey, collection, docID string, data []byte) error {
	tableName := fmt.Sprintf("ext_%s_%s", extKey, collection)
	query := fmt.Sprintf(`
		INSERT INTO %s (id, data, updated_at) 
		VALUES ($1, $2, NOW()) 
		ON CONFLICT (id) 
		DO UPDATE SET data = EXCLUDED.data, updated_at = NOW();`, tableName)
	_, err := m.db.Exec(query, docID, data)
	return err
}

func (m *CMDSManager) Get(extKey, collection, docID string) ([]byte, bool, error) {
	tableName := fmt.Sprintf("ext_%s_%s", extKey, collection)
	query := fmt.Sprintf(`SELECT data FROM %s WHERE id = $1;`, tableName)
	var data []byte
	err := m.db.QueryRow(query, docID).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

func (m *CMDSManager) Find(extKey, collection string, filters []Filter, limit, offset int) ([][]byte, error) {
	tableName := fmt.Sprintf("ext_%s_%s", extKey, collection)
	var whereClauses []string
	var args []interface{}
	argIdx := 1

	for _, f := range filters {
		// Secure operator validation
		op := "="
		if f.Operator == ">" || f.Operator == "<" || f.Operator == "LIKE" {
			op = f.Operator
		}
		whereClauses = append(whereClauses, fmt.Sprintf("data->>'%s' %s $%d", f.Field, op, argIdx))
		args = append(args, f.Value)
		argIdx++
	}

	where := ""
	if len(whereClauses) > 0 {
		where = "WHERE " + strings.Join(whereClauses, " AND ")
	}

	query := fmt.Sprintf(`SELECT data FROM %s %s LIMIT $%d OFFSET $%d;`, tableName, where, argIdx, argIdx+1)
	args = append(args, limit, offset)

	rows, err := m.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results [][]byte
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		results = append(results, data)
	}
	return results, nil
}
```

- [ ] **Step 2: Run tests with a Docker PostgreSQL DB instance**

(Assuming PG port is exposed on 5432)
Run: `go test -v ./core/db/...` (Ensure postgres environment is running and pass CONN_STR)
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add core/db/cmds.go
git commit -m "feat: implement CMDS database query executor"
```

---

### Task 3: Core Database gRPC Service & Schema Migration

Expose the `DatabaseService` gRPC server and implement declarative JSONB migrations parser during extension registration.

**Files:**
- Create: `core/tunnel/db_server.go`
- Modify: `core/tunnel/server.go`

**Interfaces:**
- Consumes: `CMDSManager` from `core/db`
- Produces: `DatabaseServiceServer` gRPC handler.

- [ ] **Step 1: Implement `core/tunnel/db_server.go`**

```go
package tunnel

import (
	"context"

	"github.com/ty-lab/moduleless/core/db"
	pb "github.com/ty-lab/moduleless/proto/tunnel"
)

type DbServer struct {
	pb.UnimplementedDatabaseServiceServer
	CMDS *db.CMDSManager
}

func NewDbServer(cmds *db.CMDSManager) *DbServer {
	return &DbServer{CMDS: cmds}
}

func (s *DbServer) Put(ctx context.Context, req *pb.PutRequest) (*pb.PutResponse, error) {
	extKey := ctx.Value("extension_key").(string) // Injected by interceptor
	err := s.CMDS.Put(extKey, req.Collection, req.DocumentId, req.JsonData)
	if err != nil {
		return &pb.PutResponse{Success: false}, err
	}
	return &pb.PutResponse{Success: true}, nil
}

func (s *DbServer) Get(ctx context.Context, req *pb.GetRequest) (*pb.GetResponse, error) {
	extKey := ctx.Value("extension_key").(string)
	data, found, err := s.CMDS.Get(extKey, req.Collection, req.DocumentId)
	if err != nil {
		return nil, err
	}
	return &pb.GetResponse{Found: found, JsonData: data}, nil
}

func (s *DbServer) Find(ctx context.Context, req *pb.FindRequest) (*pb.FindResponse, error) {
	extKey := ctx.Value("extension_key").(string)
	var filters []db.Filter
	for _, f := range req.Filters {
		filters = append(filters, db.Filter{
			Field:    f.Field,
			Operator: f.Operator,
			Value:    f.Value,
		})
	}
	docs, err := s.CMDS.Find(extKey, req.Collection, filters, int(req.Limit), int(req.Offset))
	if err != nil {
		return nil, err
	}
	return &pb.FindResponse{Documents: docs}, nil
}
```

- [ ] **Step 2: Commit**

```bash
git add core/tunnel/db_server.go
git commit -m "feat: implement DatabaseService gRPC server handlers"
```

---

### Task 4: Go SDK Database Client

Extend the Go SDK to expose type-safe CMDS Database API clients to developers.

**Files:**
- Create: `sdk/go/db.go`

**Interfaces:**
- Produces: `sdk.DB` instance.
  - `sdk.DB.Put(ctx, collection, id, val)`
  - `sdk.DB.Get(ctx, collection, id, &val)`
  - `sdk.DB.Find(ctx, collection, filters, &val)`

- [ ] **Step 1: Implement `sdk/go/db.go` client wrapper**

```go
package sdk

import (
	"context"
	"encoding/json"

	pb "github.com/ty-lab/moduleless/proto/tunnel"
	"google.golang.org/grpc"
)

type DBClient struct {
	client pb.DatabaseServiceClient
}

func NewDBClient(conn *grpc.ClientConn) *DBClient {
	return &DBClient{client: pb.NewDatabaseServiceClient(conn)}
}

func (db *DBClient) Put(ctx context.Context, collection, docID string, value interface{}) error {
	jsonData, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = db.client.Put(ctx, &pb.PutRequest{
		Collection: collection,
		DocumentId: docID,
		JsonData:   jsonData,
	})
	return err
}

func (db *DBClient) Get(ctx context.Context, collection, docID string, dest interface{}) error {
	resp, err := db.client.Get(ctx, &pb.GetRequest{
		Collection: collection,
		DocumentId: docID,
	})
	if err != nil {
		return err
	}
	if !resp.Found {
		return json.Unmarshal([]byte("{}"), dest)
	}
	return json.Unmarshal(resp.JsonData, dest)
}
```

- [ ] **Step 2: Commit**

```bash
git add sdk/go/db.go
git commit -m "feat: implement Go SDK DatabaseClient wrappers"
```
