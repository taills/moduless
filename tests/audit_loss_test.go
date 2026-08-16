package tests

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/manifest"
)

// What "the audit record silently disappears" is actually worth.
//
// The audit example's README says a log-phase filter is fail-open, so when the
// plugin is down "the write request succeeds and the record silently
// vanishes". That is the right warning to give — an audit log that is
// best-effort by default is a thing people must know — but "silently" is the
// word nobody checks, and it is doing a lot of work in that sentence.
//
// Two separate questions, and only the first was ever stated: does the request
// still succeed (it must, that is what fail-open means), and is anybody told
// that a record was lost. If Core says nothing, the audit trail has holes no
// one can even count. If Core does say something, the README is scaring
// authors away from a mechanism that is more honest than described.

// deadFilterGateway installs a log-phase filter on a plugin that is about to
// die, and records what Core reports.
func deadFilterGateway(t *testing.T) (url string, errs *filterErrors, kill func()) {
	t.Helper()

	auditor := launchPlugin(t, "auditor", "1.0.0", nil)
	backend := launchPlugin(t, "hello", "1.0.0", nil)

	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "auditor",
		Instances: []*pluginhost.Instance{auditor},
		Filters: compileFilters(t, "auditor", manifest.FilterDecl{
			Name:  "access-log",
			Phase: manifest.PhaseLog,
			Match: manifest.FilterMatch{Paths: []string{"/**"}},
		}),
	})
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "hello",
		Instances: []*pluginhost.Instance{backend},
	})

	errs = &filterErrors{}
	h := &gateway.PluginHandler{
		Registry: reg,
		Runner:   &pipeline.Runner{OnFilterError: errs.record},
	}
	srv := httptest.NewServer(h.Middleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) })))
	t.Cleanup(srv.Close)

	return srv.URL, errs, auditor.Kill
}

type filterErrors struct {
	mu   sync.Mutex
	seen []string
}

func (f *filterErrors) record(flt *pipeline.Filter, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = append(f.seen, flt.Label()+": "+err.Error())
}

func (f *filterErrors) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

// The request succeeds when the audit plugin is gone. This is fail-open
// working, and it is what makes an audit filter safe to install.
func TestRequestSucceedsWhenTheAuditPluginIsDead(t *testing.T) {
	url, _, kill := deadFilterGateway(t)

	if status, _, _ := get(t, url+"/api/plugins/hello/items"); status != http.StatusOK {
		t.Fatalf("status = %d before killing the auditor", status)
	}

	kill()

	status, _, _ := get(t, url+"/api/plugins/hello/items")
	if status != http.StatusOK {
		t.Errorf("status = %d with the audit plugin dead; a log-phase filter is fail-open "+
			"precisely so that logging cannot take the site down", status)
	}
}

// But it is not silent. Core reports the filter failure, so the holes in an
// audit trail are countable even though the records are gone.
func TestCoreReportsTheLostAuditRecord(t *testing.T) {
	url, errs, kill := deadFilterGateway(t)

	get(t, url+"/api/plugins/hello/items")
	if got := errs.all(); len(got) != 0 {
		t.Fatalf("errors reported while the auditor was healthy: %v", got)
	}

	kill()
	get(t, url+"/api/plugins/hello/items")

	// The log phase runs after the response is written, so the report arrives
	// after the request returns.
	var got []string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got = errs.all(); len(got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if len(got) == 0 {
		t.Fatal("nothing was reported when a log-phase filter failed; an audit trail with " +
			"holes nobody can count is worse than one that is merely best-effort — there " +
			"is no way to know how much is missing")
	}
	t.Logf("Core reported: %s", got[0])

	// And it names the filter, or an operator with several cannot tell which
	// one is dropping records.
	if !strings.Contains(got[0], "auditor") {
		t.Errorf("the report does not name the plugin: %q", got[0])
	}
}
