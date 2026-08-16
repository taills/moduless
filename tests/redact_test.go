package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// The redact example, end to end through the gateway.
//
// It is the only example that changes what another plugin said, and the
// mechanism it depends on — a short circuit in post_handler replacing the
// response rather than preventing it — is not visible from the mutation API.
// So the thing worth checking is not that the code compiles but that a field
// really disappears from a real response served by a different plugin.

// redactStack installs the shipped redact example in front of the echo
// fixture, configured to remove the fields the test cares about.
func redactStack(t *testing.T, fields string) string {
	t.Helper()

	root := t.TempDir()
	installExampleAs(t, root, "redact", "redact", "../extension-example/redact")

	pkg, err := pluginhost.LoadPackage(root + "/redact")
	if err != nil {
		t.Fatalf("loading the redact example: %v", err)
	}

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        "redact",
		InstanceID: "redact-0",
		Version:    "1.0.0",
		BinaryPath: root + "/redact/bin/plugin",
		Checksum:   checksum(t, root+"/redact/bin/plugin"),
		HostImpl: hostsvc.New("redact", nil, hostsvc.Deps{
			Config: hostsvc.ConfigFunc(func(context.Context, string) (map[string]string, error) {
				return map[string]string{"fields": fields, "mask": "[gone]"}, nil
			}),
		}),
		// Both paths, deliberately. OnConfigChanged is called from Configure
		// with LaunchSpec.Config; Deps.Config answers a later GetConfig. Core
		// feeds both from one place so they cannot disagree, and a test that
		// sets only the second gets a plugin that behaves as if unconfigured —
		// which is how the first run of this test "passed" the
		// nothing-configured case and failed the other two.
		Config: map[string]string{"fields": fields, "mask": "[gone]"},
		Env:    []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(inst.Kill)

	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	// The example's own declarations, compiled from its manifest — so the test
	// exercises what ships rather than a filter the test invented.
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "redact",
		Instances: []*pluginhost.Instance{inst},
		Filters:   widenPaths(t, pkg),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	srv := newGateway(reg)
	t.Cleanup(srv.Close)
	return srv.URL
}

// widenPaths recompiles the example's filters against the fixture's route.
//
// The manifest deliberately scopes the filter to the plugins that return
// personal data rather than to /**, which is the right default and the wrong
// thing for a test whose backend is called "hello". Everything else about the
// declaration — the phase, needs_response_body, the limits — is the shipped
// one.
func widenPaths(t *testing.T, pkg *pluginhost.Package) []manifest.CompiledFilter {
	t.Helper()

	decls := pkg.Manifest.Filters
	if len(decls) != 1 {
		t.Fatalf("the example declares %d filters; this helper assumes one", len(decls))
	}
	d := decls[0]
	d.Match.Paths = []string{"/**"}
	return compileFilters(t, "redact", d)
}

// A configured field disappears from another plugin's response.
func TestRedactRemovesAFieldFromAnotherPluginsResponse(t *testing.T) {
	url := redactStack(t, "email")

	status, body, hdr := get(t, url+"/api/plugins/hello/json")
	if status != http.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	t.Logf("response after redaction: %s", body)

	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the response is no longer JSON: %v (%q)", err, body)
	}
	if got := doc["email"]; got != "[gone]" {
		t.Errorf("email = %v, want the mask; the field was not redacted", got)
	}
	if doc["name"] != "Ada" {
		t.Errorf("name = %v; a field nobody asked to remove was changed", doc["name"])
	}

	// The trap this example exists to demonstrate: a replacement carries only
	// its own headers unless the filter copies them. The example copies them.
	if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q after a replacement; the backend's headers were "+
			"dropped, and a browser would render this as text", ct)
	}
}

// Nested fields go too, at any depth. A redactor that only handles the top
// level is worse than none: it reads as working.
func TestRedactReachesNestedFields(t *testing.T) {
	url := redactStack(t, "phone")

	_, body, _ := get(t, url+"/api/plugins/hello/json")
	if strings.Contains(body, "555-0100") {
		t.Errorf("a nested field survived: %s", body)
	}
	if !strings.Contains(body, "[gone]") {
		t.Errorf("nothing was masked: %s", body)
	}
}

// With nothing configured to remove, the response is passed through untouched
// — byte for byte, not merely equivalent.
//
// This is the other half of the mechanism: the example returns Continue when
// there is nothing to do, rather than Stop with a re-encoded copy. Re-encoding
// every response in the system would reorder keys, drop formatting, and cost
// the header copy on traffic that needed none of it.
func TestRedactLeavesUnconfiguredResponsesAlone(t *testing.T) {
	plain := redactStack(t, "")
	_, body, hdr := get(t, plain+"/api/plugins/hello/json")

	const original = `{"id":"7","name":"Ada","email":"ada@example.com","contacts":[{"phone":"555-0100"}]}`
	if body != original {
		t.Errorf("the response was rewritten with nothing configured:\n got %s\nwant %s",
			body, original)
	}
	if hdr.Get("X-Echo-Path") == "" {
		t.Error("the backend's own header is missing from an untouched response")
	}
}

// A response that is not JSON is left alone rather than mangled.
func TestRedactIgnoresNonJSON(t *testing.T) {
	url := redactStack(t, "email")

	_, body, _ := get(t, url+"/api/plugins/hello/large?128")
	if len(body) != 128 {
		t.Errorf("a non-JSON response came back as %d bytes, sent 128", len(body))
	}
}
