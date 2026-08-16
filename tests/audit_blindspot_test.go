package tests

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
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
