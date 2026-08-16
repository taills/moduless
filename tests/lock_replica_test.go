package tests

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// Locks across replicas of one plugin, which is the only thing they are for.
//
// Core's scheduler already picks a single replica per cron job, so a job does
// not need one. What does is work two replicas can start independently — two
// queue consumers reaching the same external account, an index rebuild that
// must not overlap. The lock backend has four tests and the SDK wrapper's
// durations now have their own, but nothing had ever checked that two
// processes are actually excluded from each other, which is the whole claim.
//
// It rests on a detail that is easy to get wrong in either direction: every
// plugin process shares one lock table, held by Core and namespaced per plugin
// key. main.go builds exactly one NewMemoryLocks and hands the same Deps to
// every plugin. The test helpers here do not — launchReplica builds a fresh
// Deps per instance — so a test written on top of them would show two replicas
// happily holding the same lock and the fault would be in the scaffolding.

// launchSharedReplicas starts n replicas of one plugin over a single set of
// host capabilities, the way Core does.
func launchSharedReplicas(t *testing.T, key string, n int, deps hostsvc.Deps) []*pluginhost.Instance {
	t.Helper()

	out := make([]*pluginhost.Instance, 0, n)
	for i := range n {
		inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
			Key:                key,
			InstanceID:         key + "-" + itoa(i),
			Version:            "1.0.0",
			BinaryPath:         pluginBinary,
			Checksum:           checksum(t, pluginBinary),
			HostImpl:           hostsvc.New(key, []string{"lock"}, deps),
			GrantedPermissions: []string{"lock"},
			Env:                []string{"PATH=/usr/bin:/bin"},
			Stderr:             os.Stderr,
		})
		if err != nil {
			t.Fatalf("launching replica %d: %v", i, err)
		}
		t.Cleanup(inst.Kill)
		out = append(out, inst)
	}
	return out
}

// askForLock calls the fixture's /lock route on one replica directly, so the
// test chooses which process attempts it rather than a load balancer.
func askForLock(t *testing.T, inst *pluginhost.Instance, name string) string {
	t.Helper()

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/lock", Query: name,
	})
	if err != nil {
		t.Fatalf("calling /lock: %v", err)
	}
	if h := resp.GetHeaders()["X-Lock"]; h != nil && len(h.GetValues()) > 0 {
		return h.GetValues()[0]
	}
	return ""
}

func TestTwoReplicasCannotHoldOneLock(t *testing.T) {
	deps := hostsvc.Deps{Locks: hostsvc.NewMemoryLocks()}
	replicas := launchSharedReplicas(t, "worker", 2, deps)

	// The control first: with nobody holding it, a replica gets the lock. If
	// this said "busy" the exclusion result below would be meaningless.
	if got := askForLock(t, replicas[0], "rebuild"); got != "held" {
		t.Fatalf("a replica could not take a free lock (%q); nothing below means "+
			"anything", got)
	}

	// Both at once. The fixture holds for 150ms, so the attempts overlap.
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []string
	)
	for _, inst := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := askForLock(t, inst, "rebuild")
			mu.Lock()
			results = append(results, got)
			mu.Unlock()
		}()
	}
	wg.Wait()

	t.Logf("two replicas asked for the same lock at once: %v", results)

	held := 0
	for _, r := range results {
		if r == "held" {
			held++
		}
	}
	if held != 1 {
		t.Errorf("%d of 2 replicas held the lock; exactly one may. Two processes "+
			"believing they own the same critical section is the failure the lock "+
			"exists to prevent, and it is silent", held)
	}
}

// The same name taken by two different plugins is two different locks.
//
// Namespacing per plugin key is what keeps one plugin from stalling another by
// choosing a common name like "cron" or "sync". Untested, and it fails quietly
// in both directions: too little namespacing wedges unrelated plugins, too
// much makes a plugin's own replicas invisible to each other, which is the
// test above.
func TestOnePluginsLockDoesNotBlockAnother(t *testing.T) {
	deps := hostsvc.Deps{Locks: hostsvc.NewMemoryLocks()}
	first := launchSharedReplicas(t, "alpha", 1, deps)[0]
	second := launchSharedReplicas(t, "beta", 1, deps)[0]

	var (
		wg   sync.WaitGroup
		a, b string
	)
	wg.Add(2)
	go func() { defer wg.Done(); a = askForLock(t, first, "sync") }()
	go func() { defer wg.Done(); b = askForLock(t, second, "sync") }()
	wg.Wait()

	t.Logf("two plugins took the name %q at once: alpha=%q beta=%q", "sync", a, b)
	if a != "held" || b != "held" {
		t.Errorf("alpha=%q beta=%q; a lock name is per plugin, so two plugins picking "+
			"the same obvious word must not queue behind each other", a, b)
	}
}
