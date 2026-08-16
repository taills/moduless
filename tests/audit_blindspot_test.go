package tests

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// The one request the log phase never saw.
//
// The guide states it plainly: the log phase runs for every request, including
// ones an earlier filter short-circuited, ones that failed authentication and
// ones where the backend returned 5xx — "so auditing has no blind spot for
// rejected traffic". Each of those is tested. Every exit in serve() calls
// logPhase, ten of them, kept in step by hand.
//
// Except one. The body is buffered before the pipeline starts, and when it is
// over the limit readBody writes 413 and serve returns then and there — before
// any phase has run, log included. So the single rejection a caller can
// trigger at will, by sending a large body, is the single rejection that
// leaves no audit record. Somebody probing for that limit is invisible in
// exactly the log meant to show them.

func auditGateway(t *testing.T, maxBody int64) (url string, watcher *pluginhost.Instance) {
	t.Helper()

	watcher = launchPlugin(t, "watcher", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "watcher",
		Instances: []*pluginhost.Instance{watcher},
		Filters: compileFilters(t, "watcher", manifest.FilterDecl{
			Name:  "audit",
			Phase: manifest.PhaseLog,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})

	h := &gateway.PluginHandler{
		Registry:     reg,
		Runner:       &pipeline.Runner{},
		MaxBodyBytes: maxBody,
	}
	srv := httptest.NewServer(h.Middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })))
	t.Cleanup(srv.Close)
	return srv.URL, watcher
}

// The control: an ordinary request is audited. Without it, "no audit record"
// below could just mean the log filter was never wired up.
func TestAnOrdinaryRequestIsAudited(t *testing.T) {
	url, watcher := auditGateway(t, 1024)

	const trace = "audit-ordinary"
	resp := postWithTrace(t, url, "/api/plugins/hello/items", trace, []byte(`{"a":1}`))
	resp.Body.Close()

	if got := waitUntilPhase(t, watcher, trace, "PHASE_LOG"); !slices.Contains(got, "PHASE_LOG") {
		t.Fatalf("phases = %v; the audit filter did not run for an ordinary request, so "+
			"nothing below this line would mean anything", got)
	}
}

func TestAnOversizedRequestIsStillAudited(t *testing.T) {
	const maxBody = 1024
	url, watcher := auditGateway(t, maxBody)

	const trace = "audit-oversized"
	resp := postWithTrace(t, url, "/api/plugins/hello/items", trace,
		bytes.Repeat([]byte("x"), maxBody*4))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: the body limit did not reject this, so the "+
			"test is not exercising the path it is about", resp.StatusCode)
	}

	got := waitUntilPhase(t, watcher, trace, "PHASE_LOG")
	t.Logf("413 for an oversized body, phases recorded: %v", got)
	if !slices.Contains(got, "PHASE_LOG") {
		t.Errorf("phases = %v; a request rejected for its body size produced no audit "+
			"record. Every other rejection runs the log phase, and this is the one a "+
			"caller can trigger deliberately — an audit log with a hole exactly where "+
			"somebody is probing is worse than one that is known to be partial", got)
	}
}

// statusesSeen asks the plugin which upstream status each log-phase call was
// shown for a trace.
func statusesSeen(t *testing.T, inst *pluginhost.Instance, traceID string) []string {
	t.Helper()

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/statuses", Query: traceID,
	})
	if err != nil {
		t.Fatalf("asking for statuses: %v", err)
	}
	body := strings.TrimSpace(string(resp.GetBody()))
	if body == "" {
		return nil
	}
	return strings.Fields(body)
}

// Running is not the same as knowing what happened.
//
// The blind spot above was the log phase not running at all. This is the
// quieter half: it runs, and is shown a status of 0. Found by hand, watching
// the audit example's own table after driving Core through air —
//
//	POST /api/plugins/notes/notes -> 0    user=
//	POST /api/plugins/notes/notes -> 201  user=admin
//
// — where the first two rows are the requests that were refused, which are the
// rows an audit trail exists for. A short circuit was written straight to the
// wire without being recorded on the request, so every refusal reached the log
// phase indistinguishable from "no idea".
func TestARefusedRequestIsAuditedWithItsRealStatus(t *testing.T) {
	watcher := launchPlugin(t, "watcher", "1.0.0", nil)
	guard := launchPlugin(t, "guard", "1.0.0", []string{"filter:authenticate"})
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "watcher",
		Instances: []*pluginhost.Instance{watcher},
		Filters: compileFilters(t, "watcher", manifest.FilterDecl{
			Name: "audit", Phase: manifest.PhaseLog,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	// A pre_route filter that refuses. /refuse rather than /deny: the fixture
	// suffix-matches the first and exact-matches the second, and an exact match
	// only ever fires in the one phase that is handed the full request path.
	// The fixture's own comment warns about this, and the precondition assertion
	// below is what caught me ignoring it — the first version of this test sailed
	// through on a 200.
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "guard",
		Instances: []*pluginhost.Instance{guard},
		Filters: compileFilters(t, "guard", manifest.FilterDecl{
			Name: "deny", Phase: manifest.PhasePreRoute,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})

	h := &gateway.PluginHandler{Registry: reg, Runner: &pipeline.Runner{}, MaxBodyBytes: 1 << 20}
	srv := httptest.NewServer(h.Middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })))
	t.Cleanup(srv.Close)

	const trace = "audit-refused"
	resp := requestWithTrace(t, srv.URL, "/api/plugins/hello/refuse", trace)
	resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatalf("the request was not refused, so this test proves nothing: %d", resp.StatusCode)
	}
	waitUntilPhase(t, watcher, trace, "PHASE_LOG")

	got := statusesSeen(t, watcher, trace)
	if len(got) == 0 {
		t.Fatal("the log phase recorded no status at all")
	}
	want := strconv.Itoa(resp.StatusCode)
	if got[len(got)-1] != want {
		t.Errorf("the audit filter was shown status %v for a request the caller "+
			"received %s for. A refusal recorded as 0 is indistinguishable from "+
			"one nobody understood, and refusals are what an audit trail is for.",
			got, want)
	}
}
