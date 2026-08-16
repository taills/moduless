package tests

import (
	"io"
	"net/http"
	"testing"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// What RewritePath can and cannot do.
//
// The SDK says "RewritePath changes where the request is routed", and nothing
// exercised it — it is one of the ten exported SDK calls with no test, example
// or fixture behind them. Reading the gateway, the claim looked only half
// true: the plugin key and sub-path are split from rc.Path, which the rewrite
// updates, but the decision of whether this is a plugin route at all is taken
// from the original r.URL.Path before the pre_route phase runs.
//
// If that reading is right, a rewrite inside the plugin namespace works and a
// rewrite into it does not — and the second is the obvious thing to reach for,
// a short public URL in front of a plugin's own path. Measured here rather
// than asserted, because the last two guesses about which direction a bug ran
// were both wrong.

// rewriteStack installs a pre_route filter that rewrites the path the fixture
// is told to rewrite to, via its own /rewrite route contract.
func rewriteStack(t *testing.T, to string) (url string, backend *pluginhost.Instance) {
	t.Helper()

	rewriter, err := launchWithEnv(t, "rewriter", "ECHO_REWRITE_TO="+to)
	if err != nil {
		t.Fatalf("launching the rewriting filter: %v", err)
	}
	t.Cleanup(rewriter.Kill)

	backend = launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "rewriter",
		Instances: []*pluginhost.Instance{rewriter},
		Filters: compileFilters(t, "rewriter", manifest.FilterDecl{
			Name:      "rewrite",
			Phase:     manifest.PhasePreRoute,
			TimeoutMS: 2000,
			Match:     manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL, backend
}

// Within the plugin namespace: the backend is asked for the rewritten path.
func TestRewriteWithinThePluginNamespaceReachesTheNewPath(t *testing.T) {
	url, _ := rewriteStack(t, "/api/plugins/hello/rewritten")

	resp, err := http.Get(url + "/api/plugins/hello/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got := resp.Header.Get("X-Echo-Path")
	t.Logf("requested /api/plugins/hello/items, the backend was asked for %q", got)
	if got != "/rewritten" {
		t.Errorf("the backend was asked for %q, want %q: a rewrite before routing has "+
			"to change what the backend is asked for, or it changes nothing anyone "+
			"can observe", got, "/rewritten")
	}
}

// Into the plugin namespace from outside it.
func TestRewriteIntoThePluginNamespace(t *testing.T) {
	url, _ := rewriteStack(t, "/api/plugins/hello/items")

	resp, err := http.Get(url + "/legacy/items")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	echoed := resp.Header.Get("X-Echo-Path")
	t.Logf("GET /legacy/items with a rewrite to /api/plugins/hello/items → status %d, "+
		"backend asked for %q", resp.StatusCode, echoed)

	if echoed == "" {
		t.Errorf("the plugin was never called: whether a request is a plugin route is " +
			"decided from the original URL, before pre_route runs, so a rewrite into " +
			"the plugin namespace changes rc.Path and nothing routes on it. That is " +
			"the obvious use for RewritePath — a short public URL in front of a " +
			"plugin — and the SDK says it changes where the request is routed")
	}
}

// A body survives being rewritten into the plugin namespace.
//
// This is the constraint that made the fix non-obvious rather than a line
// move. Filters must see the body, so it is buffered before they run, and
// whether to buffer was the same decision as whether this is a plugin route.
// A rewrite that arrives after that decision therefore lands on a request
// whose body was never read — and handing the backend an empty body would be
// a quieter failure than the 404 it replaced.
func TestABodySurvivesARewriteIntoThePluginNamespace(t *testing.T) {
	url, _ := rewriteStack(t, "/api/plugins/hello/items")

	const payload = `{"note":"carried across the rewrite"}`
	resp := postWithTrace(t, url, "/legacy/items", "rewrite-body", []byte(payload))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The fixture echoes whatever body it was handed, so the response is what
	// the plugin actually received.
	echoed, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	t.Logf("posted %d bytes to a rewritten path, the backend echoed %d back",
		len(payload), len(echoed))
	if string(echoed) != payload {
		t.Errorf("the backend received %d bytes of body, want %d: the rewrite reached "+
			"the plugin but the body was dropped on the way, which is a quieter "+
			"failure than the 404 it replaced", len(echoed), len(payload))
	}
}
