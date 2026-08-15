// Package pipeline runs the IIS-style request lifecycle: it dispatches each
// phase to the plugin filters that subscribed to it, and applies whatever they
// decide.
//
// The design goal is that a request nobody subscribed to costs essentially
// nothing. Phase lookup is an array index, and path matching allocates nothing,
// so an unfiltered request pays a few nanoseconds rather than the ~37
// microseconds a cross-process call would cost.
package pipeline

import (
	"net/http"

	pb "github.com/taills/moduless/proto/plugin"
)

// ToProtoHeaders converts an http.Header into the wire representation,
// preserving repeated values. The reverse tunnel used map[string]string here
// and silently dropped every value but the first, which corrupted Set-Cookie
// and any other legitimately repeated header.
func ToProtoHeaders(h http.Header) map[string]*pb.HeaderValues {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]*pb.HeaderValues, len(h))
	for k, vs := range h {
		out[k] = &pb.HeaderValues{Values: vs}
	}
	return out
}

// FromProtoHeaders converts wire headers into an http.Header.
func FromProtoHeaders(in map[string]*pb.HeaderValues) http.Header {
	if len(in) == 0 {
		return nil
	}
	out := make(http.Header, len(in))
	for k, hv := range in {
		out[http.CanonicalHeaderKey(k)] = append([]string(nil), hv.GetValues()...)
	}
	return out
}

// applyHeaderMutation applies a filter's header edits to h.
//
// Removals run before additions so a filter can replace a header in one step
// by listing it in both, which is the common case for rewriting an
// Authorization or a correlation header.
func applyHeaderMutation(h http.Header, set map[string]*pb.HeaderValues, remove []string) {
	for _, k := range remove {
		h.Del(k)
	}
	for k, hv := range set {
		canonical := http.CanonicalHeaderKey(k)
		h[canonical] = append([]string(nil), hv.GetValues()...)
	}
}
