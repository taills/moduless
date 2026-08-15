package pluginhost

import (
	"context"
	"sync/atomic"
	"time"

	pb "github.com/taills/moduless/proto/plugin"
)

// fakeClient stands in for a real plugin connection so lifecycle behaviour can
// be tested without forking a process. Everything is atomic because the
// registry drives instances from several goroutines.
type fakeClient struct {
	// filterDelay makes a call hang, for drain and timeout tests.
	filterDelay time.Duration
	filterErr   error

	// blockUntil, when non-nil, holds HandleHTTP open until it is closed.
	blockUntil chan struct{}

	shutdownCalls atomic.Int32
	filterCalls   atomic.Int32
	httpCalls     atomic.Int32
}

func (f *fakeClient) HandleHTTP(ctx context.Context, _ *pb.HttpRequest) (*pb.HttpResponse, error) {
	f.httpCalls.Add(1)
	if f.blockUntil != nil {
		select {
		case <-f.blockUntil:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &pb.HttpResponse{StatusCode: 200}, nil
}

func (f *fakeClient) Filter(ctx context.Context, _ *pb.FilterRequest) (*pb.FilterResponse, error) {
	f.filterCalls.Add(1)
	if f.filterDelay > 0 {
		select {
		case <-time.After(f.filterDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.filterErr != nil {
		return nil, f.filterErr
	}
	return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}, nil
}

func (f *fakeClient) RunJob(context.Context, *pb.JobRequest) (*pb.JobResponse, error) {
	return &pb.JobResponse{Success: true}, nil
}

func (f *fakeClient) OnConfigChanged(context.Context, *pb.ConfigChangeEvent) error { return nil }

func (f *fakeClient) Shutdown(context.Context, *pb.ShutdownRequest) error {
	f.shutdownCalls.Add(1)
	return nil
}

// fakeProc stands in for the go-plugin client's process control.
type fakeProc struct {
	kills  atomic.Int32
	exited atomic.Bool
}

func (p *fakeProc) Kill() {
	p.kills.Add(1)
	p.exited.Store(true)
}

func (p *fakeProc) Exited() bool { return p.exited.Load() }

// readyInstance builds an instance already in rotation.
func readyInstance(key string, weight int) (*Instance, *fakeClient, *fakeProc) {
	fc := &fakeClient{}
	fp := &fakeProc{}
	inst := NewInstance(key, key+"-1", "1.0.0", weight, fc, fp)
	inst.MarkReady()
	return inst, fc, fp
}
