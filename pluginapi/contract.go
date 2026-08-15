// Package pluginapi holds the go-plugin glue shared by Core and by every
// plugin binary. It deliberately depends on nothing under core/ so a
// third-party plugin author can import it (via sdk/go) without pulling in the
// gateway, the database layer, or anything else Core-private.
package pluginapi

import (
	"context"

	"github.com/hashicorp/go-plugin"
	pb "github.com/taills/moduless/proto/plugin"
)

const (
	// DispenseName is the key both sides use in the plugin map. A plugin
	// binary serves exactly one implementation, so there is only ever one.
	DispenseName = "moduless"

	// TraceMetadataKey carries the trace id on calls a plugin makes back into
	// Core. It lives here rather than in either side's own package because
	// both must agree on it exactly, and a drift would silently break
	// correlation rather than fail loudly.
	TraceMetadataKey = "x-moduless-trace-id"

	// DefaultMaxMessageBytes raises gRPC's 4 MiB default. go-plugin does not
	// override it, and the default surfaces as an opaque ResourceExhausted
	// rather than a usable error, so both sides set this explicitly.
	// Requests larger than this must go through the file service instead of
	// being pushed through HandleHTTP.
	DefaultMaxMessageBytes = 16 << 20
)

// Handshake gates which binaries Core is willing to talk to. Per go-plugin's
// own documentation this is a UX feature, not a security boundary: the real
// guarantees come from SecureConfig (the executed bytes match the verified
// bytes) and from Core being the process's direct parent.
var Handshake = plugin.HandshakeConfig{
	ProtocolVersion:  1,
	MagicCookieKey:   "MODULESS_PLUGIN",
	MagicCookieValue: "moduless-plugin-v1",
}

// PluginImpl is what a plugin author implements, normally indirectly through
// sdk/go rather than by hand.
//
// Every method must be safe for concurrent use: Core dispatches HTTP requests,
// filters and jobs concurrently over a single multiplexed connection.
type PluginImpl interface {
	// Configure is always called first, exactly once, before any other method.
	Configure(ctx context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error)

	// HandleHTTP serves a request routed to this plugin's own API prefix.
	HandleHTTP(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error)

	// Filter runs one subscribed lifecycle phase. Implementations should
	// return ACTION_CONTINUE for phases they do not care about rather than
	// erroring, since an error counts against the circuit breaker.
	Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error)

	// RunJob executes one scheduled job occurrence.
	RunJob(ctx context.Context, req *pb.JobRequest) (*pb.JobResponse, error)

	// OnConfigChanged receives admin config edits without a restart.
	OnConfigChanged(ctx context.Context, req *pb.ConfigChangeEvent) error

	// Shutdown asks the plugin to drain. Core kills the process once the
	// drain deadline elapses regardless of what this returns.
	Shutdown(ctx context.Context, req *pb.ShutdownRequest) error
}
