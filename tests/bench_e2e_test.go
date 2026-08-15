package tests

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// End-to-end benchmarks: a real browser-shaped HTTP request, through the real
// gateway and filter pipeline, into a real plugin subprocess and back.
//
// The pipeline benchmarks elsewhere measure the pieces. These measure what a
// caller actually experiences, which is the number that matters when deciding
// whether this architecture is fast enough for a given workload.
//
//	go test ./tests/ -bench=E2E -benchtime=2000x -run=XXX

// benchGateway builds a running Core with one plugin and the given filters.
func benchGateway(b *testing.B, filterCount int) (url string, cleanup func()) {
	b.Helper()

	reg := pluginhost.NewRegistry()
	inst := launchPlugin(b, "hello", "1.0.0", nil)

	registration := pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
	}
	if filterCount > 0 {
		decls := make([]manifest.FilterDecl, 0, filterCount)
		for i := range filterCount {
			decls = append(decls, manifest.FilterDecl{
				Name:  fmt.Sprintf("f%d", i),
				Phase: manifest.PhasePreRoute,
				Order: i,
				Match: manifest.FilterMatch{Paths: []string{"/**"}},
			})
		}
		registration.Filters = compileFilters(b, "hello", decls...)
	}
	reg.InstallPlugin(registration)

	srv := newGateway(reg)
	return srv.URL, srv.Close
}

// warmClient returns a client that reuses connections, so the benchmark
// measures the framework rather than TCP handshakes.
func warmClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        256,
			MaxIdleConnsPerHost: 256,
			MaxConnsPerHost:     256,
		},
	}
}

func doGet(b *testing.B, client *http.Client, url string) {
	b.Helper()
	resp, err := client.Get(url)
	if err != nil {
		b.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		b.Fatalf("status = %d", resp.StatusCode)
	}
}

// BenchmarkE2EPluginRequest is the headline number: one HTTP request served by
// a plugin in another process.
func BenchmarkE2EPluginRequest(b *testing.B) {
	url, cleanup := benchGateway(b, 0)
	defer cleanup()

	client := warmClient()
	target := url + "/api/plugins/hello/items"
	doGet(b, client, target) // warm the connection pool

	b.ReportAllocs()
	for b.Loop() {
		doGet(b, client, target)
	}
}

// BenchmarkE2EFilterDepth shows what each additional filter in the chain
// costs, since every one of them is a separate cross-process round trip.
func BenchmarkE2EFilterDepth(b *testing.B) {
	for _, count := range []int{0, 1, 3, 5} {
		b.Run(fmt.Sprintf("filters_%d", count), func(b *testing.B) {
			url, cleanup := benchGateway(b, count)
			defer cleanup()

			client := warmClient()
			target := url + "/api/plugins/hello/items"
			doGet(b, client, target)

			b.ReportAllocs()
			for b.Loop() {
				doGet(b, client, target)
			}
		})
	}
}

// BenchmarkE2EConcurrent measures throughput with many requests in flight,
// which is the realistic gateway shape. A single plugin process multiplexes
// them over one HTTP/2 connection.
func BenchmarkE2EConcurrent(b *testing.B) {
	url, cleanup := benchGateway(b, 1)
	defer cleanup()

	client := warmClient()
	target := url + "/api/plugins/hello/items"
	doGet(b, client, target)

	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			resp, err := client.Get(target)
			if err != nil {
				b.Errorf("GET: %v", err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	})
}

// BenchmarkE2ECoreRoute is the control: a path Core serves itself, with the
// pipeline installed but no filter matching. The gap against the plugin
// benchmark is the cost of crossing the process boundary.
func BenchmarkE2ECoreRoute(b *testing.B) {
	url, cleanup := benchGateway(b, 0)
	defer cleanup()

	client := warmClient()
	target := url + "/not-a-plugin-route"

	b.ReportAllocs()
	for b.Loop() {
		resp, err := client.Get(target)
		if err != nil {
			b.Fatalf("GET: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkE2EPayloadSize shows how response size affects the round trip,
// which is what decides whether a large payload belongs in the file service
// instead of a plugin response.
func BenchmarkE2EPayloadSize(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{
		{name: "1KB", n: 1 << 10},
		{name: "64KB", n: 64 << 10},
		{name: "512KB", n: 512 << 10},
	}

	url, cleanup := benchGateway(b, 0)
	defer cleanup()
	client := warmClient()

	for _, s := range sizes {
		body := make([]byte, s.n)
		b.Run(s.name, func(b *testing.B) {
			b.SetBytes(int64(s.n))
			b.ReportAllocs()
			for b.Loop() {
				// The echo fixture returns the request body, so this measures
				// a payload travelling in both directions.
				resp, err := client.Post(url+"/api/plugins/hello/echo",
					"application/octet-stream", newRepeatReader(body))
				if err != nil {
					b.Fatalf("POST: %v", err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		})
	}
}

func newRepeatReader(b []byte) io.Reader { return &repeatReader{b: b} }

type repeatReader struct {
	b   []byte
	pos int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += n
	return n, nil
}

// TestE2EThroughputSnapshot records sustained throughput as a plain test, so
// a regression shows up in a normal test run rather than only when someone
// remembers to run benchmarks.
//
// The threshold is deliberately far below measured capacity: this guards
// against an order-of-magnitude regression, not against normal variance on a
// loaded machine.
func TestE2EThroughputSnapshot(t *testing.T) {
	if testing.Short() {
		t.Skip("throughput snapshot skipped in short mode")
	}

	reg := pluginhost.NewRegistry()
	inst := launchPlugin(t, "hello", "1.0.0", nil)
	reg.InstallPlugin(pluginhost.Registration{Key: "hello", Instances: []*pluginhost.Instance{inst}})

	srv := newGateway(reg)
	defer srv.Close()

	client := warmClient()
	target := srv.URL + "/api/plugins/hello/items"

	// Warm up before timing.
	for range 20 {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("warmup: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	const duration = 2 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	done := 0
	start := time.Now()
	for ctx.Err() == nil {
		resp, err := client.Get(target)
		if err != nil {
			break
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		done++
	}
	elapsed := time.Since(start)

	rps := float64(done) / elapsed.Seconds()
	t.Logf("sequential throughput: %.0f req/s over %s (%d requests)", rps, elapsed.Round(time.Millisecond), done)

	const floor = 500
	if rps < floor {
		t.Errorf("throughput fell to %.0f req/s, below the %d req/s floor; "+
			"this is an order-of-magnitude regression, not variance", rps, floor)
	}
}

// The cross-process call on its own, with no HTTP in front of it.
//
// The end-to-end numbers include Go's HTTP server, the pipeline, and the
// gateway's own work. This one isolates what a single gRPC round trip to a
// plugin actually costs, which is the floor everything else sits on: if a
// filter chain is to get cheaper, this is the number that has to move.
func BenchmarkPluginRPCOnly(b *testing.B) {
	inst := launchPlugin(b, "hello", "1.0.0", nil)
	ctx := context.Background()
	req := &pb.HttpRequest{Method: "GET", Path: "/items"}

	// Warm the connection.
	if _, err := inst.Client.HandleHTTP(ctx, req); err != nil {
		b.Fatalf("warmup: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := inst.Client.HandleHTTP(ctx, req); err != nil {
			b.Fatalf("HandleHTTP: %v", err)
		}
	}
}

// A filter call, which is the one a chain pays per hop.
func BenchmarkFilterRPCOnly(b *testing.B) {
	inst := launchPlugin(b, "hello", "1.0.0", nil)
	ctx := context.Background()
	req := &pb.FilterRequest{
		Phase:  pb.Phase_PHASE_PRE_ROUTE,
		Method: "GET",
		Path:   "/items",
	}

	if _, err := inst.Client.Filter(ctx, req); err != nil {
		b.Fatalf("warmup: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := inst.Client.Filter(ctx, req); err != nil {
			b.Fatalf("Filter: %v", err)
		}
	}
}

// How much of the round trip is payload rather than the crossing itself.
func BenchmarkRPCPayloadSize(b *testing.B) {
	inst := launchPlugin(b, "hello", "1.0.0", nil)
	ctx := context.Background()

	for _, size := range []int{0, 1 << 10, 16 << 10, 256 << 10} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			req := &pb.HttpRequest{Method: "POST", Path: "/echo", Body: make([]byte, size)}
			if _, err := inst.Client.HandleHTTP(ctx, req); err != nil {
				b.Fatalf("warmup: %v", err)
			}
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := inst.Client.HandleHTTP(ctx, req); err != nil {
					b.Fatalf("HandleHTTP: %v", err)
				}
			}
		})
	}
}

// The filter chain with a realistic number of request headers.
//
// A browser sends a dozen or so; the benchmarks above send three. Header count
// is the multiplier on any per-filter work that touches them, so this is where
// a difference would show if there is one.
func BenchmarkE2EFilterDepthWithHeaders(b *testing.B) {
	headers := map[string]string{
		"User-Agent":       "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)",
		"Accept":           "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language":  "en-GB,en;q=0.9,zh-CN;q=0.8",
		"Accept-Encoding":  "gzip, deflate, br",
		"Cookie":           "moduless_token=abcdef0123456789; theme=dark; sidebar=collapsed",
		"Referer":          "http://localhost:8080/apps/notes",
		"Sec-Fetch-Dest":   "empty",
		"Sec-Fetch-Mode":   "cors",
		"Sec-Fetch-Site":   "same-origin",
		"X-Requested-With": "XMLHttpRequest",
		"Cache-Control":    "no-cache",
		"Pragma":           "no-cache",
	}

	for _, count := range []int{1, 3, 5} {
		b.Run(fmt.Sprintf("filters_%d", count), func(b *testing.B) {
			url, cleanup := benchGateway(b, count)
			defer cleanup()

			client := warmClient()
			target := url + "/api/plugins/hello/items"

			do := func() {
				req, err := http.NewRequest(http.MethodGet, target, nil)
				if err != nil {
					b.Fatalf("request: %v", err)
				}
				for k, v := range headers {
					req.Header.Set(k, v)
				}
				resp, err := client.Do(req)
				if err != nil {
					b.Fatalf("GET: %v", err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
			do()

			b.ReportAllocs()
			for b.Loop() {
				do()
			}
		})
	}
}
