package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/taills/moduless/proto/plugin"
)

// What a plugin author can test without a live Core.
//
// This matters more than most SDK tests: an external team writing a plugin has
// only what this package exports, and an agent given nothing but the plugin
// guide reported that there was no testing story at all. Two things were
// actually impossible rather than merely undocumented — asserting what a filter
// decided, and giving a handler an authenticated caller — and both are the
// exact things the guide tells authors to get right.
//
// These tests are written the way a plugin author would write theirs, using
// only exported API, so they fail if that surface stops being enough.

// A filter is an ordinary function. Calling it is easy; the problem was
// checking the answer.
func TestAuthorCanAssertWhatAFilterDecided(t *testing.T) {
	guard := func(_ context.Context, req *FilterRequest) (*FilterResult, error) {
		if req.Header.Get("X-Api-Key") == "" {
			return Stop(http.StatusUnauthorized, []byte(`{"error":"no key"}`)).
				WithHeader("WWW-Authenticate", "Bearer"), nil
		}
		return Mutate().SetIdentity(&UserContext{
			UserID: "42", Username: "svc", Roles: []string{"reader"},
		}).SetValue("via", "apikey"), nil
	}

	t.Run("refuses an anonymous request", func(t *testing.T) {
		res, err := guard(context.Background(), &FilterRequest{
			Phase:  PhaseAuthenticate,
			Method: http.MethodGet,
			Path:   "/api/plugins/notes/items",
			Header: http.Header{},
		})
		if err != nil {
			t.Fatalf("filter: %v", err)
		}

		got := res.Inspect()
		if got.Action != ActionStop {
			t.Fatalf("action = %s, want stop", got.Action)
		}
		if got.Status != http.StatusUnauthorized {
			t.Errorf("status = %d", got.Status)
		}
		if got.Header.Get("WWW-Authenticate") == "" {
			t.Error("no challenge header on a 401")
		}
	})

	t.Run("grants an identity to a request that carries a key", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Api-Key", "abc")
		res, err := guard(context.Background(), &FilterRequest{
			Phase: PhaseAuthenticate, Method: http.MethodGet, Path: "/x", Header: h,
		})
		if err != nil {
			t.Fatalf("filter: %v", err)
		}

		got := res.Inspect()
		if got.Action != ActionMutate {
			t.Fatalf("action = %s, want mutate", got.Action)
		}
		// The roles matter as much as the id: everything downstream reads them.
		if !got.Identity.HasRole("reader") {
			t.Errorf("identity = %+v; the roles did not survive", got.Identity)
		}
		if got.Values["via"] != "apikey" {
			t.Errorf("values = %v", got.Values)
		}
	})
}

// The authorization pattern the guide teaches, tested the way an author would.
func TestAuthorCanTestAHandlerThatChecksRoles(t *testing.T) {
	// Exactly what docs/plugin-development.md shows.
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !User(r.Context()).HasRole("admin") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"ok": User(r.Context()).Name()})
	})

	for _, tc := range []struct {
		name string
		user *UserContext
		want int
	}{
		// Nil rather than absent: an anonymous request is what Core passes, and
		// the accessors are nil-safe precisely so this does not panic. A panic
		// in a plugin kills the process, so this case is the one that matters.
		{"anonymous", nil, http.StatusForbidden},
		{"an ordinary user", &UserContext{UserID: "7", Username: "bob", Roles: []string{"user"}}, http.StatusForbidden},
		{"an admin", &UserContext{UserID: "1", Username: "root", Roles: []string{"admin"}}, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/keys", nil)
			req = req.WithContext(WithUser(req.Context(), tc.user))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// A filter that returns Continue says so, rather than reading as an
// unrecognised answer. Worth its own case: Continue is the zero value of the
// underlying action, and a decision type that could not tell "continue" from
// "nothing was set" would be useless for the most common outcome.
func TestContinueIsDistinctFromNothing(t *testing.T) {
	if got := Continue().Inspect().Action; got != ActionContinue {
		t.Errorf("action = %s, want continue", got)
	}
	var none *FilterResult
	if got := none.Inspect().Action; got != ActionUnrecognised {
		t.Errorf("a nil result reads as %s; a filter that returned nothing is not "+
			"the same as one that let the request through", got)
	}
}

// OnReady is where background work starts, and both halves of that matter.
//
// It exists because there is nowhere else. main() runs before Core hands over
// the reverse connection, so sdk.Queue is nil there; OnConfigChanged fires
// again on every later change, so starting a consumer in it leaves one running
// per edit. Neither failure is loud — the first is a nil dereference at
// startup, the second is silent duplicate consumption.
func TestOnReadyRunsOnceAfterConfiguration(t *testing.T) {
	var (
		mu       sync.Mutex
		calls    int
		sawValue string
	)
	p := newPlugin(Config{
		OnConfigChanged: func(cfg map[string]string) {
			mu.Lock()
			defer mu.Unlock()
			sawValue = cfg["greeting"]
		},
		OnReady: func(context.Context) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			// The settings must already be applied: work starting here reads
			// configuration, and reading it before OnConfigChanged has run
			// would silently use the compiled-in defaults.
			if sawValue == "" {
				t.Error("OnReady ran before the configuration was applied")
			}
		},
	})
	current.set(p)

	req := &pb.ConfigureRequest{
		PluginKey: "syncer",
		Config:    map[string]string{"greeting": "configured"},
	}
	if _, err := p.Configure(t.Context(), req); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	// A second Configure is what a reconfiguration looks like from here.
	if _, err := p.Configure(t.Context(), req); err != nil {
		t.Fatalf("second Configure: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give a second call a chance to arrive, or "ran once" would only mean
	// "the second one had not landed yet".
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("OnReady ran %d times; twice means a second consumer for every "+
			"configuration change, which shows up as duplicated work rather than "+
			"as an error", calls)
	}
}

// The context OnReady is given ends when Core asks the plugin to drain, so a
// blocking Consume returns instead of being killed with the process.
func TestOnReadyContextIsCancelledOnShutdown(t *testing.T) {
	stopped := make(chan struct{})
	p := newPlugin(Config{
		OnReady: func(ctx context.Context) {
			<-ctx.Done()
			close(stopped)
		},
	})
	current.set(p)

	if _, err := p.Configure(t.Context(), &pb.ConfigureRequest{PluginKey: "syncer"}); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	select {
	case <-stopped:
		t.Fatal("the context was already cancelled before Shutdown was called")
	case <-time.After(50 * time.Millisecond):
	}

	if err := p.Shutdown(t.Context(), &pb.ShutdownRequest{}); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Error("OnReady's context was not cancelled by Shutdown; a blocking consumer " +
			"would sit there until Core killed the process, which is what the drain " +
			"deadline exists to avoid")
	}
}

// A filter that logs must still be testable.
//
// The host clients are nil until Core hands over the reverse connection, so
// under the author's own `go test` every sdk.Log call has a nil receiver. That
// used to be a segmentation fault, which landed on the most ordinary shape
// there is — log the reason, then refuse:
//
//	sdk.Log.Warn(ctx, "rate limit exceeded", "bucket", key)
//	return sdk.Stop(http.StatusTooManyRequests, body), nil
//
// Found by writing the first test for extension-example/ratelimit: seven
// shipped examples had no tests between them, so nothing had ever exercised
// this path. Whether a function can be unit-tested must not depend on whether
// it happens to log.
func TestLoggingWithoutACoreDoesNotPanic(t *testing.T) {
	var unbound *Logger // exactly what sdk.Log is before Serve runs
	ctx := context.Background()

	// Each of these would have been a nil dereference.
	unbound.Debug(ctx, "debug", "k", 1)
	unbound.Info(ctx, "info", "k", "v")
	unbound.Warn(ctx, "warn", "err", errNotBoundExample)
	unbound.Error(ctx, "error")
	unbound.Metric(ctx, "things_done", 1, nil)
	unbound.Gauge(ctx, "depth", 3, nil)
	unbound.Histogram(ctx, "latency_ms", 12.5, nil)
}

var errNotBoundExample = errors.New("boom")

// The fallback goes to stderr, not stdout: Core reads the startup handshake
// from the first line of stdout, so a habit that works in tests and corrupts
// start-up would be worse than printing nothing at all.
func TestUnboundLogsGoToStderrWithTheirFields(t *testing.T) {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()

	var unbound *Logger
	unbound.Warn(context.Background(), "rate limit exceeded", "bucket", "ip:203.0.113.7", "path", "/api/x")

	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}

	got := string(out)
	for _, want := range []string{"[warn]", "rate limit exceeded", "bucket=ip:203.0.113.7", "path=/api/x"} {
		if !strings.Contains(got, want) {
			t.Errorf("the record dropped %q on the way to stderr: %s", want, got)
		}
	}
	// Fields are sorted, so a test asserting on this output is not at the mercy
	// of map iteration order.
	if i, j := strings.Index(got, "bucket="), strings.Index(got, "path="); i > j {
		t.Errorf("fields are not in a stable order: %s", got)
	}
}
