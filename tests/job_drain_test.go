package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// What a long-running job costs an upgrade.
//
// The notes example's README says: "a job's handler holds one of the plugin's
// request slots while it runs, so draining waits for it — a nightly rollup
// that takes ten minutes makes an upgrade during those ten minutes wait."
//
// The first half is true. The second is not, and the difference matters: a
// drain waits for its timeout and no longer, then terminates whatever is still
// in flight. So a ten-minute job does not delay an upgrade by ten minutes — it
// gets cut off after thirty seconds, and if it is not idempotent that is
// half-finished work rather than a slow deployment.
//
// The README's version reads as a performance note. The real behaviour is a
// correctness note, and it belongs where somebody writing a nightly rollup
// will see it.

// jobInFlight starts a job that outlasts any drain, and returns once it is
// actually running.
func jobInFlight(t *testing.T, inst *pluginhost.Instance) func() {
	t.Helper()

	release, ok := inst.BeginRequest()
	if !ok {
		t.Fatal("could not take a request slot on a ready instance")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer release()
		// The fixture sleeps for ECHO_SLOW_JOB before answering.
		_, _ = inst.Client.RunJob(context.Background(), jobRequest())
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if inst.InFlight() > 0 {
			return func() { <-done }
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the job never showed up as in flight")
	return nil
}

// A drain waits for its timeout and then stops waiting.
func TestDrainDoesNotWaitForALongJob(t *testing.T) {
	inst := launchWithSlowJob(t, "slowjob", "10s")
	wait := jobInFlight(t, inst)

	const timeout = 400 * time.Millisecond
	start := time.Now()
	err := inst.Drain(context.Background(), timeout)
	took := time.Since(start)

	t.Logf("drain of a plugin running a 10s job took %s: %v", took.Round(time.Millisecond), err)

	// It must not have waited for the job. Generous upper bound — what is
	// being ruled out is "waited ten seconds", not a few milliseconds of
	// scheduling.
	if took > 3*time.Second {
		t.Errorf("the drain waited %s; it was supposed to give up after %s", took, timeout)
	}
	// And it must say what it left behind, or an operator has no way to know
	// a job was cut off.
	if err == nil {
		t.Error("the drain reported success while a job was still running; nothing tells " +
			"an operator that work was terminated mid-flight")
	} else if !strings.Contains(err.Error(), "in flight") {
		t.Errorf("the drain error does not say what was still running: %v", err)
	}

	wait()
}

// The other half, which the README got right: a job does occupy a request
// slot, so a drain notices it at all.
//
// Without this the test above passes for a plugin whose jobs are invisible to
// the drain — which would be a different bug with the same timing.
func TestAJobOccupiesARequestSlot(t *testing.T) {
	inst := launchWithSlowJob(t, "slotjob", "2s")

	if got := inst.InFlight(); got != 0 {
		t.Fatalf("in-flight = %d before anything started", got)
	}
	wait := jobInFlight(t, inst)

	if got := inst.InFlight(); got == 0 {
		t.Error("a running job does not count as in flight, so a drain would not wait " +
			"for it at all and would kill it immediately")
	}
	wait()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if inst.InFlight() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("in-flight = %d after the job finished; the slot was never released",
		inst.InFlight())
}

// A job that finishes inside the drain budget is waited for, which is the
// behaviour worth keeping: short jobs are not cut off.
func TestDrainWaitsForAShortJob(t *testing.T) {
	inst := launchWithSlowJob(t, "quickjob", "200ms")
	wait := jobInFlight(t, inst)

	start := time.Now()
	err := inst.Drain(context.Background(), 5*time.Second)
	took := time.Since(start)

	t.Logf("drain of a plugin running a 200ms job took %s", took.Round(time.Millisecond))
	if err != nil {
		t.Errorf("draining a plugin whose job finishes in time reported: %v", err)
	}
	if took > 3*time.Second {
		t.Errorf("the drain took %s for a 200ms job", took)
	}
	wait()
}

// launchWithSlowJob starts the fixture with a job that takes the given time.
func launchWithSlowJob(t *testing.T, key, dur string) *pluginhost.Instance {
	t.Helper()
	inst, err := launchWithEnv(t, key, "ECHO_SLOW_JOB="+dur)
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	return inst
}

func jobRequest() *pb.JobRequest {
	return &pb.JobRequest{JobName: "nightly", TraceId: "job-trace"}
}
