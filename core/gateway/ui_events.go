package gateway

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// UIEventsPath is the console's server-sent events stream.
const UIEventsPath = "/api/system/ui/events"

// keepaliveInterval keeps proxies from closing an idle stream.
const keepaliveInterval = 20 * time.Second

// UIEvents pushes registry changes to open consoles.
//
// This is what makes a plugin's menu appear the moment it is enabled and
// vanish the moment it is disabled. Without it the console only learns about
// changes when someone reloads the page, which is the behaviour the extension
// model had and the reason a disabled extension's menu lingered.
//
// Events are deliberately coarse: one "changed" signal that tells the browser
// to refetch, rather than a diff. Refetching costs a small request and cannot
// get out of sync; a diff stream has to get ordering, deduplication and
// reconnection exactly right to avoid a console that quietly shows the wrong
// thing.
type UIEvents struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func NewUIEvents() *UIEvents {
	return &UIEvents{subs: map[chan string]struct{}{}}
}

// Publish notifies every open console. It never blocks: a console that cannot
// keep up misses this signal and catches up on the next one or on reconnect,
// which is harmless because every signal means the same thing.
func (b *UIEvents) Publish(event string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

// Subscribers reports how many consoles are listening, for diagnostics.
func (b *UIEvents) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Handler serves the SSE stream.
func (b *UIEvents) Handler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Without this an intermediary that buffers responses would hold events
	// until the stream closed, defeating the point.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ch := make(chan string, 8)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}()

	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	for {
		select {
		case ev := <-ch:
			fmt.Fprintf(w, "event: %s\ndata: {}\n\n", ev)
			flusher.Flush()
		case <-ticker.C:
			// A comment line is a valid SSE keepalive the browser ignores.
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
