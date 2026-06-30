package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sqlc "github.com/taills/moduleless/core/db/sqlc"
)

type fakeRecorder struct {
	mu      sync.Mutex
	records []sqlc.InsertAuditLogParams
}

func (f *fakeRecorder) InsertAuditLog(_ context.Context, arg sqlc.InsertAuditLogParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, arg)
	return nil
}

func (f *fakeRecorder) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		c := len(f.records)
		f.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected %d audit records", n)
}

func serve(h http.Handler, method, path string) {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("X-User-Id", "42")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestAuditLogsStateChanges(t *testing.T) {
	rec := &fakeRecorder{}
	h := AuditLogger(rec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	serve(h, http.MethodPost, "/api/extensions/myext/items")
	rec.wait(t, 1)

	rec.mu.Lock()
	got := rec.records[0]
	rec.mu.Unlock()
	if got.ExtensionKey != "myext" || got.UserID != "42" {
		t.Fatalf("unexpected audit record: %+v", got)
	}
}

func TestAuditSkipsReadsAndNonExtensions(t *testing.T) {
	rec := &fakeRecorder{}
	h := AuditLogger(rec)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	serve(h, http.MethodGet, "/api/extensions/myext/items") // read -> skip
	serve(h, http.MethodPost, "/api/system/files/upload")   // non-extension -> skip

	time.Sleep(100 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.records) != 0 {
		t.Fatalf("expected no audit records, got %d", len(rec.records))
	}
}
