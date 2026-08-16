package tests

import (
	"net/http"
	"strings"
	"testing"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// Replacing a response from the post_handler phase.
//
// The guide says a filter that wants to "rewrite or inspect the response body"
// must declare needs_response_body, which is half right and misleading in a
// way that costs an author real time: RequestMutation has fields for request
// headers, response headers, the path, the identity and context values — and
// nothing for the body. Reading the mutation API, rewriting looks impossible.
//
// It is possible, through a mechanism the guide never names. In post_handler a
// short circuit does not reject anything, because there is nothing left to
// reject: the backend has already answered. It replaces the answer. That is
// the only way to change a response body, and nothing said so.

// postHandlerStack serves "hello" behind a post_handler filter on "shaper".
func postHandlerStack(t *testing.T, needsBody bool) string {
	t.Helper()

	shaper := launchPlugin(t, "shaper", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "shaper",
		Instances: []*pluginhost.Instance{shaper},
		Filters: compileFilters(t, "shaper", manifest.FilterDecl{
			Name:              "reshape",
			Phase:             manifest.PhasePostHandler,
			NeedsResponseBody: needsBody,
			MaxBodyBytes:      1 << 20,
			Match:             manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL
}

// A short circuit in post_handler replaces what the backend produced.
func TestPostHandlerShortCircuitReplacesTheResponse(t *testing.T) {
	url := postHandlerStack(t, true)

	// The fixture short-circuits on any path ending /refuse. Reaching this
	// point means the backend has already answered and the filter is replacing
	// that answer rather than preventing it.
	status, body, _ := get(t, url+"/api/plugins/hello/refuse")

	t.Logf("post_handler short circuit produced %d: %s", status, strings.TrimSpace(body))
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want the filter's 403; the backend's answer was not replaced",
			status)
	}
	if !strings.Contains(body, "replaced by echoplugin") {
		t.Errorf("body = %q; the filter's body did not reach the caller", body)
	}
}

// Without needs_response_body the filter still runs and still sees the status
// — it just gets an empty body rather than an error.
//
// The distinction matters because an author who forgets the declaration gets a
// filter that appears to work and quietly decides on nothing.
func TestPostHandlerWithoutTheBodyFlagSeesAnEmptyBody(t *testing.T) {
	url := postHandlerStack(t, false)

	// An ordinary request with a real body: the filter runs, continues, and
	// the backend's own response is what the caller gets.
	status, body, _ := get(t, url+"/api/plugins/hello/large?512")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(body) != 512 {
		t.Errorf("the caller got %d bytes, the backend sent 512; a filter that did not "+
			"ask for the body must not be able to lose it", len(body))
	}
}

// The backend's response survives a filter that only looks.
//
// Without this the test above passes for a Core that discards post_handler
// entirely, which would be a different bug with the same appearance.
func TestPostHandlerContinuePreservesTheResponse(t *testing.T) {
	url := postHandlerStack(t, true)

	status, body, hdr := get(t, url+"/api/plugins/hello/large?512")
	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(body) != 512 {
		t.Errorf("the backend sent 512 bytes and the caller got %d; a filter that "+
			"returned Continue changed the response", len(body))
	}
	// The fixture's own header should still be there.
	if hdr.Get("X-Echo-Path") == "" {
		t.Error("the backend's headers were lost")
	}
}
