package tests

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// The console learns that a plugin was enabled or disabled over server-sent
// events. That makes this stream the mechanism behind the requirement that a
// plugin's menu appears and disappears without a page reload — and it is the
// piece most easily broken by a change somewhere else entirely.
//
// Specifically: the filter pipeline wraps the ResponseWriter when a
// post_handler or log filter is installed, and any wrapper hides the optional
// interfaces the real writer implements. Losing http.Flusher converts a
// streaming response into one that buffers until the handler returns, which for
// an endless event stream means it never arrives at all. The failure is silent,
// it only appears once someone installs an unrelated filter, and what breaks is
// the console's ability to report that very plugin.

// sseGateway builds a Core serving the events stream, optionally behind a
// filter chain that causes the response to be wrapped.
func sseGateway(t *testing.T, phases ...string) (url string, events *gateway.UIEvents, cleanup func()) {
	t.Helper()

	reg := pluginhost.NewRegistry()
	if len(phases) > 0 {
		decls := make([]manifest.FilterDecl, 0, len(phases))
		for i, phase := range phases {
			decls = append(decls, manifest.FilterDecl{
				Name:  "f" + string(rune('a'+i)),
				Phase: phase,
				Match: manifest.FilterMatch{Paths: []string{"/**"}},
			})
		}
		reg.InstallPlugin(pluginhost.Registration{
			Key:       "hello",
			Instances: []*pluginhost.Instance{launchPlugin(t, "hello", "1.0.0", nil)},
			Filters:   compileFilters(t, "hello", decls...),
		})
	}

	events = gateway.NewUIEvents()
	mux := http.NewServeMux()
	mux.HandleFunc(gateway.UIEventsPath, events.Handler)

	h := &gateway.PluginHandler{Registry: reg, Runner: &pipeline.Runner{}}
	srv := httptest.NewServer(h.Middleware(mux))
	return srv.URL, events, srv.Close
}

// openStream connects and returns a channel of event names.
//
// One goroutine owns the reader for the life of the stream. Starting a fresh
// reader per wait would let an earlier one consume events a later wait is
// looking for — the bytes are gone from the socket either way.
func openStream(t *testing.T, url string) (<-chan string, func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+gateway.UIEventsPath, nil)
	if err != nil {
		cancel()
		t.Fatalf("building request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("opening the stream: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		t.Fatalf("stream returned %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		resp.Body.Close()
		cancel()
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	events := make(chan string, 16)
	go func() {
		defer close(events)
		r := bufio.NewReader(resp.Body)
		for {
			line, err := r.ReadString('\n')
			if name, ok := strings.CutPrefix(strings.TrimSpace(line), "event: "); ok {
				events <- name
			}
			if err != nil {
				return
			}
		}
	}()

	return events, func() {
		cancel()
		resp.Body.Close()
	}
}

// awaitEvent waits for the next event, or reports that none arrived.
//
// The deadline is the whole point: a stream being buffered rather than flushed
// looks exactly like a stream with nothing to say, and only the passage of time
// tells them apart.
func awaitEvent(t *testing.T, events <-chan string, timeout time.Duration) (string, bool) {
	t.Helper()
	select {
	case ev, ok := <-events:
		return ev, ok
	case <-time.After(timeout):
		return "", false
	}
}

// The baseline: no filters, so nothing wraps the response.
func TestSSEDeliversWithoutFilters(t *testing.T) {
	url, events, cleanup := sseGateway(t)
	defer cleanup()

	stream, closeStream := openStream(t, url)
	defer closeStream()

	// Give the handler a moment to register before publishing, or the event
	// has no subscriber to reach.
	waitForSubscriber(t, events)
	events.Publish("plugins-changed")

	ev, ok := awaitEvent(t, stream, 3*time.Second)
	if !ok {
		t.Fatal("no event arrived within 3s on a stream with no filters installed")
	}
	if ev != "plugins-changed" {
		t.Errorf("event = %q, want plugins-changed", ev)
	}
}

// The regression: a post_handler filter makes the pipeline wrap the response.
// Events must still arrive immediately rather than being buffered.
func TestSSEDeliversBehindPostHandlerFilter(t *testing.T) {
	url, events, cleanup := sseGateway(t, manifest.PhasePostHandler)
	defer cleanup()

	stream, closeStream := openStream(t, url)
	defer closeStream()

	waitForSubscriber(t, events)
	events.Publish("plugins-changed")

	if _, ok := awaitEvent(t, stream, 3*time.Second); !ok {
		t.Fatal("no event arrived within 3s with a post_handler filter installed; " +
			"the response wrapper is hiding http.Flusher and buffering the stream")
	}
}

// A log-phase filter also causes wrapping, by a different branch.
func TestSSEDeliversBehindLogFilter(t *testing.T) {
	url, events, cleanup := sseGateway(t, manifest.PhaseLog)
	defer cleanup()

	stream, closeStream := openStream(t, url)
	defer closeStream()

	waitForSubscriber(t, events)
	events.Publish("plugins-changed")

	if _, ok := awaitEvent(t, stream, 3*time.Second); !ok {
		t.Fatal("no event arrived within 3s with a log filter installed; the stream is being buffered")
	}
}

// Both at once, which is the arrangement a real audit plugin produces.
func TestSSEDeliversBehindFullChain(t *testing.T) {
	url, events, cleanup := sseGateway(t,
		manifest.PhasePreRoute, manifest.PhasePostHandler, manifest.PhaseLog)
	defer cleanup()

	stream, closeStream := openStream(t, url)
	defer closeStream()

	waitForSubscriber(t, events)

	// Several events in a row, each of which must arrive on its own rather
	// than being batched at the end.
	for i := range 3 {
		events.Publish("plugins-changed")
		if _, ok := awaitEvent(t, stream, 3*time.Second); !ok {
			t.Fatalf("event %d of 3 did not arrive within 3s behind a full filter chain", i+1)
		}
	}
}

// Closing the browser must unregister the console, or the subscriber map grows
// with every page load until Core runs out of memory.
func TestSSEUnsubscribesOnDisconnect(t *testing.T) {
	url, events, cleanup := sseGateway(t)
	defer cleanup()

	_, closeStream := openStream(t, url)
	waitForSubscriber(t, events)
	closeStream()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if events.Subscribers() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("%d subscriber(s) still registered after the client disconnected; "+
		"every page load would leak one", events.Subscribers())
}

func waitForSubscriber(t *testing.T, events *gateway.UIEvents) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if events.Subscribers() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the stream handler never registered a subscriber")
}

// A long-lived connection must not pin a plugin.
//
// The SSE stream passes through the filter pipeline like any other request, so
// it admits itself on every plugin with a pre_route filter — and then does not
// return for as long as the console stays open. Holding that admission for the
// life of the connection means those plugins can never drain: an operator with
// the console open makes every disable and every upgrade wait out the full
// 30-second drain timeout before the process is killed anyway.
//
// Nothing after the pipeline hands off to Core's own handler calls a plugin
// again — the log phase manages its own admission — so the reservation has no
// reason to outlive the filters that took it.
func TestLongLivedConnectionDoesNotPinAPlugin(t *testing.T) {
	inst := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{inst},
		Filters: compileFilters(t, "hello", manifest.FilterDecl{
			Name:  "guard",
			Phase: manifest.PhasePreRoute,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})

	events := gateway.NewUIEvents()
	mux := http.NewServeMux()
	mux.HandleFunc(gateway.UIEventsPath, events.Handler)

	h := &gateway.PluginHandler{Registry: reg, Runner: &pipeline.Runner{}}
	srv := httptest.NewServer(h.Middleware(mux))
	defer srv.Close()

	stream, closeStream := openStream(t, srv.URL)
	defer closeStream()
	waitForSubscriber(t, events)
	_ = stream

	// The stream is open and will stay open. The plugin must not be held by it.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inst.InFlight() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Errorf("in-flight = %d while an SSE stream is open; every disable and upgrade "+
		"would wait out the drain timeout for as long as a console is connected",
		inst.InFlight())
}
