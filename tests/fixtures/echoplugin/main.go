// Command echoplugin is a minimal plugin binary used by Core's tests and
// benchmarks. It is built on demand by TestMain; it is not shipped.
//
// It exercises the full contract surface: the handshake, the initial
// Configure, the reverse HostServices connection, the filter pipeline and the
// HTTP backend path.
//
// Nothing here may write to stdout. go-plugin reads its handshake from the
// child's first stdout line, so a stray fmt.Println would break startup. The
// log package writes to stderr, which Core captures separately.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/taills/moduless/pluginapi"
	pb "github.com/taills/moduless/proto/plugin"
)

type echoImpl struct {
	mu       sync.RWMutex
	host     pb.HostServicesClient
	key      string
	hostEcho string // value round-tripped through the reverse connection
}

func (e *echoImpl) bindHost(conn *grpc.ClientConn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.host = pb.NewHostServicesClient(conn)
}

func (e *echoImpl) Configure(ctx context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	e.mu.Lock()
	e.key = req.GetPluginKey()
	host := e.host
	e.mu.Unlock()

	if host == nil {
		return &pb.ConfigureResponse{Ready: false, Error: "host services not bound"}, nil
	}

	// Prove the reverse channel carries data, not just connects: ask Core for
	// this plugin's config and keep what comes back.
	resp, err := host.GetConfig(ctx, &emptypb.Empty{})
	if err != nil {
		return &pb.ConfigureResponse{Ready: false, Error: "GetConfig: " + err.Error()}, nil
	}

	e.mu.Lock()
	e.hostEcho = resp.GetConfig()["greeting"]
	e.mu.Unlock()

	log.Printf("echoplugin: configured key=%s perms=%v greeting=%q",
		req.GetPluginKey(), req.GetGrantedPermissions(), e.hostEcho)
	return &pb.ConfigureResponse{Ready: true}, nil
}

func (e *echoImpl) HandleHTTP(_ context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
	e.mu.RLock()
	greeting := e.hostEcho
	e.mu.RUnlock()

	body := req.GetBody()
	switch req.GetPath() {
	case "/env":
		// Reports what the child process can actually see, so Core's tests can
		// prove SkipHostEnv really withheld its own environment rather than
		// just assuming it did.
		body = []byte(strings.Join(os.Environ(), "\n"))
	case "/crash":
		// Dies the way a panic or an OOM kill would: abruptly, without going
		// through Shutdown, and without Core having asked. This is what the
		// supervisor's crash recovery is for, and it is deliberately distinct
		// from Core stopping the plugin on purpose.
		log.Print("echoplugin: exiting abruptly on request")
		os.Exit(3)
	}

	return &pb.HttpResponse{
		StatusCode: 200,
		Headers: map[string]*pb.HeaderValues{
			"Content-Type":  {Values: []string{"text/plain"}},
			"X-Echo-Path":   {Values: []string{req.GetPath()}},
			"X-Host-Config": {Values: []string{greeting}},
			// Two values on one header, to prove repeated headers survive the
			// round trip. The legacy tunnel dropped all but the first.
			"X-Multi": {Values: []string{"a", "b"}},
		},
		Body: body,
	}, nil
}

func (e *echoImpl) Filter(_ context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error) {
	switch req.GetPath() {
	case "/deny":
		return &pb.FilterResponse{
			Action: pb.FilterResponse_ACTION_SHORT_CIRCUIT,
			ShortCircuitResponse: &pb.HttpResponse{
				StatusCode: 403,
				Body:       []byte("denied by echoplugin"),
			},
		}, nil
	case "/mutate":
		return &pb.FilterResponse{
			Action: pb.FilterResponse_ACTION_MUTATE,
			Mutation: &pb.RequestMutation{
				SetRequestHeaders: map[string]*pb.HeaderValues{
					"X-Added-By-Filter": {Values: []string{"echoplugin"}},
				},
			},
		}, nil
	case "/boom":
		return nil, fmt.Errorf("echoplugin: deliberate filter failure")
	default:
		return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}, nil
	}
}

func (e *echoImpl) RunJob(_ context.Context, req *pb.JobRequest) (*pb.JobResponse, error) {
	log.Printf("echoplugin: job %s trace=%s", req.GetJobName(), req.GetTraceId())
	return &pb.JobResponse{Success: true}, nil
}

func (e *echoImpl) OnConfigChanged(_ context.Context, req *pb.ConfigChangeEvent) error {
	e.mu.Lock()
	e.hostEcho = req.GetConfig()["greeting"]
	e.mu.Unlock()
	return nil
}

func (e *echoImpl) Shutdown(_ context.Context, req *pb.ShutdownRequest) error {
	log.Printf("echoplugin: shutdown reason=%q", req.GetReason())
	return nil
}

func main() {
	// ECHO_FAIL_CONFIGURE lets tests exercise Core's launch-failure and
	// rollback paths without needing a second broken binary.
	if os.Getenv("ECHO_FAIL_CONFIGURE") == "1" {
		impl := &failingImpl{}
		pluginapi.Serve(pluginapi.ServeConfig{Impl: impl, HostBinder: func(*grpc.ClientConn) {}})
		return
	}

	impl := &echoImpl{}
	pluginapi.Serve(pluginapi.ServeConfig{
		Impl:       impl,
		HostBinder: impl.bindHost,
	})
}

type failingImpl struct{ echoImpl }

func (f *failingImpl) Configure(context.Context, *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	return &pb.ConfigureResponse{Ready: false, Error: "deliberate configure failure"}, nil
}
