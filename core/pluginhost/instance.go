package pluginhost

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	pb "github.com/taills/moduless/proto/plugin"
)

// State is an instance's lifecycle position.
//
// Legal transitions:
//
//	Starting -> Ready       launch and Configure succeeded
//	Starting -> Failed      launch or Configure failed
//	Ready    -> Draining    an admin disabled or upgraded the plugin
//	Ready    -> Failed      the process died on its own
//	Draining -> Stopped     in-flight work finished (or the deadline passed)
//	Failed   -> Quarantined too many crashes in the window
//
// Stopped, Failed and Quarantined are terminal for the instance that reaches
// them. A retry or a re-enable produces a *new* Instance and replaces this one
// in the registry — nothing ever moves an instance back to Starting. The table
// used to claim otherwise, which sends anyone debugging "why is this failed
// instance not starting again" looking for a transition that does not exist.
type State int32

const (
	StateStarting State = iota
	StateReady
	StateDraining
	StateStopped
	StateFailed
	StateQuarantined
)

func (s State) String() string {
	switch s {
	case StateStarting:
		return "starting"
	case StateReady:
		return "ready"
	case StateDraining:
		return "draining"
	case StateStopped:
		return "stopped"
	case StateFailed:
		return "failed"
	case StateQuarantined:
		return "quarantined"
	default:
		return "unknown"
	}
}

// PluginClient is the narrow surface the registry and gateway need from a
// running plugin. *pluginapi.Client satisfies it; tests substitute a fake so
// draining, breaker and restart logic can be exercised without forking a real
// process.
type PluginClient interface {
	HandleHTTP(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error)
	Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error)
	RunJob(ctx context.Context, req *pb.JobRequest) (*pb.JobResponse, error)
	OnConfigChanged(ctx context.Context, req *pb.ConfigChangeEvent) error
	Shutdown(ctx context.Context, req *pb.ShutdownRequest) error
}

// processHandle is the part of go-plugin's client the lifecycle code touches.
// Note Exited is a poll, not a notification: go-plugin exposes no channel that
// closes on child exit, which is why the supervisor also watches the health
// stream.
type processHandle interface {
	Kill()
	Exited() bool
}

// Instance is one running plugin process.
//
// Instances are shared across goroutines by the atomic snapshot, so every
// mutable field here is atomic. The struct itself is never copied.
type Instance struct {
	Key        string
	InstanceID string
	Version    string

	// Weight drives smooth weighted round-robin when a plugin runs several
	// replicas. Zero is normalised to 1.
	Weight int

	Client  PluginClient
	Breaker *Breaker

	proc      processHandle
	state     atomic.Int32
	inflight  atomic.Int64
	startedAt time.Time
}

// NewInstance builds a ready-to-serve instance. Launch calls this; tests
// construct one directly with a fake client.
func NewInstance(key, instanceID, version string, weight int, client PluginClient, proc processHandle) *Instance {
	if weight <= 0 {
		weight = 1
	}
	i := &Instance{
		Key:        key,
		InstanceID: instanceID,
		Version:    version,
		Weight:     weight,
		Client:     client,
		Breaker:    NewBreaker(DefaultBreakerConfig()),
		proc:       proc,
		startedAt:  time.Now(),
	}
	i.state.Store(int32(StateStarting))
	return i
}

func (i *Instance) State() State     { return State(i.state.Load()) }
func (i *Instance) setState(s State) { i.state.Store(int32(s)) }

// MarkReady moves a freshly launched instance into rotation.
func (i *Instance) MarkReady() { i.setState(StateReady) }

// MarkFailed records that the process died unexpectedly.
func (i *Instance) MarkFailed() { i.setState(StateFailed) }

// MarkQuarantined takes the instance out of service until an admin intervenes.
func (i *Instance) MarkQuarantined() { i.setState(StateQuarantined) }

// Servable reports whether a request holding this snapshot may still use the
// instance.
//
// Draining counts. Keeping new traffic away is the snapshot's job — a swap
// removes the old instance from it, so nothing arriving afterwards can see it
// at all — and the only requests still reaching a draining instance are ones
// that were admitted before the swap and are partway through. Refusing those
// here would return 502 for work the system had already accepted, which is the
// opposite of draining.
func (i *Instance) Servable() bool {
	switch i.State() {
	case StateReady, StateDraining:
		return true
	default:
		return false
	}
}

// InFlight reports how many requests are currently executing against this
// instance. Draining waits for it to reach zero.
func (i *Instance) InFlight() int64 { return i.inflight.Load() }

// StartedAt is when the process was launched.
func (i *Instance) StartedAt() time.Time { return i.startedAt }

// BeginRequest reserves capacity for one request. It returns false when the
// instance is not accepting work, which is how a draining instance stops
// taking new traffic the moment it leaves the snapshot.
//
// The returned function must be called exactly once, and callers should defer
// it immediately:
//
//	end, ok := inst.BeginRequest()
//	if !ok { ... }
//	defer end()
func (i *Instance) BeginRequest() (end func(), ok bool) {
	if i.State() != StateReady {
		return nil, false
	}
	i.inflight.Add(1)
	// Re-check after incrementing. A drain that flipped the state between the
	// check and the increment would otherwise miss this request in its count
	// and could kill the process mid-flight.
	if i.State() != StateReady {
		i.inflight.Add(-1)
		return nil, false
	}
	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			i.inflight.Add(-1)
		}
	}, true
}

// Drain stops the instance from accepting new work, asks it to finish what it
// has, and waits for in-flight requests to complete.
//
// It always terminates the process, even on timeout: an instance that has
// already been swapped out of the snapshot is unreachable, so holding it open
// for a stuck request would leak a process indefinitely. The returned error
// reports whether the drain completed cleanly.
func (i *Instance) Drain(ctx context.Context, timeout time.Duration) error {
	i.setState(StateDraining)

	// Best effort: give the plugin a chance to close resources. A plugin that
	// ignores or fails this still gets killed below.
	sctx, cancel := context.WithTimeout(ctx, timeout)
	_ = i.Client.Shutdown(sctx, &pb.ShutdownRequest{
		Reason:              "drain",
		DrainTimeoutSeconds: int32(timeout / time.Second),
	})
	cancel()

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for i.inflight.Load() > 0 {
		select {
		case <-ctx.Done():
			remaining := i.inflight.Load()
			i.terminate()
			return fmt.Errorf("plugin %s: drain cancelled with %d request(s) in flight: %w",
				i.Key, remaining, ctx.Err())
		case <-ticker.C:
			if time.Now().After(deadline) {
				remaining := i.inflight.Load()
				i.terminate()
				return fmt.Errorf("plugin %s: drain timed out after %s with %d request(s) in flight",
					i.Key, timeout, remaining)
			}
		}
	}

	i.terminate()
	return nil
}

func (i *Instance) terminate() {
	if i.proc != nil {
		i.proc.Kill()
	}
	// Quarantine outlives the process it describes. It records *why* a plugin
	// is not running — it crash-looped past the threshold and needs a human —
	// and clearing it is an admin action. Letting the kill that enforces the
	// quarantine overwrite it with Stopped would make a plugin Core gave up on
	// indistinguishable from one an operator switched off, in the console and
	// in every log after it.
	for {
		cur := i.state.Load()
		if State(cur) == StateQuarantined {
			return
		}
		if i.state.CompareAndSwap(cur, int32(StateStopped)) {
			return
		}
	}
}

// Kill terminates the process immediately, without draining. Used on the
// rollback path when a freshly launched instance never entered rotation.
func (i *Instance) Kill() { i.terminate() }

// ProcessExited reports whether the underlying process is gone.
func (i *Instance) ProcessExited() bool {
	return i.proc == nil || i.proc.Exited()
}

// --- pipeline.Target -------------------------------------------------------
//
// These adapt an instance to what the filter pipeline needs, so the pipeline
// can depend on a narrow interface rather than on this package.

// Filter invokes one lifecycle phase on the plugin.
func (i *Instance) Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error) {
	return i.Client.Filter(ctx, req)
}

// Allow reports whether the circuit breaker permits a call.
func (i *Instance) Allow() bool { return i.Breaker.Allow() }

// RecordSuccess and RecordFailure feed the breaker.
func (i *Instance) RecordSuccess() { i.Breaker.RecordSuccess() }
func (i *Instance) RecordFailure() { i.Breaker.RecordFailure() }

// Begin reserves capacity for one call; it is BeginRequest under the name the
// pipeline expects.
func (i *Instance) Begin() (func(), bool) { return i.BeginRequest() }
