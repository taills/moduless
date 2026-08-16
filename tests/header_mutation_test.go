package tests

import (
	"net/http"
	"testing"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// The four header mutations, end to end.
//
// FilterResult has six mutation methods and four of them — SetResponseHeader,
// RemoveRequestHeader, RemoveResponseHeader and RewritePath — had nothing
// exercising them anywhere in the repository. RewritePath turned out to be
// half broken. These three are the rest of that set.
//
// Each is checked against the side that can actually confirm it: a request
// header only the backend can report on, a response header only the client
// receives. Asking the filter what it did would confirm nothing — the whole
// question is whether Core applied it.

func headerMutationStack(t *testing.T) (url string, backend *pluginhost.Instance) {
	t.Helper()

	mutator, err := launchWithEnv(t, "mutator", "ECHO_MUTATE_HEADERS=1")
	if err != nil {
		t.Fatalf("launching the mutating filter: %v", err)
	}
	t.Cleanup(mutator.Kill)

	backend = launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "mutator",
		Instances: []*pluginhost.Instance{mutator},
		Filters: compileFilters(t, "mutator",
			manifest.FilterDecl{
				Name: "req", Phase: manifest.PhasePreRoute, TimeoutMS: 2000,
				Match: manifest.FilterMatch{Paths: []string{"/**"}},
			},
			manifest.FilterDecl{
				Name: "resp", Phase: manifest.PhasePostHandler, TimeoutMS: 2000,
				Match: manifest.FilterMatch{Paths: []string{"/**"}},
			},
		),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL, backend
}

func TestHeaderMutationsReachBothSides(t *testing.T) {
	url, _ := headerMutationStack(t)

	req, err := http.NewRequest(http.MethodGet, url+"/api/plugins/hello/items", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	// Sent by the client and removed by the filter, so "the backend did not
	// see it" means the removal happened rather than that it was never there.
	req.Header.Set("X-Remove-Me", "sent-by-client")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	t.Run("SetRequestHeader reaches the backend", func(t *testing.T) {
		if got := resp.Header.Get("X-Saw-Probe"); got != "from-filter" {
			t.Errorf("the backend saw X-Probe = %q, want %q: a header a filter added "+
				"before routing has to be there when the request is handled, or "+
				"mutation is advisory", got, "from-filter")
		}
	})

	t.Run("RemoveRequestHeader keeps it from the backend", func(t *testing.T) {
		if got := resp.Header.Get("X-Saw-Removed"); got != "" {
			t.Errorf("the backend still saw X-Remove-Me = %q; a filter that strips a "+
				"header — a forged X-Forwarded-For, an internal marker a caller must "+
				"not set — has to actually strip it", got)
		}
	})

	t.Run("SetResponseHeader reaches the client", func(t *testing.T) {
		if got := resp.Header.Get("X-Added-Response"); got != "from-filter" {
			t.Errorf("X-Added-Response = %q, want %q", got, "from-filter")
		}
	})

	t.Run("RemoveResponseHeader keeps it from the client", func(t *testing.T) {
		// The backend always sets X-Multi, so its absence is the removal.
		if got := resp.Header.Values("X-Multi"); len(got) != 0 {
			t.Errorf("X-Multi = %v; the post_handler filter removed it and the client "+
				"received it anyway", got)
		}
	})
}

// The control: without the mutating filter, the backend sees what the client
// sent and the response carries what the backend set. Every assertion above is
// about a difference, and a difference needs both sides.
func TestWithoutAFilterHeadersPassThroughUnchanged(t *testing.T) {
	backend := launchPlugin(t, "hello", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})
	srv := newGateway(reg)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/plugins/hello/items", nil)
	req.Header.Set("X-Remove-Me", "sent-by-client")
	req.Header.Set("X-Probe", "sent-by-client")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("X-Saw-Removed"); got != "sent-by-client" {
		t.Errorf("the backend saw X-Remove-Me = %q with no filter installed; the removal "+
			"test above would pass for the wrong reason", got)
	}
	if got := resp.Header.Get("X-Saw-Probe"); got != "sent-by-client" {
		t.Errorf("the backend saw X-Probe = %q with no filter installed", got)
	}
	if got := resp.Header.Values("X-Multi"); len(got) == 0 {
		t.Errorf("X-Multi was absent with no filter installed; the removal test above " +
			"would pass for the wrong reason")
	}
}
