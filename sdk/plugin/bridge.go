package sdk

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"

	pb "github.com/taills/moduless/proto/plugin"
)

// plugin implements pluginapi.PluginImpl on top of the author's Config.
type plugin struct {
	cfg Config

	mu       sync.RWMutex
	key      string
	config   map[string]string
	dataDir  string
	granted  []string
	instance string
}

func newPlugin(cfg Config) *plugin {
	return &plugin{cfg: cfg, config: map[string]string{}}
}

func (p *plugin) Configure(_ context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	p.mu.Lock()
	p.key = req.GetPluginKey()
	p.instance = req.GetInstanceId()
	p.dataDir = req.GetDataDir()
	p.granted = req.GetGrantedPermissions()
	p.config = req.GetConfig()
	p.mu.Unlock()
	return &pb.ConfigureResponse{Ready: true}, nil
}

// Key is the plugin's identifier, as declared in its manifest.
func Key() string { return current.get().key }

// DataDir is a plugin-private writable directory. Everything else in the
// filesystem should be treated as read-only.
func DataDir() string { return current.get().dataDir }

// Config returns the admin-managed settings for this plugin.
func GetConfig() map[string]string {
	p := current.get()
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[string]string, len(p.config))
	for k, v := range p.config {
		out[k] = v
	}
	return out
}

// current holds the running plugin so the package-level helpers can reach it.
// A process serves exactly one plugin, so a single value is correct here.
var current instanceHolder

type instanceHolder struct {
	mu sync.RWMutex
	p  *plugin
}

func (h *instanceHolder) set(p *plugin) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.p = p
}

func (h *instanceHolder) get() *plugin {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.p == nil {
		return &plugin{config: map[string]string{}}
	}
	return h.p
}

// HandleHTTP turns Core's request into a standard *http.Request, runs the
// author's handler, and turns the response back.
//
// Reconstructing a real http.Request rather than inventing an interface is
// what lets a plugin use any router and any middleware from the ecosystem
// unchanged.
func (p *plugin) HandleHTTP(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
	if p.cfg.Handler == nil {
		return &pb.HttpResponse{StatusCode: http.StatusNotImplemented,
			Body: []byte("this plugin serves no HTTP API")}, nil
	}

	target := req.GetPath()
	if q := req.GetQuery(); q != "" {
		target += "?" + q
	}
	u, err := url.ParseRequestURI(target)
	if err != nil {
		return &pb.HttpResponse{StatusCode: http.StatusBadRequest,
			Body: []byte("bad request path")}, nil
	}

	body := io.NopCloser(bytes.NewReader(req.GetBody()))
	httpReq := (&http.Request{
		Method:        methodOr(req.GetMethod()),
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        headersFrom(req.GetHeaders()),
		Body:          body,
		ContentLength: int64(len(req.GetBody())),
		RemoteAddr:    req.GetClientIp(),
		RequestURI:    target,
		Host:          hostFrom(req.GetHeaders()),
	}).WithContext(withRequestValues(ctx, req))

	rec := &responseRecorder{header: http.Header{}, status: http.StatusOK}
	p.cfg.Handler.ServeHTTP(rec, httpReq)

	return &pb.HttpResponse{
		StatusCode: int32(rec.status),
		Headers:    headersTo(rec.header),
		Body:       rec.body.Bytes(),
	}, nil
}

func methodOr(m string) string {
	if m == "" {
		return http.MethodGet
	}
	return m
}

func hostFrom(h map[string]*pb.HeaderValues) string {
	if hv, ok := h["Host"]; ok && len(hv.GetValues()) > 0 {
		return hv.GetValues()[0]
	}
	return "plugin.local"
}

func headersFrom(in map[string]*pb.HeaderValues) http.Header {
	out := make(http.Header, len(in))
	for k, hv := range in {
		out[http.CanonicalHeaderKey(k)] = append([]string(nil), hv.GetValues()...)
	}
	return out
}

func headersTo(h http.Header) map[string]*pb.HeaderValues {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]*pb.HeaderValues, len(h))
	for k, vs := range h {
		out[k] = &pb.HeaderValues{Values: vs}
	}
	return out
}

// responseRecorder captures what the handler wrote.
type responseRecorder struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(p)
}

// Flush exists so handlers that stream do not panic. The response is buffered
// and returned whole, so flushing has no effect beyond that — streaming
// responses through a plugin are not supported yet, and silently accepting the
// call is better than crashing a handler that merely tries.
func (r *responseRecorder) Flush() {}

// Filter dispatches to the author's handler for that phase.
func (p *plugin) Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error) {
	fn, ok := p.cfg.Filters[Phase(req.GetPhase()-1)]
	if !ok || fn == nil {
		// Subscribed in the manifest but unhandled here. Continuing is right:
		// an error would count against the circuit breaker and eventually take
		// the plugin out of service over a request it did not care about.
		return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}, nil
	}

	result, err := fn(withFilterValues(ctx, req), filterRequestFrom(req))
	if err != nil {
		return nil, err
	}
	if result == nil {
		return &pb.FilterResponse{Action: pb.FilterResponse_ACTION_CONTINUE}, nil
	}
	return result.toProto(), nil
}

func (p *plugin) RunJob(ctx context.Context, req *pb.JobRequest) (*pb.JobResponse, error) {
	fn, ok := p.cfg.Jobs[req.GetJobName()]
	if !ok || fn == nil {
		return &pb.JobResponse{Success: false,
			Error: "no handler registered for job " + req.GetJobName()}, nil
	}

	job := &Job{
		Name:      req.GetJobName(),
		TraceID:   req.GetTraceId(),
		Scheduled: req.GetScheduledUnix(),
	}
	if err := fn(withTrace(ctx, req.GetTraceId()), job); err != nil {
		return &pb.JobResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.JobResponse{Success: true}, nil
}

func (p *plugin) OnConfigChanged(_ context.Context, req *pb.ConfigChangeEvent) error {
	p.mu.Lock()
	p.config = req.GetConfig()
	p.mu.Unlock()

	if p.cfg.OnConfigChanged != nil {
		p.cfg.OnConfigChanged(req.GetConfig())
	}
	return nil
}

func (p *plugin) Shutdown(ctx context.Context, _ *pb.ShutdownRequest) error {
	if p.cfg.OnShutdown != nil {
		return p.cfg.OnShutdown(ctx)
	}
	return nil
}

// Job describes one scheduled occurrence.
type Job struct {
	Name    string
	TraceID string
	// Scheduled is the occurrence this run is for, as a Unix timestamp. It may
	// lag wall-clock time if Core was busy, so work that must be attributed to
	// a specific window should key off this rather than time.Now().
	Scheduled int64
}
