package pluginhost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/plugin"
)

// JobSource supplies the jobs a plugin declares. The manager implements it;
// the scheduler takes it as an interface so it can be driven without one.
type JobSource interface {
	Package(key string) (*Package, bool)
}

// Scheduler runs the cron jobs plugins declare in their manifests.
//
// It ticks on a fixed interval and asks each schedule whether it fires in the
// current minute, rather than computing next-fire times. That means it holds no
// state to lose: a Core that restarts mid-minute resumes correctly, and a job
// cannot drift late by accumulating error. What it costs is minute resolution,
// which is all a cron expression has anyway.
//
// A job only runs while its plugin is enabled and serving. Nothing needs to
// unregister a schedule when a plugin is disabled — the plugin simply stops
// appearing in the snapshot, and its jobs stop being asked.
type Scheduler struct {
	registry *Registry
	jobs     JobSource

	// interval is how often schedules are checked. It must divide a minute so
	// every minute is inspected at least once.
	interval time.Duration

	now func() time.Time

	mu sync.Mutex
	// lastRun keys on plugin+job+minute, so a job fires once per matching
	// minute however often the ticker checks. Without it, a 20-second tick
	// would run every job three times.
	lastRun map[string]time.Time

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup

	// OnJobResult, when set, is called after each run. Core wires it to the
	// log: a nightly job that has been failing for a week is invisible
	// otherwise, because nobody is watching when it runs.
	OnJobResult func(pluginKey, jobName string, err error)
}

// DefaultSchedulerInterval checks schedules three times a minute, so a job
// still fires in its minute even if one tick is delayed.
const DefaultSchedulerInterval = 20 * time.Second

func NewScheduler(reg *Registry, jobs JobSource) *Scheduler {
	return &Scheduler{
		registry: reg,
		jobs:     jobs,
		interval: DefaultSchedulerInterval,
		now:      time.Now,
		lastRun:  map[string]time.Time{},
		stop:     make(chan struct{}),
	}
}

// SetInterval overrides the tick rate. Test-only; production wants the default.
func (s *Scheduler) SetInterval(d time.Duration) { s.interval = d }

// SetClock overrides the time source. Test-only.
func (s *Scheduler) SetClock(now func() time.Time) { s.now = now }

// Start begins scheduling. It returns immediately.
func (s *Scheduler) Start(ctx context.Context) {
	s.wg.Go(func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Tick(ctx)
			}
		}
	})
}

// Stop ends scheduling and waits for the loop to finish. Jobs already running
// are not interrupted; they hold a request slot and a drain waits for them.
func (s *Scheduler) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// Tick runs every job whose schedule fires in the current minute. It is
// exported so a test can drive scheduling without waiting on a clock.
func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now()
	snap := s.registry.Current()

	for _, key := range snap.Keys() {
		pkg, ok := s.jobs.Package(key)
		if !ok || pkg.Manifest == nil {
			continue
		}
		for _, job := range pkg.Manifest.Jobs {
			schedule, err := manifest.ParseSchedule(job.Cron)
			if err != nil {
				// Rejected at install time, so reaching here means the package
				// changed underneath us. Skip rather than run at the wrong time.
				continue
			}
			if !schedule.Matches(now) || !s.claim(key, job.Name, now) {
				continue
			}
			s.run(ctx, snap, key, job.Name)
		}
	}
}

// claim reserves this minute for a job, returning false if it already ran.
func (s *Scheduler) claim(pluginKey, jobName string, now time.Time) bool {
	minute := now.Truncate(time.Minute)
	id := pluginKey + "\x00" + jobName

	s.mu.Lock()
	defer s.mu.Unlock()

	if last, ok := s.lastRun[id]; ok && last.Equal(minute) {
		return false
	}
	s.lastRun[id] = minute

	// Forget minutes long past, so a Core running for months does not
	// accumulate an entry per job per minute.
	for k, v := range s.lastRun {
		if minute.Sub(v) > time.Hour {
			delete(s.lastRun, k)
		}
	}
	return true
}

// run invokes one job on one replica.
//
// Picking a single instance is what keeps a job from running once per replica:
// the work is the plugin's, not each process's. Which replica gets it does not
// matter, so this reuses the ordinary load-balancing choice.
func (s *Scheduler) run(ctx context.Context, snap *Snapshot, pluginKey, jobName string) {
	inst, ok := snap.Pick(pluginKey)
	if !ok {
		return
	}
	release, ok := inst.BeginRequest()
	if !ok {
		return
	}

	s.wg.Go(func() {
		defer release()

		resp, err := inst.Client.RunJob(ctx, &pb.JobRequest{
			JobName:       jobName,
			TraceId:       newJobTraceID(),
			ScheduledUnix: s.now().Truncate(time.Minute).Unix(),
		})
		if err == nil && !resp.GetSuccess() && resp.GetError() != "" {
			err = jobError(resp.GetError())
		}
		if s.OnJobResult != nil {
			s.OnJobResult(pluginKey, jobName, err)
		}
	})
}

type jobError string

func (e jobError) Error() string { return string(e) }

// newJobTraceID gives a scheduled run the same kind of id a request gets, so
// everything it does downstream — queries, queue writes, outbound calls — can
// be tied back to the occurrence that caused it.
func newJobTraceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "job-" + hex.EncodeToString(b[:4])
	}
	return hex.EncodeToString(b[:])
}
