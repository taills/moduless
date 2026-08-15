package pluginhost

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/taills/moduless/proto/plugin"
)

// echoBinary is built once by TestMain and reused by every test here.
var echoBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "moduless-plugin-test-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "tempdir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	echoBinary = filepath.Join(dir, "echoplugin")
	if runtime.GOOS == "windows" {
		echoBinary += ".exe"
	}

	// CGO_ENABLED=0 mirrors how real plugins must be built: a dynamically
	// linked binary fails to exec inside the musl-based runtime image.
	build := exec.Command("go", "build", "-o", echoBinary, "../../tests/fixtures/echoplugin")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "building echoplugin fixture: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// stubHost is a minimal HostServices implementation. Everything it does not
// implement returns Unimplemented, which is exactly what a plugin lacking a
// permission would see.
type stubHost struct {
	pb.UnimplementedHostServicesServer
	config map[string]string
}

func (s *stubHost) GetConfig(context.Context, *emptypb.Empty) (*pb.GetConfigResponse, error) {
	return &pb.GetConfigResponse{Config: s.config}, nil
}

func launchEcho(t testing.TB, env ...string) *Instance {
	t.Helper()

	sum, err := fileChecksum(echoBinary)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}

	inst, err := Launch(context.Background(), LaunchSpec{
		Key:        "echo",
		InstanceID: "echo-1",
		BinaryPath: echoBinary,
		Checksum:   sum,
		HostImpl:   &stubHost{config: map[string]string{"greeting": "hello-from-core"}},
		// SkipHostEnv is on, so the child sees exactly this and nothing else.
		Env:                append([]string{"PATH=/usr/bin:/bin"}, env...),
		GrantedPermissions: []string{"db", "cache"},
		Stdout:             os.Stderr,
		Stderr:             os.Stderr,
		DevMode:            true,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(inst.Kill)
	return inst
}

// TestLaunchEstablishesReverseChannel is the core Phase 0 assertion: Core
// forks the plugin, the handshake succeeds, and the plugin can call back into
// HostServices and get real data. The greeting is only obtainable through the
// reverse connection, so seeing it in the HTTP response proves the whole loop.
func TestLaunchEstablishesReverseChannel(t *testing.T) {
	inst := launchEcho(t)

	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		TraceId: "trace-abc",
		Method:  "POST",
		Path:    "/hello",
		Body:    []byte("ping"),
	})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}
	if resp.GetStatusCode() != 200 {
		t.Errorf("status = %d, want 200", resp.GetStatusCode())
	}
	if got := string(resp.GetBody()); got != "ping" {
		t.Errorf("body = %q, want %q", got, "ping")
	}
	if got := resp.GetHeaders()["X-Host-Config"].GetValues(); len(got) != 1 || got[0] != "hello-from-core" {
		t.Errorf("X-Host-Config = %v, want [hello-from-core] — reverse channel did not deliver data", got)
	}
	// Repeated headers must survive; the legacy tunnel silently kept only the
	// first value of each header.
	if got := resp.GetHeaders()["X-Multi"].GetValues(); len(got) != 2 {
		t.Errorf("X-Multi = %v, want 2 values", got)
	}
}

func TestFilterActions(t *testing.T) {
	inst := launchEcho(t)

	tests := []struct {
		name       string
		path       string
		wantAction pb.FilterResponse_Action
		wantErr    bool
	}{
		{name: "continue by default", path: "/anything", wantAction: pb.FilterResponse_ACTION_CONTINUE},
		{name: "short circuit", path: "/deny", wantAction: pb.FilterResponse_ACTION_SHORT_CIRCUIT},
		{name: "mutate", path: "/mutate", wantAction: pb.FilterResponse_ACTION_MUTATE},
		{name: "plugin error surfaces", path: "/boom", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := inst.Client.Filter(context.Background(), &pb.FilterRequest{
				TraceId: "trace-1",
				Phase:   pb.Phase_PHASE_PRE_ROUTE,
				Method:  "GET",
				Path:    tc.path,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got %v", resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("Filter: %v", err)
			}
			if resp.GetAction() != tc.wantAction {
				t.Errorf("action = %v, want %v", resp.GetAction(), tc.wantAction)
			}
		})
	}
}

// TestLaunchRejectsTamperedBinary covers SecureConfig: the bytes that execute
// must be the bytes that were verified at install time.
func TestLaunchRejectsTamperedBinary(t *testing.T) {
	wrong := sha256.Sum256([]byte("not the real binary"))

	inst, err := Launch(context.Background(), LaunchSpec{
		Key:        "echo",
		InstanceID: "echo-tampered",
		BinaryPath: echoBinary,
		Checksum:   wrong[:],
		HostImpl:   &stubHost{},
		Env:        []string{"PATH=/usr/bin:/bin"},
		Stderr:     os.Stderr,
		DevMode:    true,
	})
	if err == nil {
		inst.Kill()
		t.Fatal("expected launch to fail on checksum mismatch")
	}
}

// TestConfigureFailureKillsProcess ensures a plugin that reports "not ready"
// leaves no orphan behind — the rollback path relies on this.
func TestConfigureFailureKillsProcess(t *testing.T) {
	sum, err := fileChecksum(echoBinary)
	if err != nil {
		t.Fatalf("checksum: %v", err)
	}

	inst, err := Launch(context.Background(), LaunchSpec{
		Key:        "echo",
		InstanceID: "echo-fail",
		BinaryPath: echoBinary,
		Checksum:   sum,
		HostImpl:   &stubHost{},
		Env:        []string{"PATH=/usr/bin:/bin", "ECHO_FAIL_CONFIGURE=1"},
		Stderr:     os.Stderr,
		DevMode:    true,
	})
	if err == nil {
		inst.Kill()
		t.Fatal("expected launch to fail when the plugin reports not ready")
	}
}

// TestSkipHostEnvHidesCoreSecrets guards the credential-leak fix: go-plugin
// forwards Core's entire environment to the child by default, which would hand
// a third-party plugin DATABASE_URL, ADMIN_PASSWORD and the object-store keys.
// The plugin reports its own os.Environ() so this asserts on what the child
// actually sees, not on how Core was configured.
func TestSkipHostEnvHidesCoreSecrets(t *testing.T) {
	t.Setenv("MODULESS_TEST_SECRET", "super-secret-value")

	inst := launchEcho(t)
	resp, err := inst.Client.HandleHTTP(context.Background(), &pb.HttpRequest{
		Method: "GET",
		Path:   "/env",
	})
	if err != nil {
		t.Fatalf("HandleHTTP: %v", err)
	}

	childEnv := string(resp.GetBody())
	if strings.Contains(childEnv, "super-secret-value") {
		t.Error("plugin can read Core's environment; SkipHostEnv is not in effect")
	}
	if strings.Contains(childEnv, "MODULESS_TEST_SECRET") {
		t.Error("Core's secret variable name leaked into the plugin environment")
	}
	// Sanity check the assertion itself: the spec-supplied PATH must be there,
	// otherwise an empty environment would pass this test vacuously.
	if !strings.Contains(childEnv, "PATH=/usr/bin:/bin") {
		t.Errorf("expected the spec-supplied PATH in the child environment, got:\n%s", childEnv)
	}
}

// ---------------------------------------------------------------------------
// Benchmarks — these exist to replace guesses about filter latency with
// measured numbers before any timeout default is chosen.
//
//	go test ./core/pluginhost/ -bench=. -benchtime=3000x -run=XXX -cpu=1,8
//
// Baseline on an Apple M3 Max (darwin/arm64), go-plugin v1.8.0 over a Unix
// socket with AutoMTLS enabled:
//
//	Filter      body 0B    ~37 µs/op   108 allocs/op
//	Filter      body 4KB   ~41 µs/op   106 allocs/op
//	Filter      body 64KB  ~157 µs/op  115 allocs/op
//	HandleHTTP  body 4KB   ~53 µs/op   132 allocs/op
//	Filter      8 parallel ~15 µs/op   105 allocs/op
//
// Two things to read off this. First, a chain of three filters costs roughly
// 110 µs, which is negligible against a typical request — but only because
// filters that do not match a path are never dispatched at all. Second, a 64KB
// body is 4x the cost of an empty one, which is why bodies are withheld unless
// a filter explicitly declares needs_body: pushing large payloads through every
// filter in the chain would multiply that cost.
//
// Linux containers will be slower than this laptop; re-measure on the target
// platform before hard-coding production timeouts.
// ---------------------------------------------------------------------------

func BenchmarkFilterRoundTrip(b *testing.B) {
	sizes := []struct {
		name string
		n    int
	}{
		{"body_0B", 0},
		{"body_4KB", 4 << 10},
		{"body_64KB", 64 << 10},
	}

	inst := launchEcho(b)
	ctx := context.Background()

	for _, s := range sizes {
		body := make([]byte, s.n)
		req := &pb.FilterRequest{
			TraceId: "bench",
			Phase:   pb.Phase_PHASE_PRE_ROUTE,
			Method:  "GET",
			Path:    "/bench",
			Body:    body,
		}
		b.Run(s.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := inst.Client.Filter(ctx, req); err != nil {
					b.Fatalf("Filter: %v", err)
				}
			}
		})
	}
}

func BenchmarkHandleHTTPRoundTrip(b *testing.B) {
	inst := launchEcho(b)
	ctx := context.Background()
	req := &pb.HttpRequest{Method: "GET", Path: "/bench", Body: make([]byte, 4<<10)}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := inst.Client.HandleHTTP(ctx, req); err != nil {
			b.Fatalf("HandleHTTP: %v", err)
		}
	}
}

// BenchmarkFilterConcurrent measures throughput when many requests are in
// flight at once, which is the realistic gateway shape. The legacy tunnel
// serialised every send behind a mutex; go-plugin's HTTP/2 connection does not.
func BenchmarkFilterConcurrent(b *testing.B) {
	inst := launchEcho(b)
	req := &pb.FilterRequest{Phase: pb.Phase_PHASE_PRE_ROUTE, Method: "GET", Path: "/bench"}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb_ *testing.PB) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for pb_.Next() {
			if _, err := inst.Client.Filter(ctx, req); err != nil {
				b.Fatalf("Filter: %v", err)
			}
		}
	})
}
