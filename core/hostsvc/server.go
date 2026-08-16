// Package hostsvc implements the capabilities Core exposes back to plugins:
// data, queue, cache, locks, config, files, outbound HTTP, events and
// observability.
//
// A Server instance serves exactly one plugin instance. The plugin key and the
// permissions an admin granted are closed over at construction, and no request
// message carries a key. That is what makes a plugin's identity structural
// rather than self-asserted: there is no field for a plugin to forge, and no
// way to address another plugin's data.
package hostsvc

import (
	"context"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/taills/moduless/manifest"
	"github.com/taills/moduless/pluginapi"
	pb "github.com/taills/moduless/proto/plugin"
)

// TraceMetadataKey is the gRPC metadata key carrying the trace id on calls a
// plugin makes back into Core. The SDK sets it automatically from the context
// of whatever request or job is running, so a slow query can be attributed to
// the request that caused it. Defined in pluginapi so both sides share one
// definition.
const TraceMetadataKey = pluginapi.TraceMetadataKey

// permSet is the granted permission set, resolved once at construction.
type permSet map[string]struct{}

func newPermSet(perms []string) permSet {
	s := make(permSet, len(perms))
	for _, p := range perms {
		s[p] = struct{}{}
	}
	return s
}

func (p permSet) has(perm string) bool {
	_, ok := p[perm]
	return ok
}

// Deps are the Core subsystems a Server delegates to. Any left nil makes the
// corresponding capability report Unavailable rather than panicking, which is
// how Core runs without a database or object store configured.
type Deps struct {
	Data   DataBackend
	Queue  QueueBackend
	Cache  CacheBackend
	Locks  LockBackend
	Config ConfigBackend
	Files  FileBackend
	Events EventBackend
	Egress EgressBackend
	Obs    ObservabilityBackend
}

// Server implements pb.HostServicesServer for one plugin instance.
type Server struct {
	pb.UnimplementedHostServicesServer

	key   string
	perms permSet
	deps  Deps

	// inflight counts messages delivered to this consumer and not yet
	// acknowledged, and bounds it to the prefetch it asked for.
	//
	// Without a bound, prefetch limits only the size of one claim batch and not
	// the work in hand: the consume loop claims, streams and loops again
	// without waiting, so one consumer drains the whole backlog in
	// milliseconds. Measured — one replica took every message while its
	// sibling sat idle, and the visibility clock was running on messages that
	// had not been started.
	//
	// Counted on delivery rather than reserved before the claim. Reserving
	// first leaks: a claim that finds nothing never delivers and never gets
	// acknowledged, so the reservation is never returned and the consumer
	// blocks for good on its next poll. That is not hypothetical — it is what
	// the first version of this did, and it looked exactly like the bug it was
	// meant to fix.
	inflightMu sync.Mutex
	inflight   int
}

// New builds the host-side service for a plugin instance.
func New(pluginKey string, granted []string, deps Deps) *Server {
	return &Server{
		key:   pluginKey,
		perms: newPermSet(granted),
		deps:  deps,
	}
}

// PluginKey reports which plugin this server belongs to.
func (s *Server) PluginKey() string { return s.key }

// require rejects a call the plugin was not granted.
//
// The check runs here, inside Core, so a plugin cannot reach past it. What it
// buys is not protection from hostile code — plugins are reviewed and run with
// Core's own privileges — but that a plugin doing something its manifest never
// declared fails immediately and visibly instead of quietly working.
//
// PermissionDenied rather than Unimplemented is deliberate: it tells the
// plugin author their manifest is missing a declaration, instead of suggesting
// Core does not support the feature.
func (s *Server) require(perm string) error {
	if s.perms.has(perm) {
		return nil
	}
	return status.Errorf(codes.PermissionDenied,
		"plugin %q is not granted the %q permission; declare it under permissions: in manifest.yaml and have an admin approve it",
		s.key, perm)
}

// unavailable reports a capability Core is not running.
func (s *Server) unavailable(what string) error {
	return status.Errorf(codes.Unavailable, "%s is not configured on this Core instance", what)
}

// traceFrom pulls the trace id a plugin attached to the call, falling back to
// the one carried in the request message. Plugins are not trusted to supply a
// correct trace id — it is only used for correlation in logs, never for
// authorization — so an absent or odd value costs nothing.
func traceFrom(ctx context.Context, fallback string) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get(TraceMetadataKey); len(vals) > 0 && vals[0] != "" {
			return vals[0]
		}
	}
	return fallback
}

// Permission constants re-exported so callers do not need the manifest package
// just to name a permission.
const (
	PermDB         = manifest.PermDB
	PermDBTx       = manifest.PermDBTx
	PermQueue      = manifest.PermQueue
	PermCache      = manifest.PermCache
	PermLock       = manifest.PermLock
	PermEvents     = manifest.PermEvents
	PermFilesRead  = manifest.PermFilesRead
	PermFilesWrite = manifest.PermFilesWrite
	PermHTTPEgress = manifest.PermHTTPEgress
)
