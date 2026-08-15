package hostsvc

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func allowList(patterns ...string) func(string) []string {
	return func(string) []string { return patterns }
}

func TestHostAllowed(t *testing.T) {
	tests := []struct {
		name     string
		host     string
		patterns []string
		want     bool
	}{
		{name: "exact match", host: "api.example.com", patterns: []string{"api.example.com"}, want: true},
		{name: "case insensitive", host: "API.Example.COM", patterns: []string{"api.example.com"}, want: true},
		{name: "trailing dot", host: "api.example.com.", patterns: []string{"api.example.com"}, want: true},
		{name: "wildcard subdomain", host: "cdn.example.net", patterns: []string{"*.example.net"}, want: true},
		{name: "wildcard deep subdomain", host: "a.b.example.net", patterns: []string{"*.example.net"}, want: true},
		{name: "not listed", host: "evil.com", patterns: []string{"api.example.com"}, want: false},
		{name: "empty allow list", host: "api.example.com", want: false},
		{
			// The wildcard must not match the bare domain, and more
			// importantly must not match a lookalike registered elsewhere.
			name: "wildcard does not match a suffix lookalike",
			host: "notexample.net", patterns: []string{"*.example.net"}, want: false,
		},
		{
			name: "substring is not a match",
			host: "api.example.com.evil.com", patterns: []string{"api.example.com"}, want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostAllowed(tc.host, tc.patterns); got != tc.want {
				t.Errorf("hostAllowed(%q, %v) = %v, want %v", tc.host, tc.patterns, got, tc.want)
			}
		})
	}
}

func TestEgressRejectsNonHTTPSchemes(t *testing.T) {
	e := NewHTTPEgress(allowList("*"))
	ctx := context.Background()

	for _, raw := range []string{
		"file:///etc/passwd",
		"gopher://example.com/",
		"ftp://example.com/x",
		"",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := e.Fetch(ctx, "p", EgressRequest{URL: raw}); err == nil {
				t.Errorf("Fetch accepted %q", raw)
			}
		})
	}
}

func TestEgressRejectsUnlistedHost(t *testing.T) {
	e := NewHTTPEgress(allowList("api.example.com"))

	_, err := e.Fetch(context.Background(), "p", EgressRequest{URL: "https://evil.example.org/"})
	if err == nil {
		t.Fatal("Fetch reached a host that is not in the allow list")
	}
	if !strings.Contains(err.Error(), "egress_allow") {
		t.Errorf("error = %v; it should name the manifest field to fix", err)
	}
}

// The allow-list alone is not enough. A permitted hostname can resolve to a
// loopback or metadata address, so the address actually dialled is checked.
// This is the difference between an allow-list and an SSRF guard.
func TestEgressRefusesInternalAddressesDespiteAllowList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this should never be reachable"))
	}))
	defer srv.Close()

	// Deliberately permissive: the host resolves to 127.0.0.1 and is on the
	// list, so only the dial-time check can stop this.
	host := strings.TrimPrefix(srv.URL, "http://")
	hostname, _, _ := strings.Cut(host, ":")
	e := NewHTTPEgress(allowList(hostname))

	_, err := e.Fetch(context.Background(), "p", EgressRequest{URL: srv.URL, Timeout: 3 * time.Second})
	if err == nil {
		t.Fatal("an allow-listed host resolving to loopback was reachable")
	}
	if !strings.Contains(err.Error(), "refusing to connect") {
		t.Errorf("error = %v, want the dial guard to refuse it", err)
	}
}

func TestBlockedIPCoversTheUsualTargets(t *testing.T) {
	tests := []struct {
		ip      string
		blocked bool
	}{
		{ip: "127.0.0.1", blocked: true},
		{ip: "::1", blocked: true},
		{ip: "10.0.0.5", blocked: true},
		{ip: "192.168.1.10", blocked: true},
		{ip: "172.16.0.1", blocked: true},
		// The cloud instance metadata endpoint, the classic SSRF prize.
		{ip: "169.254.169.254", blocked: true},
		{ip: "0.0.0.0", blocked: true},
		{ip: "224.0.0.1", blocked: true},
		{ip: "fd00::1", blocked: true},
		{ip: "8.8.8.8", blocked: false},
		{ip: "93.184.216.34", blocked: false},
	}

	for _, tc := range tests {
		t.Run(tc.ip, func(t *testing.T) {
			got, why := blockedIP(parseIP(t, tc.ip))
			if got != tc.blocked {
				t.Errorf("blockedIP(%s) = %v (%s), want %v", tc.ip, got, why, tc.blocked)
			}
		})
	}
}

func TestEgressRateLimit(t *testing.T) {
	e := NewHTTPEgress(allowList("example.com"))
	e.RatePerMinute = 3

	for i := range 3 {
		if !e.allowRate("p") {
			t.Fatalf("request %d was rate limited below the limit", i+1)
		}
	}
	if e.allowRate("p") {
		t.Error("the limit was not enforced")
	}
	// Limits are per plugin, so one noisy plugin does not throttle the others.
	if !e.allowRate("other") {
		t.Error("a different plugin was throttled by this one's usage")
	}
}

func TestEgressAuditsRefusals(t *testing.T) {
	type record struct {
		url    string
		status int
		failed bool
	}
	var seen []record

	e := NewHTTPEgress(allowList("api.example.com"))
	e.OnRequest = func(_, _, url string, status int, err error) {
		seen = append(seen, record{url: url, status: status, failed: err != nil})
	}

	_, _ = e.Fetch(context.Background(), "p", EgressRequest{URL: "https://evil.example.org/"})

	if len(seen) != 1 {
		t.Fatalf("audited %d attempts, want 1", len(seen))
	}
	if !seen[0].failed {
		t.Error("a refused request was audited as successful")
	}
	// A refusal that is not recorded is exactly the one an operator needs to
	// see, so it must reach the audit hook rather than being dropped early.
	if seen[0].url != "https://evil.example.org/" {
		t.Errorf("audited url = %q", seen[0].url)
	}
}

func TestHopByHopHeadersAreNotForwarded(t *testing.T) {
	for _, h := range []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Host"} {
		if !isHopByHop(h) {
			t.Errorf("%s should be treated as hop-by-hop", h)
		}
	}
	for _, h := range []string{"Authorization", "Content-Type", "X-Custom"} {
		if isHopByHop(h) {
			t.Errorf("%s should be forwarded", h)
		}
	}
}

func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("bad test IP %q", s)
	}
	return ip
}

// A redirect is a second request the plugin never asked for, aimed at a host
// nobody checked against the allow-list. The client must not follow it.
//
// This has to bypass guardedDial to be testable at all: every server a test
// can start is on loopback, which the dial guard refuses — correctly, and
// before the redirect logic would ever run. Swapping the transport isolates
// the redirect policy, which is the thing under test here; the dial guard has
// its own tests above.
func TestEgressDoesNotFollowRedirects(t *testing.T) {
	var internalReached bool
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		internalReached = true
		_, _ = w.Write([]byte("internal service"))
	}))
	defer internal.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/", http.StatusFound)
	}))
	defer redirector.Close()

	host, _, _ := strings.Cut(strings.TrimPrefix(redirector.URL, "http://"), ":")
	e := &HTTPEgress{AllowFor: func(string) []string { return []string{host} }}
	e.init()
	// Ordinary dialling, so the request reaches the test server; the redirect
	// policy set up by init is left exactly as production has it.
	e.client.Transport = &http.Transport{}

	resp, err := e.Fetch(context.Background(), "plug", EgressRequest{
		Method: http.MethodGet,
		URL:    redirector.URL + "/",
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if internalReached {
		t.Error("the redirect was followed to a host that was never checked against the allow-list")
	}
	if resp.StatusCode != http.StatusFound {
		t.Errorf("status = %d, want the 302 handed back unfollowed", resp.StatusCode)
	}
	if strings.Contains(string(resp.Body), "internal service") {
		t.Error("the redirect target's body reached the caller")
	}
	// The plugin can see where it was being sent, which is useful and safe —
	// what matters is that Core did not go there.
	t.Logf("unfollowed redirect: %d -> %s", resp.StatusCode, resp.Headers["Location"])
}
