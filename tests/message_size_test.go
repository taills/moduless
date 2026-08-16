package tests

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/pluginapi"
	pb "github.com/taills/moduless/proto/plugin"
)

// The message-size ceiling, from both sides of it.
//
// gRPC defaults to 4 MiB and go-plugin does not override it, so a framework
// built on it inherits that limit unless both ends raise it — and the failure
// when they have not is an opaque ResourceExhausted rather than anything an
// operator or a plugin author can act on. This project's own plan called it
// out as a trap the team had already been bitten by, and set both sides to
// 16 MiB.
//
// Nothing had ever sent a message big enough to find out whether that took
// effect. A ceiling that is configured on paper and not in fact looks
// identical to a working one until the day something large goes through it.

// bodyOf asks the fixture for a response of exactly n bytes.
func bodyOf(t *testing.T, inst *pluginhost.Instance, n int) (*pb.HttpResponse, error) {
	t.Helper()
	return inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet,
		Path:   "/large",
		Query:  fmt.Sprint(n),
	})
}

// A response larger than gRPC's own default gets through, which is the whole
// point of raising it. Eight megabytes is comfortably over 4 MiB and
// comfortably under the 16 MiB this framework sets.
func TestResponseAboveTheGRPCDefaultGetsThrough(t *testing.T) {
	inst := launchPlugin(t, "big", "1.0.0", nil)

	const size = 8 << 20
	resp, err := bodyOf(t, inst, size)
	if err != nil {
		t.Fatalf("an %d-byte response failed: %v\n"+
			"gRPC's default ceiling is 4 MiB; if this is ResourceExhausted then the "+
			"raise to %d bytes is configured somewhere it does not take effect",
			size, err, pluginapi.DefaultMaxMessageBytes)
	}
	if got := len(resp.GetBody()); got != size {
		t.Errorf("got %d bytes back, sent %d; something truncated it silently", got, size)
	}
}

// And the boundary itself: just under the ceiling works.
//
// Worth its own case because a limit that is off by a header's worth of bytes
// passes the 8 MiB test above and fails the first time somebody sends
// something genuinely large.
func TestResponseJustUnderTheCeilingGetsThrough(t *testing.T) {
	inst := launchPlugin(t, "big", "1.0.0", nil)

	// A margin for the rest of the message — headers, status, framing.
	size := pluginapi.DefaultMaxMessageBytes - (64 << 10)
	resp, err := bodyOf(t, inst, size)
	if err != nil {
		t.Fatalf("a %d-byte response failed with the ceiling at %d: %v",
			size, pluginapi.DefaultMaxMessageBytes, err)
	}
	if got := len(resp.GetBody()); got != size {
		t.Errorf("got %d bytes back, sent %d", got, size)
	}
}

// Over the ceiling, the failure has to be legible.
//
// This is the half that matters to whoever meets it: a plugin author whose
// response is too large needs to learn that from the error, not from a code
// that also means "the server is overloaded". The plan for this framework
// named this specifically — the previous architecture reported every send
// failure as one opaque string, and the team had already lost time to it.
func TestResponseOverTheCeilingFailsLegibly(t *testing.T) {
	inst := launchPlugin(t, "big", "1.0.0", nil)

	size := pluginapi.DefaultMaxMessageBytes + (4 << 20)
	resp, err := bodyOf(t, inst, size)
	if err == nil {
		t.Fatalf("a %d-byte response succeeded against a %d-byte ceiling (%d bytes back)",
			size, pluginapi.DefaultMaxMessageBytes, len(resp.GetBody()))
	}
	t.Logf("over the ceiling: %v", err)

	// What an author can act on: the message has to say it was about size.
	// "ResourceExhausted" alone reads as back-pressure and sends them looking
	// at load rather than at the size of what they returned.
	msg := err.Error()
	saysSize := strings.Contains(msg, "message") || strings.Contains(msg, "size") ||
		strings.Contains(msg, "larger") || strings.Contains(msg, "max")
	if !saysSize {
		t.Errorf("the error does not mention size: %q\n"+
			"a plugin author reading this goes looking for load or a broken connection", msg)
	}

	// And the plugin survives it. A response it could not send must not take
	// the process with it — the next request has to work.
	if inst.ProcessExited() {
		t.Fatal("the plugin process died trying to send an oversized response")
	}
	small, err := bodyOf(t, inst, 1024)
	if err != nil {
		t.Errorf("the plugin stopped serving after an oversized response: %v", err)
	} else if len(small.GetBody()) != 1024 {
		t.Errorf("the next response was %d bytes", len(small.GetBody()))
	}
}

// The ceiling is negotiated, not assumed.
//
// Core tells the plugin what it will accept during Configure, so the two sides
// cannot drift: a plugin built against an older SDK with a smaller default
// would otherwise refuse messages Core happily sends. Checked by watching a
// message that only fits under the negotiated value actually arrive.
func TestBothSidesAgreeOnTheCeiling(t *testing.T) {
	inst := launchPlugin(t, "big", "1.0.0", nil)

	// Six megabytes: over gRPC's default in both directions, so it can only
	// work if the plugin raised its send limit *and* Core raised its receive
	// limit. Either one left at the default fails this.
	const size = 6 << 20
	resp, err := bodyOf(t, inst, size)
	if err != nil {
		t.Fatalf("%d bytes failed: %v; one side is still on gRPC's 4 MiB default", size, err)
	}
	if len(resp.GetBody()) != size {
		t.Errorf("got %d bytes", len(resp.GetBody()))
	}
}

// What an HTTP caller sees when a plugin returns more than the transport
// carries.
//
// The plan for this framework asked for "a clear 413 rather than an opaque
// ResourceExhausted", and 413 turns out to be the wrong code — it means the
// *request* entity was too large, and this is the response. What a caller
// needs is a 5xx that names the cause, since nothing they change about their
// request will fix a plugin returning 20MB. Core's 502 is right; the test is
// that the reason survives to the caller instead of being replaced by
// something generic.
func TestOversizedResponseReachesTheCallerWithAReason(t *testing.T) {
	inst := launchPlugin(t, "big", "1.0.0", nil)
	reg := pluginhost.NewRegistry()
	reg.InstallPlugin(pluginhost.Registration{
		Key:       "big",
		Instances: []*pluginhost.Instance{inst},
	})

	srv := newGateway(reg)
	defer srv.Close()

	size := pluginapi.DefaultMaxMessageBytes + (4 << 20)
	status, body, _ := get(t, fmt.Sprintf("%s/api/plugins/big/large?%d", srv.URL, size))

	t.Logf("caller got %d: %s", status, strings.TrimSpace(body))

	if status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502; the plugin was reachable and produced something "+
			"Core could not carry, which is neither a missing route nor a transient state",
			status)
	}
	if !strings.Contains(body, "larger than max") && !strings.Contains(body, "size") {
		t.Errorf("body = %q; the reason did not survive to the caller, so whoever is "+
			"debugging this sees a bare 502", body)
	}

	// A request that fits still works afterwards, through the same gateway.
	okStatus, okBody, _ := get(t, srv.URL+"/api/plugins/big/large?1024")
	if okStatus != http.StatusOK || len(okBody) != 1024 {
		t.Errorf("after an oversized response: status %d, %d bytes", okStatus, len(okBody))
	}
}
