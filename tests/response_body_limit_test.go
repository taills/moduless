package tests

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// max_body_bytes on a response-inspecting filter.
//
// The request side enforces it: a body over the declared size skips a
// fail-open filter and refuses a fail-closed one (see body_limit_test.go).
// buildFilterRequest attaches the response body under a separate flag, and the
// size check in runFilter reads only NeedsRequestBody — so whether the
// declared cap means anything on the response side had never been established.
//
// It matters because the shipped redact example is exactly this shape:
// needs_response_body with max_body_bytes: 262144, on a filter whose job is to
// strip sensitive fields out of what the browser is about to receive. If the
// cap were enforced the way the request side enforces it, a large enough
// response would skip redaction — the client picks the size by asking for more
// rows. If it is not enforced, the declaration is telling the author something
// untrue about what they will be handed.

// bodySizesSeen asks the plugin how much body each of its filter calls got.
func bodySizesSeen(t *testing.T, inst *pluginhost.Instance, traceID string) []string {
	t.Helper()

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/bodysizes", Query: traceID,
	})
	if err != nil {
		t.Fatalf("asking for body sizes: %v", err)
	}
	return strings.Fields(strings.TrimSpace(string(resp.GetBody())))
}

func TestResponseBodyIgnoresTheDeclaredLimit(t *testing.T) {
	const (
		declared = 1024
		actual   = 64 * 1024
	)

	inspector := launchPlugin(t, "inspector", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "inspector",
		Instances: []*pluginhost.Instance{inspector},
		Filters: compileFilters(t, "inspector", manifest.FilterDecl{
			Name:              "scan",
			Phase:             manifest.PhasePostHandler,
			TimeoutMS:         2000,
			NeedsResponseBody: true,
			MaxBodyBytes:      declared,
			Match:             manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key: "hello", Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	defer srv.Close()

	const trace = "resp-body-limit"
	resp := requestWithTrace(t, srv.URL, "/api/plugins/hello/large?"+itoa(actual), trace)
	resp.Body.Close()

	sizes := bodySizesSeen(t, inspector, trace)
	t.Logf("declared max_body_bytes=%d, backend returned %dB, filter received %v",
		declared, actual, sizes)

	if len(sizes) == 0 {
		t.Fatal("the response filter was never called, so this measures nothing")
	}

	// One of two things is true and the test reports which.
	got := sizes[len(sizes)-1]
	switch {
	case got == "req=0/resp="+itoa(actual):
		t.Logf("max_body_bytes is not enforced for response bodies: the filter was " +
			"handed the whole response. No bypass — redaction still runs — but the " +
			"declaration promises a cap that does not exist, and the full body " +
			"crosses the process boundary either way")
	case strings.HasSuffix(got, "resp="+itoa(declared)):
		t.Errorf("the filter received exactly the declared limit (%s): the response was "+
			"truncated to fit, which is the case the pipeline design set out to avoid — "+
			"the filter decides on data that differs from what the client receives", got)
	default:
		t.Errorf("the filter received %s, which is neither the whole response nor the "+
			"declared limit", got)
	}
}

// itoa without pulling strconv into the test's import list for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
