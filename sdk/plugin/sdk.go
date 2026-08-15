// Package sdk is what a plugin author writes against.
//
// A plugin is an ordinary Go program whose main() ends in sdk.Serve. It serves
// HTTP with a standard http.Handler — net/http, chi, gin, whatever — and Core
// routes requests to it over a private connection. The plugin opens no ports
// and needs no address; Core starts it as a child process.
//
// The smallest useful plugin:
//
//	func main() {
//		mux := http.NewServeMux()
//		mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
//			fmt.Fprintf(w, "hello %s", sdk.User(r.Context()).Username)
//		})
//		sdk.Serve(sdk.Config{Handler: mux})
//	}
//
// One rule matters more than any other: never write to stdout. Core reads the
// handshake from the first line of the plugin's stdout, so a stray fmt.Println
// prevents the plugin from starting at all. Use sdk.Log, or the standard log
// package, both of which go to stderr.
package sdk

import (
	"context"
	"net/http"

	"google.golang.org/grpc"

	"github.com/taills/moduless/pluginapi"
	pb "github.com/taills/moduless/proto/plugin"
)

// Phase identifies a point in the request lifecycle a filter can intercept.
// The names mirror the manifest, and a filter only ever receives phases the
// manifest subscribed it to.
type Phase int

const (
	PhasePreRoute Phase = iota
	PhaseAuthenticate
	PhaseAuthorize
	PhasePreHandler
	PhasePostHandler
	PhaseOnError
	PhaseLog
)

// FilterFunc inspects a request at one lifecycle phase.
//
// Return Continue to let the request proceed, Stop to answer it immediately,
// or Mutate to change it and continue. Returning an error counts against the
// plugin's circuit breaker, so prefer returning Continue when a filter simply
// has nothing to say.
type FilterFunc func(ctx context.Context, req *FilterRequest) (*FilterResult, error)

// JobFunc runs one scheduled job occurrence.
type JobFunc func(ctx context.Context, job *Job) error

// Config describes a plugin.
type Config struct {
	// Handler serves requests routed to /api/plugins/<key>/*. The key prefix
	// is stripped before the handler sees the path.
	Handler http.Handler

	// Filters maps lifecycle phases to handlers. A phase declared here but not
	// in manifest.yaml is never called; a phase in the manifest with no
	// handler here continues the request unchanged.
	Filters map[Phase]FilterFunc

	// Jobs maps the job names declared in manifest.yaml to their handlers.
	Jobs map[string]JobFunc

	// OnConfigChanged receives admin config edits. It is called on a
	// background goroutine, so it must not assume request scope.
	OnConfigChanged func(config map[string]string)

	// OnShutdown runs when Core asks the plugin to drain. In-flight requests
	// are already finishing; use this to close what the plugin itself opened.
	// Core kills the process once the drain deadline elapses regardless.
	OnShutdown func(ctx context.Context) error

	// MaxMessageBytes overrides the transport ceiling. Core enforces its own
	// limit as well, and the lower of the two applies.
	MaxMessageBytes int
}

// Serve runs the plugin. It blocks until Core stops the process, and must be
// the last thing main() does.
func Serve(cfg Config) {
	impl := newPlugin(cfg)
	// Package-level helpers such as Key and DataDir read through this.
	current.set(impl)
	pluginapi.Serve(pluginapi.ServeConfig{
		Impl:            impl,
		HostBinder:      impl.bindHost,
		MaxMessageBytes: cfg.MaxMessageBytes,
	})
}

// bindHost wires the host clients once Core hands over the reverse connection.
func (p *plugin) bindHost(conn *grpc.ClientConn) {
	client := pb.NewHostServicesClient(conn)

	DB = &DBClient{c: client}
	Queue = &QueueClient{c: client}
	Cache = &CacheClient{c: client}
	Locks = &LockClient{c: client}
	Files = &FilesClient{c: client}
	Events = &EventClient{c: client}
	HTTP = &HTTPClient{c: client}
	Log = &Logger{c: client}
}

// The host capabilities. They are usable from the moment Configure runs, which
// is before any request arrives, so a plugin may use them during start-up.
//
// Each call is permission-checked by Core against the manifest, so a capability
// the plugin did not declare returns PermissionDenied naming what is missing.
var (
	DB     *DBClient
	Queue  *QueueClient
	Cache  *CacheClient
	Locks  *LockClient
	Files  *FilesClient
	Events *EventClient
	HTTP   *HTTPClient
	Log    *Logger
)
