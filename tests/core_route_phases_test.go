package tests

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// Which phases a filter gets on a route Core serves itself.
//
// Filters are global by design — the handler is installed around the whole
// gateway, not only plugin routes, so that a rate limiter or a WAF sees
// everything. The guide says post_handler is where a response is rewritten,
// and a filter declaring /** would be expected to cover Core's own API too:
// redacting /api/system/users is exactly the kind of thing somebody would
// reach for.
//
// Reading serve(), that path hands off to the rest of the gateway and returns
// without running post_handler at all. The response body is captured — but
// only into rc, which from there feeds the log phase. And the recorder writes
// through as it captures, so by the time anything could rewrite the response,
// the client already has it.
//
// So the question is not whether rewriting works there; it is whether the
// phase runs at all, and what an author is therefore promised.

func coreRouteGateway(t *testing.T, phases ...string) (url string, watcher *pluginhost.Instance) {
	t.Helper()

	watcher = launchPlugin(t, "watcher", "1.0.0", nil)

	decls := make([]manifest.FilterDecl, 0, len(phases))
	for _, p := range phases {
		decls = append(decls, manifest.FilterDecl{
			Name:              p,
			Phase:             p,
			TimeoutMS:         2000,
			NeedsResponseBody: true,
			Match:             manifest.FilterMatch{Paths: []string{"/**"}},
		})
	}

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "watcher",
		Instances: []*pluginhost.Instance{watcher},
		Filters:   compileFilters(t, "watcher", decls...),
	})

	h := &gateway.PluginHandler{Registry: reg, Runner: &pipeline.Runner{}}
	// Stands in for Core's own routes: not under /api/plugins/.
	srv := httptest.NewServer(h.Middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"users":[{"name":"admin","email":"a@example.com"}]}`))
		})))
	t.Cleanup(srv.Close)
	return srv.URL, watcher
}

// The control: pre_route does run for a route Core serves itself. Filters
// being global is the premise everything below rests on.
func TestPreRouteRunsForACoreRoute(t *testing.T) {
	url, watcher := coreRouteGateway(t, manifest.PhasePreRoute)

	const trace = "core-preroute"
	resp := requestWithTrace(t, url, "/api/system/users", trace)
	resp.Body.Close()

	if got := waitForPhases(t, watcher, trace, 1); !slices.Contains(got, "PHASE_PRE_ROUTE") {
		t.Fatalf("phases = %v; a pre_route filter matching /** did not run for a route "+
			"Core serves, so filters are not global after all", got)
	}
}

// The log phase runs there too, and sees the response body.
func TestLogPhaseRunsForACoreRoute(t *testing.T) {
	url, watcher := coreRouteGateway(t, manifest.PhaseLog)

	const trace = "core-log"
	resp := requestWithTrace(t, url, "/api/system/users", trace)
	resp.Body.Close()

	got := waitUntilPhase(t, watcher, trace, "PHASE_LOG")
	if !slices.Contains(got, "PHASE_LOG") {
		t.Fatalf("phases = %v; auditing a route Core serves itself is the ordinary "+
			"case and it did not run", got)
	}
	sizes := bodySizesSeen(t, watcher, trace)
	t.Logf("log phase on a Core route saw bodies: %v", sizes)
	if len(sizes) == 0 || sizes[len(sizes)-1] == "req=0/resp=0" {
		t.Errorf("the log filter saw no response body (%v); the response is captured "+
			"precisely so that it can, and an audit record without the response is "+
			"half a record", sizes)
	}
}

// post_handler does not run for a route Core serves itself, and that is the
// contract rather than an oversight.
//
// By the time the phase could run, the response has already gone to the
// client: the recorder writes through as it captures, deliberately, because
// wrapping a ResponseWriter hides http.Flusher and buffering would turn every
// streaming response into a stalled one — the console's own event stream
// included. Holding a response back so a filter can reshape it is possible for
// a plugin backend, whose response Core has in hand before it writes anything,
// and not for a handler Core is streaming from.
//
// So this pins the limitation rather than wishing it away. A filter that needs
// to see Core's own responses uses the log phase, which does run and does get
// the body. If this test ever fails, the behaviour changed and the guide's
// "post_handler 不会在 Core 自己的路由上运行" needs to change with it.
func TestPostHandlerDoesNotRunForACoreRoute(t *testing.T) {
	url, watcher := coreRouteGateway(t, manifest.PhasePostHandler, manifest.PhaseLog)

	const trace = "core-posthandler"
	resp := requestWithTrace(t, url, "/api/system/users", trace)
	resp.Body.Close()

	// Wait on the log phase, which does run, so this is not asserting an
	// absence that simply had not arrived yet.
	got := waitUntilPhase(t, watcher, trace, "PHASE_LOG")
	t.Logf("filter on /** declaring both phases, request to a route Core serves: %v", got)

	if !slices.Contains(got, "PHASE_LOG") {
		t.Fatalf("phases = %v; the log phase did not run either, so this proves nothing "+
			"about post_handler", got)
	}
	if slices.Contains(got, "PHASE_POST_HANDLER") {
		t.Errorf("phases = %v; post_handler ran for a route Core serves itself. That may "+
			"be an improvement, but the response is written through as it is produced, "+
			"so a filter running here cannot change what the client received — check "+
			"whether it can now, and update the guide either way", got)
	}
}
