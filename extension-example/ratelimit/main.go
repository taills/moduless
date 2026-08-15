// Command ratelimit is a token-bucket rate limiter that runs as a Moduless
// plugin.
//
// It is the second example, and it deliberately looks nothing like the first.
// The notes plugin owns an API, a table and a menu; this one owns none of
// those. It subscribes to a single lifecycle phase and answers one question
// about every request in the system: may this proceed?
//
// That is the IIS filter model. The plugin is not a destination, it is a stage
// in someone else's request.
//
// Build:
//
//	CGO_ENABLED=0 go build -o bin/ratelimit .
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/taills/moduless/sdk/plugin"
)

func main() {
	lim := newLimiter()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /stats", lim.handleStats)

	sdk.Serve(sdk.Config{
		Handler: mux,
		Filters: map[sdk.Phase]sdk.FilterFunc{
			sdk.PhasePreRoute: lim.check,
		},
		// Fires once at start-up with the admin's settings and again on every
		// change, so there is one path that applies configuration rather than
		// a start-up path and an update path that can drift apart.
		//
		// Limits get changed during an incident, which is exactly when
		// restarting the limiter is least welcome.
		OnConfigChanged: lim.configure,
	})
}

// --- the filter ---------------------------------------------------------

// check is called for every request in the system, so its cost is the cost of
// having this plugin installed at all. It takes one mutex and does arithmetic.
func (l *limiter) check(ctx context.Context, req *sdk.FilterRequest) (*sdk.FilterResult, error) {
	if l.exempt(req.Path) {
		return sdk.Continue(), nil
	}

	key := l.bucketKey(req)
	allowed, retryAfter := l.take(key)
	if allowed {
		return sdk.Continue(), nil
	}

	l.rejected.Add(1)
	// The trace id ties this rejection to the same request in Core's access
	// log and in whatever plugin would have served it.
	sdk.Log.Warn(ctx, "rate limit exceeded",
		"bucket", key, "path", req.Path, "trace", req.TraceID)

	return sdk.Stop(http.StatusTooManyRequests,
		[]byte(`{"error":"rate limit exceeded"}`)).
		WithHeader("Content-Type", "application/json").
		WithHeader("Retry-After", strconv.Itoa(int(retryAfter.Seconds()+0.5))), nil
}

// bucketKey decides who shares a budget.
//
// Authenticated callers are limited per user, so one person hammering the API
// from several tabs is one budget. Everyone else is limited per source
// address, which is the best identity available before authentication has run.
func (l *limiter) bucketKey(req *sdk.FilterRequest) string {
	if u := req.Identity; u != nil && u.UserID != "" {
		return "user:" + u.UserID
	}
	return "ip:" + req.ClientIP
}

func (l *limiter) exempt(path string) bool {
	// Never limit the status endpoint: a site under attack must not lose the
	// page that explains why.
	if strings.HasPrefix(path, "/api/plugins/ratelimit/") {
		return true
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, p := range l.exemptPaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// --- the bucket ---------------------------------------------------------

// limiter holds one token bucket per caller.
//
// Buckets refill continuously rather than on a timer: each take computes how
// much time has passed and credits that fraction of the rate. There is no
// background goroutine, and an idle bucket costs nothing until it is swept.
type limiter struct {
	mu          sync.RWMutex
	rate        float64 // tokens per second
	burst       float64
	exemptPaths []string
	buckets     map[string]*bucket

	// Counters are atomic rather than mutex-guarded: rejected is incremented
	// from the filter, which has already released the lock by then.
	allowed  atomic.Int64
	rejected atomic.Int64

	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter {
	return &limiter{
		rate:      defaultRate,
		burst:     defaultBurst,
		buckets:   make(map[string]*bucket),
		lastSweep: time.Now(),
	}
}

const (
	defaultRate  = 100.0 / 60.0 // 100 requests per minute
	defaultBurst = 20.0
	sweepEvery   = time.Minute
)

func (l *limiter) take(key string) (allowed bool, retryAfter time.Duration) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	// Credit the tokens earned since the last request from this caller.
	b.tokens += now.Sub(b.last).Seconds() * l.rate
	b.tokens = min(b.tokens, l.burst)
	b.last = now

	if b.tokens < 1 {
		// Time until one whole token exists.
		return false, time.Duration((1-b.tokens)/l.rate*float64(time.Second)) + time.Second
	}
	b.tokens--
	l.allowed.Add(1)
	return true, 0
}

// sweepLocked drops buckets that have refilled completely, which means their
// owner has been quiet for long enough that forgetting them changes nothing.
// Without this, the map grows with every distinct source address ever seen —
// which under a spoofed-source flood is a memory leak with a hostile author.
func (l *limiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < sweepEvery {
		return
	}
	l.lastSweep = now

	idle := time.Duration(l.burst/l.rate*float64(time.Second)) + sweepEvery
	for k, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, k)
		}
	}
}

// --- configuration ------------------------------------------------------

// configure applies admin settings, falling back to the defaults for anything
// missing or unparseable. A bad value must not disable limiting: that would
// turn a typo in the console into an open door.
func (l *limiter) configure(cfg map[string]string) {
	rate := defaultRate
	if v, err := strconv.ParseFloat(cfg["requests_per_minute"], 64); err == nil && v > 0 {
		rate = v / 60
	}
	burst := defaultBurst
	if v, err := strconv.ParseFloat(cfg["burst"], 64); err == nil && v >= 1 {
		burst = v
	}

	var exempt []string
	for p := range strings.SplitSeq(cfg["exempt_paths"], ",") {
		if p = strings.TrimSpace(p); p != "" {
			exempt = append(exempt, p)
		}
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.rate, l.burst, l.exemptPaths = rate, burst, exempt
	// Existing buckets keep their tokens. Re-filling them on a config change
	// would let anyone reset their own budget by asking an admin to save the
	// settings page.
}

// --- status -------------------------------------------------------------

func (l *limiter) handleStats(w http.ResponseWriter, r *http.Request) {
	l.mu.RLock()
	stats := map[string]any{
		"requests_per_minute": l.rate * 60,
		"burst":               l.burst,
		"tracked_callers":     len(l.buckets),
		"exempt_paths":        l.exemptPaths,
		"allowed":             l.allowed.Load(),
		"rejected":            l.rejected.Load(),
	}
	l.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		sdk.Log.Error(r.Context(), "writing stats", "err", err)
	}
}
