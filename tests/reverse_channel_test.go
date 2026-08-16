package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// Where the 850µs of a reverse-channel database call goes.
//
// The full-stack measurement put a Count from a plugin at about 850µs on top
// of the plugin call carrying it — three times the cost of the plugin call
// itself. A localhost PostgreSQL query is usually one or two hundred
// microseconds and each gRPC hop is about forty, so most of that number was
// unaccounted for, and an unexplained number in a performance document is a
// number somebody will optimise the wrong thing against.
//
// This takes the same operation at four depths and subtracts.
//
//	MEASURE=1 TEST_DATABASE_URL=... go test ./tests/ -run TestReverseChannelBreakdown -v

func quantiles(got []time.Duration) (p50, p99 time.Duration) {
	if len(got) == 0 {
		return 0, 0
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	at := func(q float64) time.Duration { return got[int(float64(len(got)-1)*q)] }
	return at(0.50), at(0.99)
}

func timeIt(n int, fn func()) (p50, p99 time.Duration) {
	// Warm, so the first call does not pay for a connection nobody else does.
	for range 20 {
		fn()
	}
	got := make([]time.Duration, 0, n)
	for range n {
		start := time.Now()
		fn()
		got = append(got, time.Since(start))
	}
	return quantiles(got)
}

func TestReverseChannelBreakdown(t *testing.T) {
	if os.Getenv("MEASURE") == "" {
		t.Skip("MEASURE is not set")
	}

	handle := requireDB(t)
	cmds := db.NewCMDSManager(handle)
	txs := db.NewTxRegistry()
	t.Cleanup(txs.Close)

	const key = "breakdown"
	data := hostsvc.NewCMDSData(handle, cmds, txs)
	if err := data.ProvisionSchema(key, []db.CollectionSchema{{Name: "items"}, {Name: "notes"}}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		handle.Exec(`DROP TABLE IF EXISTS ext_breakdown_items`)
		handle.Exec(`DROP TABLE IF EXISTS ext_breakdown_notes`)
	})

	// A little data, so the query does something.
	ctx := context.Background()
	for i := range 50 {
		body, _ := json.Marshal(map[string]any{"n": i, "status": "open"})
		if _, err := data.Put(ctx, key, "items", fmt.Sprint(i), body, "", 0); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	const n = 300
	t.Logf("%-52s %-12s %s", "layer", "p50", "p99")

	// 1. The database on its own, through Core's own query layer.
	p50DB, p99DB := timeIt(n, func() {
		_, _ = cmds.Aggregate(ctx, nil, key, "items", db.AggregateOptions{Func: db.AggCount})
	})
	t.Logf("%-52s %-12s %s", "1. PostgreSQL through Core's query layer",
		p50DB.Round(time.Microsecond), p99DB.Round(time.Microsecond))

	// 2. Plus the host service, in process — permission checks, wire types.
	server := hostsvc.New(key, []string{"db"}, hostsvc.Deps{Data: data})
	p50Host, p99Host := timeIt(n, func() {
		_, _ = server.Aggregate(ctx, &pb.AggregateRequest{
			Collection: "items", Func: pb.AggregateFunc_AGG_COUNT,
		})
	})
	t.Logf("%-52s %-12s %s", "2. + the host service, in process",
		p50Host.Round(time.Microsecond), p99Host.Round(time.Microsecond))

	// 3. Plus the reverse gRPC channel: a real plugin process calling back.
	//
	// Launched here rather than through launchPlugin, whose Deps carry no data
	// backend — a plugin started that way answers /db with "not configured" in
	// microseconds, which is what made the first run of this test look fast
	// and inverted.
	inst, err := pluginhost.Launch(ctx, pluginhost.LaunchSpec{
		Key:        key,
		InstanceID: key + "-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New(key, []string{"db"}, hostsvc.Deps{
			Config: hostsvc.NewStaticConfig(),
			Data:   data,
		}),
		GrantedPermissions: []string{"db"},
		Env:                []string{"PATH=/usr/bin:/bin"},
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(inst.Kill)
	// Probed once before anything is timed. Three attempts at this
	// measurement timed a route that was failing in microseconds, and each
	// time the table looked like an answer — the route name was wrong, so the
	// fixture fell through to its default reply and never touched a database.
	probe, err := inst.Client.HandleHTTP(ctx, &pb.HttpRequest{Method: "GET", Path: "/db"})
	if err != nil {
		t.Fatalf("the probe call failed: %v", err)
	}
	if probe.GetStatusCode() != 200 {
		t.Fatalf("the plugin's database route returned %d: %s\ntiming this would measure "+
			"how fast it fails", probe.GetStatusCode(), probe.GetBody())
	}
	t.Logf("probe returned: %s", probe.GetBody())

	p50RPC, p99RPC := timeIt(n, func() {
		_, _ = inst.Client.HandleHTTP(ctx, &pb.HttpRequest{
			Method: "GET", Path: "/db",
		})
	})
	t.Logf("%-52s %-12s %s", "3. + a plugin calling back (write + read) over the channel",
		p50RPC.Round(time.Microsecond), p99RPC.Round(time.Microsecond))

	// 4. A plugin call that touches nothing, for subtraction.
	p50Bare, p99Bare := timeIt(n, func() {
		_, _ = inst.Client.HandleHTTP(ctx, &pb.HttpRequest{Method: "GET", Path: "/items"})
	})
	t.Logf("%-52s %-12s %s", "4. a plugin call using no host capability",
		p50Bare.Round(time.Microsecond), p99Bare.Round(time.Microsecond))

	// The arithmetic, stated with what each layer actually does. Layer 3 is a
	// write and a read back — two operations — so it cannot be subtracted from
	// a single Count without saying so.
	perOp := (p50RPC - p50Bare) / 2
	t.Logf("")
	t.Logf("a database operation, no framework:     %s", p50DB.Round(time.Microsecond))
	t.Logf("the host service adds:                  %s (within noise of zero)",
		(p50Host - p50DB).Round(time.Microsecond))
	t.Logf("a plugin call carrying nothing:         %s", p50Bare.Round(time.Microsecond))
	t.Logf("one reverse-channel database operation: %s", perOp.Round(time.Microsecond))
	t.Logf("  of which the database itself:         %s", p50DB.Round(time.Microsecond))
	t.Logf("  so the reverse channel costs about:   %s",
		(perOp - p50DB).Round(time.Microsecond))

	// The measurement checks itself, because earlier versions did not and
	// printed a plausible table three times over for runs that had measured
	// the wrong thing: the plugin route was misspelled, the fixture fell
	// through to its default reply, and layer 3 came out faster than the
	// database alone. A strictly larger amount of work cannot take less time,
	// so an inversion means the layers are not nested the way the labels say.
	if p50RPC < p50DB {
		t.Fatalf("layer 3 (%s) is faster than layer 1 (%s); it does strictly more work, "+
			"so these are not measuring what the labels claim", p50RPC, p50DB)
	}
	// Layer 2 against layer 1 is deliberately loose. The host service adds a
	// permission check and a type conversion, too small to separate from
	// run-to-run variance — the finding is that it costs nothing measurable,
	// and a strict comparison here fails on noise rather than on a regression.
	if p50Host < p50DB*8/10 {
		t.Errorf("layer 2 (%s) is more than 20%% faster than layer 1 (%s); the host "+
			"service cannot cost negative time", p50Host, p50DB)
	}
}
