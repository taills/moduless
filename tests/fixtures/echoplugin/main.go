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
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/taills/moduless/pluginapi"
	pb "github.com/taills/moduless/proto/plugin"
)

type echoImpl struct {
	mu       sync.RWMutex
	host     pb.HostServicesClient
	key      string
	instance string // which replica this process is, echoed back on responses
	hostEcho string // value round-tripped through the reverse connection

	// Events this process received over its subscription, so a test can prove
	// one plugin heard another across two process boundaries and Core.
	received []string
}

func (e *echoImpl) bindHost(conn *grpc.ClientConn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.host = pb.NewHostServicesClient(conn)
}

func (e *echoImpl) Configure(ctx context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	e.mu.Lock()
	e.key = req.GetPluginKey()
	e.instance = req.GetInstanceId()
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

	// A plugin granted the events capability starts listening. Subscribe
	// blocks until the stream ends, so it belongs in its own goroutine — the
	// mistake this fixture would otherwise be modelling for anyone reading it.
	if slices.Contains(req.GetGrantedPermissions(), "events") {
		// What to listen for is configuration, which is also how a real plugin
		// would decide.
		go e.listen(resp.GetConfig()["subscribe_to"])
	}

	log.Printf("echoplugin: configured key=%s perms=%v greeting=%q",
		req.GetPluginKey(), req.GetGrantedPermissions(), e.hostEcho)
	return &pb.ConfigureResponse{Ready: true}, nil
}

// listen records every event delivered on the subscription.
func (e *echoImpl) listen(eventName string) {
	if eventName == "" {
		return
	}
	stream, err := e.hostClient().Subscribe(context.Background(),
		&pb.SubscribeRequest{EventName: eventName})
	if err != nil {
		log.Printf("echoplugin: subscribe: %v", err)
		return
	}
	for {
		ev, err := stream.Recv()
		if err != nil {
			log.Printf("echoplugin: subscription ended: %v", err)
			return
		}
		e.mu.Lock()
		e.received = append(e.received, string(ev.GetData()))
		e.mu.Unlock()
	}
}

func (e *echoImpl) hostClient() pb.HostServicesClient {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.host
}

func (e *echoImpl) HandleHTTP(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
	e.mu.RLock()
	greeting, instance := e.hostEcho, e.instance
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
	case "/db":
		// Exercises the reverse channel against the real document store: a
		// write and a read back, both crossing the process boundary.
		out, err := e.roundTripDocument(context.Background())
		if err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		body = out
	case "/publish":
		// Broadcast an event other plugins can hear.
		if _, err := e.hostClient().Publish(context.Background(), &pb.PublishRequest{
			EventName: "thing.happened",
			Data:      []byte(req.GetQuery()),
		}); err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		body = []byte("published")

	case "/received":
		// What this process heard on its subscription, newline separated.
		e.mu.RLock()
		body = []byte(strings.Join(e.received, "\n"))
		e.mu.RUnlock()

	case "/cache":
		// Uses a capability that requires a declared permission, so tests can
		// prove Core refuses it on its own side rather than trusting the
		// plugin's SDK to police itself.
		out, err := e.roundTripCache(context.Background())
		if err != nil {
			return &pb.HttpResponse{StatusCode: 403, Body: []byte(err.Error())}, nil
		}
		body = out

	case "/queue":
		out, err := e.roundTripQueue(context.Background())
		if err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		body = out

	// --- deliberately badly-behaved responses, for robustness tests ---

	case "/hang":
		// Never returns until the caller gives up. A plugin that wedges must
		// not wedge Core along with it.
		<-ctx.Done()
		return nil, ctx.Err()

	case "/huge":
		// A response far larger than anything sensible, to check the size
		// ceiling is enforced rather than swallowing the whole thing.
		body = make([]byte, 32<<20)

	case "/badstatus":
		// A status code outside the valid range. Core must not pass it
		// through to net/http, which panics on an invalid code.
		return &pb.HttpResponse{StatusCode: 9999, Body: []byte("bad status")}, nil

	case "/zerostatus":
		// A plugin that forgot to set a status at all.
		return &pb.HttpResponse{Body: []byte("no status set")}, nil

	case "/panic":
		// A handler panic inside the plugin. The plugin process should
		// survive it or die cleanly, but either way Core must stay up.
		panic("deliberate panic inside the plugin handler")

	case "/slow":
		// Slow but not hung: finishes well within any sane timeout.
		time.Sleep(150 * time.Millisecond)
	}

	return &pb.HttpResponse{
		StatusCode: 200,
		Headers: map[string]*pb.HeaderValues{
			"Content-Type": {Values: []string{"text/plain"}},
			"X-Echo-Path":  {Values: []string{req.GetPath()}},
			// Which replica answered, so load-balancing can be observed from
			// outside rather than inferred.
			"X-Instance":    {Values: []string{instance}},
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

// roundTripDocument writes a document and reads it back through HostServices,
// proving the reverse channel reaches the real store rather than a stub.
func (e *echoImpl) roundTripDocument(ctx context.Context) ([]byte, error) {
	e.mu.RLock()
	host := e.host
	e.mu.RUnlock()
	if host == nil {
		return nil, fmt.Errorf("host services not bound")
	}

	payload := []byte(`{"written_by":"echoplugin"}`)
	put, err := host.Put(ctx, &pb.PutRequest{Collection: "notes", DocId: "e2e", Data: payload})
	if err != nil {
		return nil, fmt.Errorf("put: %w", err)
	}

	got, err := host.Get(ctx, &pb.GetRequest{Collection: "notes", DocId: "e2e"})
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	if !got.GetFound() {
		return nil, fmt.Errorf("document was not found after writing it")
	}
	return fmt.Appendf(nil, "version=%d data=%s", put.GetVersion(), got.GetData()), nil
}

// roundTripCache writes and reads a cache entry, which needs the "cache"
// permission. Whether that permission was granted is Core's decision, made on
// Core's side of the connection.
func (e *echoImpl) roundTripCache(ctx context.Context) ([]byte, error) {
	e.mu.RLock()
	host := e.host
	e.mu.RUnlock()
	if host == nil {
		return nil, fmt.Errorf("host services not bound")
	}

	if _, err := host.CacheSet(ctx, &pb.CacheSetRequest{
		Key:   "probe",
		Value: []byte("cached"),
	}); err != nil {
		return nil, fmt.Errorf("cache set: %w", err)
	}
	got, err := host.CacheGet(ctx, &pb.CacheGetRequest{Key: "probe"})
	if err != nil {
		return nil, fmt.Errorf("cache get: %w", err)
	}
	return fmt.Appendf(nil, "found=%v value=%s", got.GetFound(), got.GetValue()), nil
}

// roundTripQueue enqueues a message and consumes it back.
func (e *echoImpl) roundTripQueue(ctx context.Context) ([]byte, error) {
	e.mu.RLock()
	host := e.host
	e.mu.RUnlock()
	if host == nil {
		return nil, fmt.Errorf("host services not bound")
	}

	if _, err := host.Enqueue(ctx, &pb.EnqueueRequest{
		Topic:   "e2e",
		Payload: []byte("queued work"),
	}); err != nil {
		return nil, fmt.Errorf("enqueue: %w", err)
	}

	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	stream, err := host.Consume(cctx, &pb.ConsumeRequest{Topic: "e2e", Prefetch: 1})
	if err != nil {
		return nil, fmt.Errorf("consume: %w", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("receive: %w", err)
	}
	if _, err := host.Ack(ctx, &pb.AckRequest{MessageId: msg.GetMessageId()}); err != nil {
		return nil, fmt.Errorf("ack: %w", err)
	}
	return fmt.Appendf(nil, "attempt=%d payload=%s", msg.GetAttempt(), msg.GetPayload()), nil
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
