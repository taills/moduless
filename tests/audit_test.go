package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/middleware"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
)

// Auditing, against a real route.
//
// The middleware carried its own copy of the API prefix as a string literal.
// When the route was renamed from /api/extensions/ to /api/plugins/, the
// literal stayed behind: every request still passed through the middleware,
// none of them matched, and the audit table was permanently empty. Nothing
// failed — the middleware's own test used the dead prefix too, so it went on
// passing, and a compliance feature was silently absent for the whole life of
// the rename.
//
// A unit test of the middleware cannot catch that. It can only check that the
// prefix it was handed is honoured, and the prefix it is handed in a test is
// whatever the test decided. Only a request through the real route can say
// whether Core audits the traffic Core actually serves — which is why this
// test lives here and not in core/middleware.

type recordingAuditor struct {
	mu      sync.Mutex
	records []sqlc.InsertAuditLogParams
}

func (a *recordingAuditor) InsertAuditLog(_ context.Context, arg sqlc.InsertAuditLogParams) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.records = append(a.records, arg)
	return nil
}

func (a *recordingAuditor) all() []sqlc.InsertAuditLogParams {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sqlc.InsertAuditLogParams(nil), a.records...)
}

// waitFor polls until n records have arrived. Auditing is deliberately
// asynchronous — it must never delay a response — so a read straight after the
// request would race the write rather than measure it.
func (a *recordingAuditor) waitFor(t *testing.T, n int) []sqlc.InsertAuditLogParams {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := a.all(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d audit record(s) within 3s, want %d; Core is not auditing the route it serves",
		len(a.all()), n)
	return nil
}

// newAuditedServer is the real plugin route with the real audit middleware
// over it, wired the way core/main.go wires them — in particular, taking the
// prefix from the gateway package rather than writing it out again here. That
// pairing is the thing under test.
func newAuditedServer(t *testing.T, reg *pluginhost.Registry, auditor middleware.AuditRecorder,
	identify func(*http.Request) string) string {
	t.Helper()

	h := &gateway.PluginHandler{Registry: reg, Runner: &pipeline.Runner{}}
	notFound := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	handler := middleware.AuditLogger(auditor, middleware.AuditOptions{
		Prefix:   gateway.PluginAPIPrefix,
		Identify: identify,
	})(h.Middleware(notFound))

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv.URL
}

// A write to a plugin's own API is recorded.
func TestAuditRecordsWritesToTheRealPluginRoute(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
	})

	auditor := &recordingAuditor{}
	srv := newAuditedServer(t, reg, auditor, func(*http.Request) string { return "7" })

	resp, err := http.Post(srv+"/api/plugins/hello/items", "application/json",
		strings.NewReader(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	got := auditor.waitFor(t, 1)[0]
	if got.ExtensionKey != "hello" {
		t.Errorf("plugin key = %q, want hello", got.ExtensionKey)
	}
	if got.HttpPath != "/api/plugins/hello/items" {
		t.Errorf("path = %q", got.HttpPath)
	}
	if got.UserID != "7" {
		t.Errorf("user = %q, want the session's user", got.UserID)
	}
}

// A forged identity does not become the acting user. The audited party is
// exactly who should not get to choose the name in the record.
func TestAuditIgnoresAForgedIdentityHeader(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
	})

	auditor := &recordingAuditor{}
	// Nobody is authenticated, which is what an attacker's request looks like.
	srv := newAuditedServer(t, reg, auditor, func(*http.Request) string { return "" })

	req, _ := http.NewRequest(http.MethodPost, srv+"/api/plugins/hello/items",
		strings.NewReader(`{"title":"x"}`))
	req.Header.Set("X-User-Id", "1")
	req.Header.Set("X-Forwarded-User", "admin")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	got := auditor.waitFor(t, 1)[0]
	if got.UserID == "1" || got.UserID == "admin" {
		t.Fatalf("the audit log recorded %q, which the client put in a header", got.UserID)
	}
	if got.UserID != "anonymous" {
		t.Errorf("user = %q; an unauthenticated write should read as anonymous", got.UserID)
	}
}

// Reads are not audited. The log is about who changed what.
func TestAuditSkipsReadsOnTheRealRoute(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
	})

	auditor := &recordingAuditor{}
	srv := newAuditedServer(t, reg, auditor, nil)

	resp, err := http.Get(srv + "/api/plugins/hello/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	time.Sleep(300 * time.Millisecond)
	if got := auditor.all(); len(got) != 0 {
		t.Errorf("a read produced %d audit record(s): %+v", len(got), got)
	}
}

// An audit record has to join to the request that produced it.
//
// Core assigns a trace to every request and publishes it as X-Request-Id, and
// the plugin's own log lines carry it. The audit trail was the one place it
// did not land — so "admin deleted user 5" could not be tied to the request
// that did it, nor to anything the plugins logged while doing it. Which is
// what an audit trail is for.
//
// The claim is not "the field is populated" but "the three agree", so all
// three are read: what the caller asked for, what came back, and what was
// written down.
func TestAnAuditRecordCarriesTheRequestsTrace(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{inst},
	})

	auditor := &recordingAuditor{}
	srv := newAuditedServer(t, reg, auditor, func(*http.Request) string { return "7" })

	const trace = "audit-trace-1"
	req, err := http.NewRequest(http.MethodPost, srv+"/api/plugins/hello/items",
		strings.NewReader(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("X-Request-Id", trace)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	// Core echoes the id it used, which is what a caller keeps in order to ask
	// about this request later.
	echoed := resp.Header.Get("X-Request-Id")
	if echoed != trace {
		t.Fatalf("X-Request-Id = %q, want the id the caller sent (%q); with a "+
			"different id on the response there is nothing to correlate against",
			echoed, trace)
	}

	got := auditor.waitFor(t, 1)[0]
	if got.TraceID != echoed {
		t.Errorf("audit record trace = %q, response said %q. An entry that cannot be "+
			"joined to its request records that something happened without recording "+
			"which something", got.TraceID, echoed)
	}
}

// A request with no trace of its own still gets one, and the record carries
// it. Otherwise every unattributed request shares the empty string and the
// column is worse than absent — it looks like a join key and is not one.
func TestAnAuditRecordHasATraceEvenWhenTheCallerSendsNone(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{inst},
	})

	auditor := &recordingAuditor{}
	srv := newAuditedServer(t, reg, auditor, func(*http.Request) string { return "7" })

	resp, err := http.Post(srv+"/api/plugins/hello/items", "application/json",
		strings.NewReader(`{"title":"x"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	got := auditor.waitFor(t, 1)[0]
	if got.TraceID == "" {
		t.Error("the audit record has no trace at all; Core generates one for every " +
			"request precisely so that this never has to be empty")
	}
	if got.TraceID != resp.Header.Get("X-Request-Id") {
		t.Errorf("record says %q, response said %q", got.TraceID, resp.Header.Get("X-Request-Id"))
	}
}
