package tests

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/pluginhost"
	pb "github.com/taills/moduless/proto/plugin"
)

// The first rule in the plugin guide, and what breaking it actually costs.
//
// "A plugin must never write to stdout" is rule one in CLAUDE.md, listed there
// precisely because it fails in a way that does not point at its cause:
// go-plugin reads the handshake from the child's first stdout line, so a
// debugging fmt.Println replaces it and the plugin never starts.
//
// It had no test. Two things are worth establishing rather than assuming: that
// Core's report of the failure gives an author somewhere to look, and whether
// the rule is as absolute as it is written — a rule stated more strictly than
// reality has a cost of its own, since the obvious workaround for "no printing
// at all" is to stop logging.

// launchWithEnv is launchPlugin with extra environment for the child.
func launchWithEnv(t *testing.T, key string, env ...string) (*pluginhost.Instance, error) {
	t.Helper()

	inst, err := pluginhost.Launch(context.Background(), pluginhost.LaunchSpec{
		Key:        key,
		InstanceID: key + "-0",
		Version:    "1.0.0",
		BinaryPath: pluginBinary,
		Checksum:   checksum(t, pluginBinary),
		HostImpl: hostsvc.New(key, nil, hostsvc.Deps{
			Config: hostsvc.NewStaticConfig(),
		}),
		Env:    append([]string{"PATH=/usr/bin:/bin"}, env...),
		Stderr: os.Stderr,
	})
	if err == nil {
		t.Cleanup(inst.Kill)
	}
	return inst, err
}

// Printing before the handshake stops the plugin starting — and Core has to
// say something an author can act on.
func TestStdoutBeforeTheHandshakeFailsTheLaunch(t *testing.T) {
	inst, err := launchWithEnv(t, "noisy", "ECHO_STDOUT_BEFORE=1")
	if err == nil {
		inst.Kill()
		t.Fatal("a plugin that printed to stdout before the handshake started anyway; " +
			"the guide's first rule describes a failure that did not happen")
	}
	t.Logf("Core reported: %v", err)

	// The bar is not that the message is perfect — go-plugin writes most of it
	// — but that it points at the handshake rather than at something generic.
	// An author who reads "handshake" can find the rule; one who reads
	// "unexpected error" cannot.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "handshake") {
		t.Errorf("the failure does not mention the handshake: %q\n"+
			"this is the framework's most-warned-about mistake and the error is "+
			"an author's only clue", err)
	}
}

// Printing *after* the handshake is a different question, and the guide does
// not distinguish them.
//
// Whatever the answer, it belongs in the documentation: if late stdout is
// harmless then the rule is about start-up and can say so, and if it is fatal
// then a plugin that prints on its hundredth request dies in production having
// passed every test.
func TestStdoutAfterTheHandshake(t *testing.T) {
	inst, err := launchWithEnv(t, "chatty", "ECHO_STDOUT_AFTER=1")
	if err != nil {
		t.Fatalf("the plugin did not start at all: %v", err)
	}

	// The fixture prints 200ms in. Give it time, then keep using the plugin.
	time.Sleep(500 * time.Millisecond)

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: http.MethodGet, Path: "/items",
	})
	if err != nil {
		t.Fatalf("the plugin stopped answering after writing to stdout: %v\n"+
			"then the rule is not about start-up, and a plugin that prints on its "+
			"hundredth request dies in production having passed every test", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Errorf("status = %d after the plugin wrote to stdout", resp.GetStatusCode())
	}
	if inst.ProcessExited() {
		t.Error("the plugin process exited after writing to stdout")
	}

	// Several more, in case the damage is cumulative rather than immediate.
	for range 20 {
		if _, err := inst.Client.HandleHTTP(context.Background(),
			&pb.HttpRequest{Method: http.MethodGet, Path: "/items"}); err != nil {
			t.Fatalf("the connection degraded after stdout output: %v", err)
		}
	}
	t.Log("stdout after the handshake is survivable; the rule is about start-up")
}

// A plugin that says nothing starts, which is the control. Without it the test
// above passes for a Core that cannot launch this fixture at all.
func TestQuietPluginStartsNormally(t *testing.T) {
	inst, err := launchWithEnv(t, "quiet")
	if err != nil {
		t.Fatalf("the unmodified fixture failed to start: %v", err)
	}
	if inst.ProcessExited() {
		t.Fatal("it exited immediately")
	}
}
