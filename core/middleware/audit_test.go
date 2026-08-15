package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sqlc "github.com/taills/moduless/core/db/sqlc"
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

// testPrefix is what these tests audit. It is a stand-in, deliberately not the
// real route: what matters here is that the middleware honours the prefix it
// was given. That the prefix Core supplies is the one Core actually routes is
// a different claim, and it is checked end to end in tests/audit_test.go —
// this package cannot check it without importing the gateway.
//
// The distinction is the whole reason auditing was broken for as long as it
// was. The old test hard-coded /api/extensions/ and passed for years after
// nothing routed that path any more.
const testPrefix = "/api/audited/"

func auditing(rec AuditRecorder, identify func(*http.Request) string) http.Handler {
	return AuditLogger(rec, AuditOptions{Prefix: testPrefix, Identify: identify})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
}

func serve(h http.Handler, method, path string) {
	req := httptest.NewRequest(method, path, nil)
	// Sent on every request in these tests: nothing in Core sets this header,
	// so anything that reads it is reading what the client chose to say.
	req.Header.Set("X-User-Id", "42")
	h.ServeHTTP(httptest.NewRecorder(), req)
}

func TestAuditLogsStateChanges(t *testing.T) {
	rec := &fakeRecorder{}
	h := auditing(rec, func(*http.Request) string { return "7" })

	serve(h, http.MethodPost, testPrefix+"myext/items")
	rec.wait(t, 1)

	rec.mu.Lock()
	got := rec.records[0]
	rec.mu.Unlock()
	if got.ExtensionKey != "myext" {
		t.Errorf("plugin key = %q, want myext", got.ExtensionKey)
	}
	if got.UserID != "7" {
		t.Errorf("user = %q, want the session's user; %q would mean the header was believed",
			got.UserID, "42")
	}
}

// The identity in an audit record comes from the session, never from the
// request. The middleware used to read X-User-Id — a header nothing in Core
// sets and any client may send — so an audit log recorded whatever name the
// audited party chose. Evidence that is wrong is worse than no evidence.
func TestAuditIgnoresAClientSuppliedIdentity(t *testing.T) {
	rec := &fakeRecorder{}
	h := auditing(rec, func(*http.Request) string { return "" }) // nobody is logged in

	serve(h, http.MethodPost, testPrefix+"myext/items") // sends X-User-Id: 42
	rec.wait(t, 1)

	rec.mu.Lock()
	got := rec.records[0]
	rec.mu.Unlock()
	if got.UserID == "42" {
		t.Fatal("the client's own X-User-Id header was recorded as the acting user")
	}
	if got.UserID != "anonymous" {
		t.Errorf("user = %q; an unauthenticated write should be recorded as anonymous", got.UserID)
	}
}

func TestAuditSkipsReadsAndOtherRoutes(t *testing.T) {
	rec := &fakeRecorder{}
	h := auditing(rec, nil)

	serve(h, http.MethodGet, testPrefix+"myext/items")    // read -> skip
	serve(h, http.MethodPost, "/api/system/files/upload") // outside the prefix -> skip

	time.Sleep(100 * time.Millisecond)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.records) != 0 {
		t.Fatalf("expected no audit records, got %d", len(rec.records))
	}
}

// A prefix is required. Auditing nothing is what this middleware already did
// for a whole rename cycle without anyone noticing, so the wiring mistake that
// causes it fails at start-up instead.
func TestAuditLoggerRefusesAnEmptyPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an unconfigured audit logger was accepted; it would audit nothing, silently")
		}
	}()
	AuditLogger(&fakeRecorder{}, AuditOptions{})
}
