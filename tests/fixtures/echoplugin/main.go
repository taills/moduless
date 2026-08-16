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
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"strconv"
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
	// What Configure was handed, as opposed to what asking for it returned.
	// A plugin reads its configuration both ways and they must agree; keeping
	// them apart here is what lets a test notice when they do not.
	launchEcho string

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
	e.launchEcho = req.GetConfig()["greeting"]
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
	greeting, launched, instance := e.hostEcho, e.launchEcho, e.instance
	e.mu.RUnlock()

	body := req.GetBody()
	switch req.GetPath() {
	case "/large":
		// A response of a size the caller chooses, so a test can drive the
		// message-size ceiling from both sides of it. gRPC's own default is
		// 4 MiB and this framework raises it to 16 MiB on both ends; nothing
		// had ever sent anything big enough to find out whether the raise
		// actually took effect.
		n, err := strconv.Atoi(req.GetQuery())
		if err != nil || n < 0 {
			return &pb.HttpResponse{StatusCode: 400, Body: []byte("bad size")}, nil
		}
		body = bytes.Repeat([]byte("x"), n)
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
	case "/tx-commit":
		// A transaction spanning several RPCs: begin, write, commit. Each of
		// these is a separate call, which is the whole point — the transaction
		// must outlive the call that opened it.
		out, err := e.transact(context.Background())
		if err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		body = out

	case "/tx-then-crash":
		// Opens a transaction, writes inside it, and dies without committing.
		// Core has to notice and roll back: a database connection pinned by a
		// process that no longer exists is never coming back on its own.
		host := e.hostClient()
		tx, err := host.BeginTx(context.Background(), &pb.BeginTxRequest{TimeoutSeconds: 2})
		if err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		if _, err := host.Put(context.Background(), &pb.PutRequest{
			Collection: "notes",
			DocId:      "uncommitted",
			Data:       []byte(`{"written":"inside an abandoned transaction"}`),
			TxId:       tx.GetTxId(),
		}); err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		log.Print("echoplugin: dying with a transaction open")
		os.Exit(4)

	case "/file-shortlived":
		// Same as /file but with a download link that expires almost at once,
		// so a test can watch it stop working.
		out, err := e.roundTripFileExpiry(context.Background(), req.GetQuery(), 1)
		if err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		body = out

	case "/file-delete":
		// Deletes a file id this plugin was given, which may belong to
		// somebody else.
		if _, err := e.hostClient().DeleteFile(context.Background(),
			&pb.FileRequest{FileId: req.GetQuery()}); err != nil {
			return &pb.HttpResponse{StatusCode: 403, Body: []byte(err.Error())}, nil
		}
		body = []byte("deleted")

	case "/file-meta":
		// Asks what Core knows about a file id, ditto.
		meta, err := e.hostClient().GetFileMetadata(context.Background(),
			&pb.FileRequest{FileId: req.GetQuery()})
		if err != nil {
			return &pb.HttpResponse{StatusCode: 403, Body: []byte(err.Error())}, nil
		}
		body = fmt.Appendf(nil, "found=%v filename=%s size=%d",
			meta.GetFound(), meta.GetFilename(), meta.GetSize())

	case "/file-token-for":
		// Asks for a download link to a file id this plugin was given, which
		// may belong to somebody else.
		host := e.hostClient()
		token, err := host.GenerateDownloadToken(context.Background(), &pb.DownloadTokenRequest{
			FileId: req.GetQuery(), UserId: "1", ExpirySeconds: 300,
		})
		if err != nil {
			return &pb.HttpResponse{StatusCode: 403, Body: []byte(err.Error())}, nil
		}
		body = []byte("url=" + token.GetUrl())

	case "/file":
		// Writes a file through Core and asks for a download link. The bytes
		// go out through the plugin transport in chunks; nothing comes back
		// through it, which is the asymmetry worth exercising.
		out, err := e.roundTripFile(context.Background(), req.GetQuery())
		if err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		body = out

	case "/fetch":
		// Outbound HTTP through Core's egress proxy. The query string is the
		// target URL, so a test can aim it wherever it likes and see what Core
		// makes of it.
		out, err := e.hostClient().Fetch(context.Background(), &pb.FetchRequest{
			Method: "GET",
			Url:    req.GetQuery(),
		})
		if err != nil {
			return &pb.HttpResponse{StatusCode: 403, Body: []byte(err.Error())}, nil
		}
		return &pb.HttpResponse{
			StatusCode: 200,
			Body:       fmt.Appendf(nil, "upstream=%d body=%s", out.GetStatusCode(), out.GetBody()),
		}, nil

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

	case "/queue-consume-crash":
		// Consumes one message and dies without acknowledging it, the way a
		// plugin killed mid-job would. The message must come back.
		out, err := e.consumeThenCrash(context.Background())
		if err != nil {
			return &pb.HttpResponse{StatusCode: 500, Body: []byte(err.Error())}, nil
		}
		return &pb.HttpResponse{StatusCode: 200, Body: out}, nil

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

	if strings.HasSuffix(req.GetPath(), "/filter-names") {
		return &pb.HttpResponse{
			StatusCode: 200,
			Body:       []byte(strings.Join(namesFor(req.GetQuery()), " ")),
		}, nil
	}

	if strings.HasSuffix(req.GetPath(), "/phases") {
		return &pb.HttpResponse{
			StatusCode: 200,
			Headers: map[string]*pb.HeaderValues{
				"Content-Type": {Values: []string{"text/plain"}},
			},
			Body: []byte(strings.Join(phasesFor(req.GetQuery()), " ")),
		}, nil
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
			// Who Core says is calling. Echoed so a test can check that an
			// identity another plugin established in the authenticate phase
			// actually reaches this one's handler — which is the whole claim
			// of a shared request pipeline, and was not covered until it
			// turned out not to work at the gateway.
			"X-Caller":        {Values: []string{req.GetIdentity().GetUsername()}},
			"X-Caller-Roles":  {Values: []string{strings.Join(req.GetIdentity().GetRoles(), ",")}},
			"X-Launch-Config": {Values: []string{launched}},
			// Two values on one header, to prove repeated headers survive the
			// round trip. The legacy tunnel dropped all but the first.
			"X-Multi": {Values: []string{"a", "b"}},
		},
		Body: body,
	}, nil
}

// phaseLog records every phase this plugin was called in, per trace.
//
// A request's lifecycle is a sequence of claims — pre_route then authenticate
// then authorize, log runs for every request including the ones an earlier
// filter refused — and each of those was only ever checked one phase at a
// time, with fakes. Recording the real sequence is what lets a test assert the
// order rather than the parts.
var phaseLog struct {
	sync.Mutex
	seen map[string][]string
}

func recordPhase(traceID string, phase pb.Phase) {
	if traceID == "" {
		return
	}
	phaseLog.Lock()
	defer phaseLog.Unlock()
	if phaseLog.seen == nil {
		phaseLog.seen = map[string][]string{}
	}
	phaseLog.seen[traceID] = append(phaseLog.seen[traceID], phase.String())
}

// filterNames records which manifest declaration matched, per trace. A plugin
// may declare several filters in one phase, and they arrive at one function.
var filterNames struct {
	sync.Mutex
	seen map[string][]string
}

func recordFilterName(traceID, name string) {
	if traceID == "" || name == "" {
		return
	}
	filterNames.Lock()
	defer filterNames.Unlock()
	if filterNames.seen == nil {
		filterNames.seen = map[string][]string{}
	}
	filterNames.seen[traceID] = append(filterNames.seen[traceID], name)
}

func namesFor(traceID string) []string {
	filterNames.Lock()
	defer filterNames.Unlock()
	return filterNames.seen[traceID]
}

func phasesFor(traceID string) []string {
	phaseLog.Lock()
	defer phaseLog.Unlock()
	return phaseLog.seen[traceID]
}

func (e *echoImpl) Filter(_ context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error) {
	recordPhase(req.GetTraceId(), req.GetPhase())
	recordFilterName(req.GetTraceId(), req.GetFilterName())

	// ECHO_FILTER_DELAY makes every filter call slow, which is how a plugin
	// degrades in practice — not by dying, but by taking longer than anyone
	// budgeted. A filter subscribed to /** is on the critical path of every
	// request in the system, including requests belonging to other plugins.
	if d := os.Getenv("ECHO_FILTER_DELAY"); d != "" {
		if wait, err := time.ParseDuration(d); err == nil {
			time.Sleep(wait)
		}
	}

	// Matched by suffix: a filter sees whatever path the phase gives it, which
	// is the full request path in pre_route and the plugin-relative one later.
	// Matching on the whole string made a test silently exercise nothing.
	if strings.HasSuffix(req.GetPath(), "/slow-filter") {
		// Slow enough that something else can happen while the request is
		// still inside the filter.
		time.Sleep(200 * time.Millisecond)
		return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}, nil
	}

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

func (e *echoImpl) RunJob(ctx context.Context, req *pb.JobRequest) (*pb.JobResponse, error) {
	// ECHO_SLOW_JOB models the thing the notes example warns about: a job that
	// takes far longer than a drain is willing to wait. Whether the drain
	// waits for it or cuts it off is the question, and the README asserted an
	// answer nobody had checked.
	if d := os.Getenv("ECHO_SLOW_JOB"); d != "" {
		wait, err := time.ParseDuration(d)
		if err == nil {
			select {
			case <-time.After(wait):
				log.Printf("echoplugin: slow job %s finished", req.GetJobName())
			case <-ctx.Done():
				log.Printf("echoplugin: slow job %s cut off: %v", req.GetJobName(), ctx.Err())
				return nil, ctx.Err()
			}
		}
	}
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

// roundTripFile uploads content and returns "id=<id> size=<n> url=<url>".
func (e *echoImpl) roundTripFile(ctx context.Context, content string) ([]byte, error) {
	return e.roundTripFileExpiry(ctx, content, 300)
}

func (e *echoImpl) roundTripFileExpiry(ctx context.Context, content string, expiry int32) ([]byte, error) {
	host := e.hostClient()

	stream, err := host.PutFile(ctx)
	if err != nil {
		return nil, fmt.Errorf("open upload: %w", err)
	}
	// First chunk carries the metadata, the rest carry only bytes — split
	// deliberately so the streaming path is exercised rather than a single
	// message that happens to fit.
	if err := stream.Send(&pb.PutFileChunk{
		Filename: "report.txt",
		MimeType: "text/plain",
		Data:     []byte(content),
	}); err != nil {
		return nil, fmt.Errorf("send metadata chunk: %w", err)
	}
	if err := stream.Send(&pb.PutFileChunk{Data: []byte("\ntrailing chunk")}); err != nil {
		return nil, fmt.Errorf("send data chunk: %w", err)
	}
	put, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("finish upload: %w", err)
	}

	token, err := host.GenerateDownloadToken(ctx, &pb.DownloadTokenRequest{
		FileId:        put.GetFileId(),
		UserId:        "1",
		ExpirySeconds: expiry,
	})
	if err != nil {
		return nil, fmt.Errorf("download token: %w", err)
	}
	return fmt.Appendf(nil, "id=%s size=%d url=%s",
		put.GetFileId(), put.GetSize(), token.GetUrl()), nil
}

// transact writes two documents inside one transaction and commits it.
func (e *echoImpl) transact(ctx context.Context) ([]byte, error) {
	host := e.hostClient()

	tx, err := host.BeginTx(ctx, &pb.BeginTxRequest{TimeoutSeconds: 30})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	for _, id := range []string{"tx-one", "tx-two"} {
		if _, err := host.Put(ctx, &pb.PutRequest{
			Collection: "notes",
			DocId:      id,
			Data:       []byte(`{"in":"transaction"}`),
			TxId:       tx.GetTxId(),
		}); err != nil {
			return nil, fmt.Errorf("put %s: %w", id, err)
		}
	}
	if _, err := host.CommitTx(ctx, &pb.TxRequest{TxId: tx.GetTxId()}); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return []byte("committed"), nil
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

// consumeThenCrash receives one message and exits without acking it.
func (e *echoImpl) consumeThenCrash(ctx context.Context) ([]byte, error) {
	host := e.hostClient()

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	stream, err := host.Consume(cctx, &pb.ConsumeRequest{
		Topic:                    "crashtest",
		Prefetch:                 1,
		VisibilityTimeoutSeconds: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("consume: %w", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		return nil, fmt.Errorf("receive: %w", err)
	}

	log.Printf("echoplugin: received message %d attempt %d, dying without ack",
		msg.GetMessageId(), msg.GetAttempt())
	// Die the way an OOM kill or a panic would: no ack, no shutdown, nothing.
	os.Exit(5)
	return nil, nil
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
	e.launchEcho = req.GetConfig()["greeting"]
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

	// ECHO_STDOUT_BEFORE writes to stdout before the handshake, which is the
	// mistake the plugin guide calls its first rule: go-plugin reads the
	// handshake from the child's first stdout line, so a stray fmt.Println
	// during start-up replaces it. Modelled here so a test can see what Core
	// reports, which is the part that decides whether an author can find it.
	if os.Getenv("ECHO_STDOUT_BEFORE") == "1" {
		fmt.Println("debugging: about to start")
	}

	impl := &echoImpl{}
	// ECHO_STDOUT_AFTER writes once the handshake is done. Whether that is
	// also fatal is a separate question from the one above, and the guide
	// states the rule without distinguishing them.
	if os.Getenv("ECHO_STDOUT_AFTER") == "1" {
		go func() {
			time.Sleep(200 * time.Millisecond)
			fmt.Println("chatty plugin says hello")
		}()
	}

	pluginapi.Serve(pluginapi.ServeConfig{
		Impl:       impl,
		HostBinder: impl.bindHost,
	})
}

type failingImpl struct{ echoImpl }

func (f *failingImpl) Configure(context.Context, *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	return &pb.ConfigureResponse{Ready: false, Error: "deliberate configure failure"}, nil
}
