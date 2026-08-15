package pluginhost

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// jobRecorder is a plugin client that records the jobs it was asked to run.
type jobRecorder struct {
	fakeClient

	mu   sync.Mutex
	runs []string
	fail bool
}

func (j *jobRecorder) RunJob(_ context.Context, req *pb.JobRequest) (*pb.JobResponse, error) {
	j.mu.Lock()
	j.runs = append(j.runs, req.GetJobName())
	j.mu.Unlock()
	if j.fail {
		return &pb.JobResponse{Success: false, Error: "job failed"}, nil
	}
	return &pb.JobResponse{Success: true}, nil
}

func (j *jobRecorder) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.runs)
}

// waitForRuns waits until the recorder has seen n runs. Jobs execute on their
// own goroutine, so a count read straight after Tick would race with the job
// rather than measure it.
func waitForRuns(t *testing.T, r *jobRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if r.count() >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the job ran %d time(s) within 5s, want %d", r.count(), n)
}

// jobPackages is a JobSource backed by a map.
type jobPackages map[string]*Package

func (p jobPackages) Package(key string) (*Package, bool) {
	pkg, ok := p[key]
	return pkg, ok
}

func schedulerFixture(t *testing.T, cron string) (*Scheduler, *jobRecorder, *Registry) {
	t.Helper()

	client := &jobRecorder{}
	inst := NewInstance("jobs", "jobs-0", "1.0.0", 1, client, &fakeProc{})
	inst.MarkReady()

	reg := NewRegistry()
	reg.InstallPlugin(Registration{Key: "jobs", Instances: []*Instance{inst}})

	pkgs := jobPackages{"jobs": {Manifest: &manifest.Manifest{
		Key:  "jobs",
		Jobs: []manifest.JobDecl{{Name: "nightly", Cron: cron}},
	}}}

	return NewScheduler(reg, pkgs), client, reg
}

// The basic contract: a job whose schedule matches the current minute runs.
func TestSchedulerRunsAMatchingJob(t *testing.T) {
	s, client, _ := schedulerFixture(t, "17 3 * * *")
	s.SetClock(func() time.Time { return time.Date(2026, 8, 12, 3, 17, 30, 0, time.UTC) })

	s.Tick(context.Background())
	s.Stop()

	if got := client.count(); got != 1 {
		t.Errorf("the job ran %d time(s), want 1", got)
	}
}

// And one whose schedule does not match does not.
func TestSchedulerSkipsANonMatchingJob(t *testing.T) {
	s, client, _ := schedulerFixture(t, "17 3 * * *")
	s.SetClock(func() time.Time { return time.Date(2026, 8, 12, 3, 18, 0, 0, time.UTC) })

	s.Tick(context.Background())
	s.Stop()

	if got := client.count(); got != 0 {
		t.Errorf("the job ran %d time(s) outside its schedule", got)
	}
}

// The scheduler ticks more often than once a minute so a delayed tick still
// catches its minute. That makes running once per minute — rather than once
// per tick — the property that has to hold.
func TestSchedulerRunsOncePerMinuteNotPerTick(t *testing.T) {
	s, client, _ := schedulerFixture(t, "* * * * *")

	at := time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return at })

	for range 5 {
		s.Tick(context.Background())
	}
	s.Stop()

	if got := client.count(); got != 1 {
		t.Errorf("the job ran %d times across 5 ticks in one minute, want 1", got)
	}
}

// The next minute is a new occurrence.
func TestSchedulerRunsAgainNextMinute(t *testing.T) {
	s, client, _ := schedulerFixture(t, "* * * * *")

	at := time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return at })
	s.Tick(context.Background())

	at = at.Add(time.Minute)
	s.Tick(context.Background())
	s.Stop()

	if got := client.count(); got != 2 {
		t.Errorf("the job ran %d time(s) across two minutes, want 2", got)
	}
}

// A disabled plugin's jobs stop. Nothing unregisters them — the plugin simply
// leaves the snapshot — so this is what makes "disabling a plugin stops its
// scheduled work" true.
func TestSchedulerStopsWhenThePluginIsRemoved(t *testing.T) {
	s, client, reg := schedulerFixture(t, "* * * * *")

	at := time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return at })
	s.Tick(context.Background())
	waitForRuns(t, client, 1)

	reg.Remove("jobs")

	at = at.Add(time.Minute)
	s.Tick(context.Background())
	s.Stop()

	if got := client.count(); got != 1 {
		t.Errorf("the job ran %d time(s) after its plugin was removed", got)
	}
}

// Several replicas of a plugin share one job: the work belongs to the plugin,
// not to each process. Running it per replica would send the same nightly
// summary three times.
func TestSchedulerRunsAJobOnOneReplicaOnly(t *testing.T) {
	clients := []*jobRecorder{{}, {}, {}}
	instances := make([]*Instance, 0, len(clients))
	for i, c := range clients {
		inst := NewInstance("jobs", "jobs-"+string(rune('a'+i)), "1.0.0", 1, c, &fakeProc{})
		inst.MarkReady()
		instances = append(instances, inst)
	}

	reg := NewRegistry()
	reg.InstallPlugin(Registration{Key: "jobs", Instances: instances})

	pkgs := jobPackages{"jobs": {Manifest: &manifest.Manifest{
		Key:  "jobs",
		Jobs: []manifest.JobDecl{{Name: "nightly", Cron: "* * * * *"}},
	}}}

	s := NewScheduler(reg, pkgs)
	s.SetClock(func() time.Time { return time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC) })
	s.Tick(context.Background())
	s.Stop()

	total := 0
	for _, c := range clients {
		total += c.count()
	}
	if total != 1 {
		t.Errorf("the job ran %d time(s) across %d replicas, want exactly 1", total, len(clients))
	}
}

// A failing job must be reported. A nightly job that has been broken for a
// week is otherwise invisible: nobody is watching at 03:17.
func TestSchedulerReportsJobFailures(t *testing.T) {
	s, client, _ := schedulerFixture(t, "* * * * *")
	client.fail = true

	var reported atomic.Int32
	var lastErr atomic.Pointer[string]
	s.OnJobResult = func(_, _ string, err error) {
		if err != nil {
			reported.Add(1)
			msg := err.Error()
			lastErr.Store(&msg)
		}
	}

	s.SetClock(func() time.Time { return time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC) })
	s.Tick(context.Background())
	s.Stop() // waits for the job goroutine

	if reported.Load() != 1 {
		t.Fatalf("%d failure(s) reported, want 1", reported.Load())
	}
	if p := lastErr.Load(); p == nil || *p != "job failed" {
		t.Errorf("reported error = %v, want the plugin's own message", lastErr.Load())
	}
}

// A running job holds a request slot, so a drain waits for it rather than
// killing the process mid-job.
func TestSchedulerJobHoldsARequestSlot(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	client := &blockingJobClient{started: started, release: release}

	inst := NewInstance("jobs", "jobs-0", "1.0.0", 1, client, &fakeProc{})
	inst.MarkReady()

	reg := NewRegistry()
	reg.InstallPlugin(Registration{Key: "jobs", Instances: []*Instance{inst}})

	pkgs := jobPackages{"jobs": {Manifest: &manifest.Manifest{
		Key:  "jobs",
		Jobs: []manifest.JobDecl{{Name: "nightly", Cron: "* * * * *"}},
	}}}

	s := NewScheduler(reg, pkgs)
	s.SetClock(func() time.Time { return time.Date(2026, 8, 12, 3, 17, 0, 0, time.UTC) })
	s.Tick(context.Background())

	<-started
	if got := inst.InFlight(); got != 1 {
		t.Errorf("in-flight = %d while a job is running, want 1; a drain would not wait for it", got)
	}

	close(release)
	s.Stop()

	if got := inst.InFlight(); got != 0 {
		t.Errorf("in-flight = %d after the job finished", got)
	}
}

type blockingJobClient struct {
	fakeClient
	started chan struct{}
	release chan struct{}
}

func (b *blockingJobClient) RunJob(context.Context, *pb.JobRequest) (*pb.JobResponse, error) {
	close(b.started)
	<-b.release
	return &pb.JobResponse{Success: true}, nil
}
