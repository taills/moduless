package tests

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// Outbound HTTP, end to end through a real plugin process.
//
// The egress guard has thorough unit tests. What they cannot show is that a
// plugin actually goes through it — a plugin makes outbound requests by asking
// Core, and if that path were bypassable the allow-list would be decoration.
//
// One thing shapes every test here: the guard refuses any address that resolves
// into a private or loopback range, and every server a test can start locally
// is on 127.0.0.1. So the success path cannot be exercised without weakening
// the very check under test, and it is not weakened here. What is left is the
// refusals, which is the half that matters for a security boundary anyway:
// a guard that allows too much fails silently, a guard that refuses too much
// fails loudly.

func egressPlugin(t *testing.T, key string, granted, allow []string) *pluginhost.Instance {
	t.Helper()

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        key,
		InstanceID: key + "-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New(key, granted, hostsvc.Deps{
			Config: hostsvc.NewStaticConfig(),
			Egress: hostsvc.NewHTTPEgress(func(string) []string { return allow }),
		}),
		GrantedPermissions: granted,
		Env:                []string{"PATH=/usr/bin:/bin"},
		Stderr:             os.Stderr,
		DevMode:            true,
	})
	if err != nil {
		t.Fatalf("launch %s: %v", key, err)
	}
	t.Cleanup(inst.Kill)
	return inst
}

func fetchVia(t *testing.T, inst *pluginhost.Instance, target string) *pb.HttpResponse {
	t.Helper()
	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet,
		Path:   "/fetch",
		Query:  target,
	})
	if err != nil {
		t.Fatalf("calling the plugin: %v", err)
	}
	return resp
}

// Without the permission there is no outbound HTTP at all, whatever the
// allow-list says.
func TestEgressRequiresPermissionEndToEnd(t *testing.T) {
	inst := egressPlugin(t, "nonet", nil, []string{"example.com"})

	resp := fetchVia(t, inst, "https://example.com/")
	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin made an outbound request without the http:egress permission")
	}
	body := string(resp.GetBody())
	t.Logf("refused: %s", body)
	if !strings.Contains(body, "http:egress") {
		t.Errorf("the refusal does not name the missing permission: %q", body)
	}
}

// A host that is not on the plugin's allow-list is refused even with the
// permission. The permission grants the capability; the allow-list bounds it.
func TestEgressRefusesUnlistedHostEndToEnd(t *testing.T) {
	inst := egressPlugin(t, "netplugin", []string{"http:egress"}, []string{"allowed.example.com"})

	resp := fetchVia(t, inst, "https://not-allowed.example.com/secrets")
	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin reached a host that was not on its allow-list")
	}
	t.Logf("refused: %s", resp.GetBody())
}

// The check that matters most: a host the plugin IS allowed to reach, which
// resolves to an address it must not. This is the shape of both an SSRF via
// DNS and a simple misconfiguration, and the allow-list alone cannot catch it
// — only checking the address actually dialled can.
func TestEgressRefusesAllowedHostResolvingInternally(t *testing.T) {
	// A real server, on loopback, on the plugin's allow-list.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this must never reach a plugin"))
	}))
	defer upstream.Close()

	host := strings.TrimPrefix(upstream.URL, "http://")
	hostname, _, _ := strings.Cut(host, ":")

	inst := egressPlugin(t, "netplugin", []string{"http:egress"}, []string{hostname})

	resp := fetchVia(t, inst, upstream.URL+"/")
	body := string(resp.GetBody())
	t.Logf("allow-listed loopback host: status %d, %s", resp.GetStatusCode(), body)

	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin reached a loopback address that was on its allow-list; " +
			"the dial-time address check is not running")
	}
	if strings.Contains(body, "this must never reach a plugin") {
		t.Error("the upstream body reached the plugin")
	}
}

// The cloud metadata endpoint is the canonical SSRF target: it hands out
// credentials to anything that can make an HTTP request from inside the
// network. It is on the allow-list here on purpose — being listed must not be
// enough.
//
// Only the literal address is used. A hostname would make this test depend on
// what the local resolver does with it, and outside a cloud environment that
// is a DNS failure rather than a refusal — the test would pass without the
// guard ever running.
func TestEgressRefusesCloudMetadata(t *testing.T) {
	inst := egressPlugin(t, "netplugin", []string{"http:egress"}, []string{"169.254.169.254"})

	resp := fetchVia(t, inst, "http://169.254.169.254/latest/meta-data/iam/security-credentials/")
	body := string(resp.GetBody())
	t.Logf("refused: %s", body)

	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin reached the cloud metadata endpoint")
	}
	// It must be refused by the address check, not by the request happening to
	// fail. On a machine with no route to it, a timeout would look like
	// success to a weaker assertion.
	if !strings.Contains(body, "link-local") {
		t.Errorf("refused for the wrong reason: %q; the address guard did not run", body)
	}
}

// Non-HTTP schemes must be refused before anything is dialled. file:// would
// read Core's own filesystem through the proxy; gopher:// is the classic way
// to speak an arbitrary protocol through an HTTP client.
func TestEgressRefusesNonHTTPSchemesEndToEnd(t *testing.T) {
	inst := egressPlugin(t, "netplugin", []string{"http:egress"}, []string{"*"})

	for _, target := range []string{
		"file:///etc/passwd",
		"gopher://127.0.0.1:6379/_INFO",
		"ftp://example.com/",
	} {
		t.Run(target, func(t *testing.T) {
			resp := fetchVia(t, inst, target)
			if resp.GetStatusCode() == 200 {
				t.Errorf("a plugin fetched %s", target)
			}
			t.Logf("refused: %s", resp.GetBody())
		})
	}
}

// A plugin cannot reach a redirector on loopback either — the dial guard
// stops the first hop, before any redirect is involved.
//
// Whether the redirect itself is followed cannot be established here: every
// server a test can start is on loopback, so the first hop never succeeds and
// a test at this level would pass without the redirect policy running at all.
// That policy is tested directly in core/hostsvc, where the transport can be
// isolated from the dial guard.
func TestEgressRefusesLoopbackRedirector(t *testing.T) {
	var reached bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte("internal service"))
	}))
	defer internal.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/", http.StatusFound)
	}))
	defer redirector.Close()

	host, _, _ := strings.Cut(strings.TrimPrefix(redirector.URL, "http://"), ":")
	inst := egressPlugin(t, "netplugin", []string{"http:egress"}, []string{host})

	resp := fetchVia(t, inst, redirector.URL+"/")
	body := string(resp.GetBody())
	t.Logf("status %d, %s", resp.GetStatusCode(), body)

	if resp.GetStatusCode() == 200 {
		t.Fatal("a plugin reached a loopback redirector")
	}
	if !strings.Contains(body, "loopback") {
		t.Errorf("refused for the wrong reason: %q", body)
	}
	if reached {
		t.Error("the redirect target was reached")
	}
}

// A refused request must not take the plugin or Core down with it: the plugin
// keeps serving, and a later legitimate call still works.
func TestEgressRefusalIsRecoverable(t *testing.T) {
	inst := egressPlugin(t, "netplugin", []string{"http:egress"}, []string{"allowed.example.com"})

	for i := range 20 {
		resp := fetchVia(t, inst, fmt.Sprintf("http://127.0.0.1:1/attempt-%d", i))
		if resp.GetStatusCode() == 200 {
			t.Fatalf("attempt %d was allowed through", i)
		}
	}

	// The plugin is still healthy and serving its own routes.
	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/items",
	})
	if err != nil {
		t.Fatalf("the plugin stopped responding after refused requests: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Errorf("status = %d after 20 refusals", resp.GetStatusCode())
	}
}
