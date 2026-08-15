package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// testDB connects to TEST_DATABASE_URL or skips the test when unavailable, so
// the suite stays green on machines without a PostgreSQL instance.
func testDB(t *testing.T) *sql.DB {
	t.Helper()
	connStr := os.Getenv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping CMDS integration test")
	}
	conn, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := conn.Ping(); err != nil {
		t.Skipf("cannot reach test database: %v", err)
	}
	return conn
}

func TestValidateIdentifier(t *testing.T) {
	good := []string{"items", "user_manager", "Col1"}
	for _, g := range good {
		if err := ValidateIdentifier(g); err != nil {
			t.Errorf("expected %q valid, got %v", g, err)
		}
	}
	bad := []string{"", "1items", "drop table", "a;b", "a-b", "a'b"}
	for _, b := range bad {
		if err := ValidateIdentifier(b); err == nil {
			t.Errorf("expected %q invalid", b)
		}
	}
}

func TestNormalizeOperator(t *testing.T) {
	cases := map[string]string{">": ">", "<": "<", "LIKE": "LIKE", "=": "=", "; DROP": "="}
	for in, want := range cases {
		if got := normalizeOperator(in); got != want {
			t.Errorf("normalizeOperator(%q)=%q want %q", in, got, want)
		}
	}
}

func TestCMDSPutGetFindDelete(t *testing.T) {
	conn := testDB(t)
	defer conn.Close()

	m := NewCMDSManager(conn)
	ext, col := "testext", "items"
	defer conn.Exec("DROP TABLE IF EXISTS ext_testext_items;")

	if err := m.ReconcileSchema(ext, []CollectionSchema{
		{Name: col, Indexes: []Index{{Fields: []string{"status"}}, {Fields: []string{"code"}, Unique: true}}},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	doc := map[string]any{"status": "active", "code": "A1", "name": "Alice"}
	raw, _ := json.Marshal(doc)
	if err := m.Put(ext, col, "1", raw); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, ok, err := m.Get(ext, col, "1")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	var back map[string]any
	json.Unmarshal(got, &back)
	if back["name"] != "Alice" {
		t.Fatalf("expected Alice, got %v", back["name"])
	}

	docs, err := m.Find(context.Background(), nil, ext, col, []Filter{{Field: "status", Operator: "=", Value: "active"}}, 10, 0)
	if err != nil || len(docs) != 1 {
		t.Fatalf("find: len=%d err=%v", len(docs), err)
	}

	if err := m.Delete(ext, col, "1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok, _ := m.Get(ext, col, "1"); ok {
		t.Fatalf("expected doc deleted")
	}
}

func TestCMDSMigrationRenameField(t *testing.T) {
	conn := testDB(t)
	defer conn.Close()

	m := NewCMDSManager(conn)
	ext, col := "migext", "items"
	defer conn.Exec("DROP TABLE IF EXISTS ext_migext_items;")

	if err := m.ReconcileSchema(ext, []CollectionSchema{{Name: col}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"name": "Bob"})
	if err := m.Put(ext, col, "1", raw); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := m.ApplyMigration(ext, Migration{
		Version: "1.1.0",
		Actions: []MigrationAction{{Type: "rename_field", Collection: col, From: "name", To: "full_name"}},
	}); err != nil {
		t.Fatalf("migration: %v", err)
	}

	got, _, _ := m.Get(ext, col, "1")
	var back map[string]any
	json.Unmarshal(got, &back)
	if back["full_name"] != "Bob" || back["name"] != nil {
		t.Fatalf("expected rename to full_name, got %v", back)
	}
}
