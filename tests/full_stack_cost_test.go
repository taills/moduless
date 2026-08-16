package tests

import (
	"net/http"
	"os"
	"sort"
	"testing"
	"time"
)

// What a realistic installation costs.
//
// Two numbers have been measured in isolation and neither answers the question
// an operator actually has. A filter costs about 48µs each, measured one at a
// time against a fixture that does nothing. A plugin process costs about 19MB,
// measured with twenty copies of the same binary — and that measurement's own
// caveat was that real deployments run different binaries with no shared text,
// so the figure should hold but had never been checked against six.
//
// This measures the composed thing: six shipped examples, five of them with
// filters on the request path, serving real routes.
//
//	MEASURE=1 TEST_DATABASE_URL=... go test ./tests/ -run TestFullStackCost -v

func TestFullStackCost(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE is not set")
	}

	baseRSS, baseCount := childRSS(t)

	url, done := sixPluginStack(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "600000", "burst": "50000"},
		"apikey":    {"protected_prefixes": "", "cache_ttl": "60s", "local_ttl": "5s"},
		"redact":    {"fields": "email", "mask": "[gone]"},
	})
	defer done()

	// Settle: every plugin has finished Configure and its first allocations.
	time.Sleep(500 * time.Millisecond)
	rss, count := childRSS(t)
	plugins := count - baseCount

	t.Logf("six different plugin binaries: %d process(es), %.1f MB total, %.1f MB each",
		plugins, float64(rss-baseRSS)/1024, float64(rss-baseRSS)/float64(max(plugins, 1))/1024)

	client := warmClientPlain()
	measure := func(path string, n int) (p50, p99 time.Duration) {
		// Warm the connection and any per-route caches.
		for range 20 {
			resp, err := client.Get(url + path)
			if err == nil {
				resp.Body.Close()
			}
		}
		got := make([]time.Duration, 0, n)
		for range n {
			start := time.Now()
			resp, err := client.Get(url + path)
			if err != nil {
				continue
			}
			resp.Body.Close()
			got = append(got, time.Since(start))
		}
		if len(got) == 0 {
			return 0, 0
		}
		sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
		at := func(q float64) time.Duration { return got[int(float64(len(got)-1)*q)] }
		return at(0.50), at(0.99)
	}

	t.Logf("%-46s %-12s %s", "route", "p50", "p99")
	// Labelled by what each route actually does, which took a second look:
	// notes/stats was first written down here as "no database work" and runs
	// a Count through the reverse channel. A mislabelled row is how a
	// measurement becomes a wrong conclusion — "notes is slow" rather than
	// "a database round trip through the reverse channel costs about a
	// millisecond".
	for _, route := range []struct{ name, path string }{
		// Not a plugin route: the matching filters still run, no backend does.
		{"a path no plugin owns: filters only", "/nothing"},
		{"a plugin call, no host capability used", "/api/plugins/ratelimit/stats"},
		{"+ a Count through the reverse channel", "/api/plugins/notes/stats"},
		{"+ a listing query through the reverse channel", "/api/plugins/inventory/items"},
	} {
		p50, p99 := measure(route.path, 300)
		t.Logf("%-46s %-12s %s", route.name,
			p50.Round(time.Microsecond), p99.Round(time.Microsecond))
	}
}

// A floor that runs by default: with every example installed, a request is
// still served in a time a person would accept.
//
// Not a benchmark — the numbers above are for that. This is here so a change
// that makes the composed path pathological is noticed without anyone
// remembering to run the measurement, which is the same reason
// TestOnePluginStartsPromptly exists.
func TestFullStackStaysResponsive(t *testing.T) {
	url, done := sixPluginStack(t, map[string]map[string]string{
		"ratelimit": {"requests_per_minute": "600000", "burst": "50000"},
		"apikey":    {"protected_prefixes": "", "cache_ttl": "60s", "local_ttl": "5s"},
		"redact":    {"fields": "email", "mask": "[gone]"},
	})
	defer done()

	client := warmClientPlain()
	const path = "/api/plugins/ratelimit/stats"
	if code := getStatus(t, client, url+path); code != http.StatusOK {
		t.Fatalf("status = %d before measuring", code)
	}

	start := time.Now()
	const n = 100
	for range n {
		if code := getStatus(t, client, url+path); code != http.StatusOK {
			t.Fatalf("status = %d during the run", code)
		}
	}
	per := time.Since(start) / n

	t.Logf("%s per request through the full six-plugin stack", per.Round(time.Microsecond))

	// Far above what it costs (hundreds of microseconds) and far below what
	// would be tolerable. What this rules out is a composed path that has
	// become milliseconds per request without anyone noticing.
	if per > 20*time.Millisecond {
		t.Errorf("%s per request with six plugins installed; the composed path has "+
			"become something an operator would feel", per)
	}
}
