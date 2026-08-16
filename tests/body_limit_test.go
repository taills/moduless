package tests

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// What an oversized body does to a filter that asked to see bodies.
//
// The design note for the pipeline worries about a specific bypass: if a
// filter were handed a truncated body while the backend got the whole thing,
// the filter would be deciding on data the system does not actually process.
// Its answer was that the limit is the maximum over every filter that wants a
// body and the backend, and that exceeding it is a 413 rather than a silent
// truncation.
//
// Chain does compute that maximum — MaxRequestBodyBytes, "the largest body any
// filter is willing to receive". Nothing reads it. Its only caller in the
// repository is its own unit test, which is the same shape as the dead menu
// builder the plan called out. So what actually happens to an oversized body
// is whatever runFilter does with it, and that is worth knowing exactly,
// because the size is chosen by whoever sends the request.

// bodyFilterStack serves a plugin behind one body-inspecting filter.
func bodyFilterStack(t *testing.T, maxBody int, failClosed bool) (url string, inst *pluginhost.Instance) {
	t.Helper()

	inst = launchPlugin(t, "inspector", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "inspector",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "inspector", manifest.FilterDecl{
			Name:             "scan",
			Phase:            manifest.PhasePreRoute,
			TimeoutMS:        2000,
			NeedsRequestBody: true,
			MaxBodyBytes:     maxBody,
			FailClosed:       failClosed,
			Match:            manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL, inst
}

func postWithTrace(t *testing.T, url, path, traceID string, body []byte) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	req.Header.Set("X-Request-Id", traceID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// The control: a body within the declared limit reaches the filter. Without
// this, "the filter did not run" below could mean the wiring never worked.
func TestABodyWithinTheLimitReachesTheFilter(t *testing.T) {
	const maxBody = 4096
	url, inst := bodyFilterStack(t, maxBody, false)

	const trace = "body-small"
	resp := postWithTrace(t, url, "/api/plugins/hello/items", trace,
		[]byte(`{"note":"`+strings.Repeat("a", 100)+`"}`))
	resp.Body.Close()

	if got := phasesSeen(t, inst, trace); len(got) == 0 {
		t.Fatal("a body-inspecting filter was not called for a body well inside its " +
			"declared limit; nothing below this line would mean anything")
	}
}

// The finding: exceeding the limit skips a fail-open filter entirely.
//
// Not a truncation, which is what the design note guarded against — the filter
// is simply not consulted. That is safer in one sense and worse in another,
// because the request still reaches the backend with the whole body. The size
// is the client's to choose, so any fail-open filter that inspects bodies can
// be taken out of the path by sending one larger than it declared.
func TestAnOversizedBodySkipsAFailOpenFilter(t *testing.T) {
	const maxBody = 1024
	url, inst := bodyFilterStack(t, maxBody, false)

	const trace = "body-large"
	resp := postWithTrace(t, url, "/api/plugins/hello/items", trace,
		bytes.Repeat([]byte("x"), maxBody*4))
	defer resp.Body.Close()

	phases := phasesSeen(t, inst, trace)
	t.Logf("body %dB against a %dB limit: filter phases seen = %v, status = %d",
		maxBody*4, maxBody, phases, resp.StatusCode)

	if len(phases) > 0 {
		t.Errorf("the filter was called with a body over its declared limit (%v); either "+
			"it received a truncated body, which is the bypass the limit exists to "+
			"prevent, or the limit is not enforced at all", phases)
	}
	if resp.StatusCode == http.StatusRequestEntityTooLarge {
		t.Errorf("status 413: a fail-open filter rejected the request, which contradicts " +
			"fail-open meaning a broken filter does not take the site down")
	}
}

// A fail-closed filter refuses instead, which is the half the design note got
// right: a filter enforcing a security decision does not get skipped.
func TestAnOversizedBodyIsRefusedByAFailClosedFilter(t *testing.T) {
	const maxBody = 1024
	url, inst := bodyFilterStack(t, maxBody, true)

	const trace = "body-large-closed"
	resp := postWithTrace(t, url, "/api/plugins/hello/items", trace,
		bytes.Repeat([]byte("x"), maxBody*4))
	defer resp.Body.Close()

	t.Logf("fail-closed, body %dB against a %dB limit: status = %d, phases = %v",
		maxBody*4, maxBody, resp.StatusCode, phasesSeen(t, inst, trace))

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413: a fail-closed body filter that cannot see the "+
			"body must refuse, or declaring fail_closed buys nothing against exactly "+
			"the input it was meant to inspect", resp.StatusCode)
	}
}
