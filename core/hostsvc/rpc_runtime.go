package hostsvc

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/taills/moduless/proto/plugin"
)

// Every method here follows the same shape: check the permission, check the
// capability is configured, then delegate. The permission check comes first so
// an ungranted plugin learns it lacks a declaration rather than that Core is
// missing a subsystem.

// --- Cache ------------------------------------------------------------------

func (s *Server) CacheGet(_ context.Context, req *pb.CacheGetRequest) (*pb.CacheGetResponse, error) {
	if err := s.require(PermCache); err != nil {
		return nil, err
	}
	if s.deps.Cache == nil {
		return nil, s.unavailable("the cache")
	}
	value, found := s.deps.Cache.Get(s.key, req.GetKey())
	return &pb.CacheGetResponse{Found: found, Value: value}, nil
}

func (s *Server) CacheSet(_ context.Context, req *pb.CacheSetRequest) (*emptypb.Empty, error) {
	if err := s.require(PermCache); err != nil {
		return nil, err
	}
	if s.deps.Cache == nil {
		return nil, s.unavailable("the cache")
	}
	s.deps.Cache.Set(s.key, req.GetKey(), req.GetValue(), time.Duration(req.GetTtlSeconds())*time.Second)
	return &emptypb.Empty{}, nil
}

func (s *Server) CacheDelete(_ context.Context, req *pb.CacheDeleteRequest) (*emptypb.Empty, error) {
	if err := s.require(PermCache); err != nil {
		return nil, err
	}
	if s.deps.Cache == nil {
		return nil, s.unavailable("the cache")
	}
	s.deps.Cache.Delete(s.key, req.GetKey())
	return &emptypb.Empty{}, nil
}

// --- Locks ------------------------------------------------------------------

func (s *Server) AcquireLock(ctx context.Context, req *pb.AcquireLockRequest) (*pb.AcquireLockResponse, error) {
	if err := s.require(PermLock); err != nil {
		return nil, err
	}
	if s.deps.Locks == nil {
		return nil, s.unavailable("locking")
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "lock name is required")
	}

	lease, ok, err := s.deps.Locks.Acquire(ctx, s.key, req.GetName(),
		time.Duration(req.GetTtlSeconds())*time.Second,
		time.Duration(req.GetWaitSeconds())*time.Second)
	if err != nil {
		return nil, status.Errorf(codes.Aborted, "acquire lock: %v", err)
	}
	if !ok {
		return &pb.AcquireLockResponse{Acquired: false}, nil
	}
	return &pb.AcquireLockResponse{
		Acquired:      true,
		LeaseId:       lease.ID,
		ExpiresAtUnix: lease.ExpiresAt.Unix(),
	}, nil
}

func (s *Server) RenewLock(_ context.Context, req *pb.LeaseRequest) (*pb.AcquireLockResponse, error) {
	if err := s.require(PermLock); err != nil {
		return nil, err
	}
	if s.deps.Locks == nil {
		return nil, s.unavailable("locking")
	}
	lease, ok := s.deps.Locks.Renew(s.key, req.GetName(), req.GetLeaseId(),
		time.Duration(req.GetTtlSeconds())*time.Second)
	if !ok {
		// The caller no longer owns the lock. Reporting this rather than
		// silently re-acquiring is important: its work may already have been
		// taken over by whoever holds the lease now.
		return &pb.AcquireLockResponse{Acquired: false}, nil
	}
	return &pb.AcquireLockResponse{
		Acquired:      true,
		LeaseId:       lease.ID,
		ExpiresAtUnix: lease.ExpiresAt.Unix(),
	}, nil
}

func (s *Server) ReleaseLock(_ context.Context, req *pb.LeaseRequest) (*emptypb.Empty, error) {
	if err := s.require(PermLock); err != nil {
		return nil, err
	}
	if s.deps.Locks == nil {
		return nil, s.unavailable("locking")
	}
	s.deps.Locks.Release(s.key, req.GetName(), req.GetLeaseId())
	return &emptypb.Empty{}, nil
}

// --- Config -----------------------------------------------------------------

// GetConfig needs no permission: configuration is the plugin's own settings,
// supplied by the admin who installed it.
func (s *Server) GetConfig(ctx context.Context, _ *emptypb.Empty) (*pb.GetConfigResponse, error) {
	if s.deps.Config == nil {
		return &pb.GetConfigResponse{}, nil
	}
	cfg, err := s.deps.Config.Get(ctx, s.key)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read config: %v", err)
	}
	return &pb.GetConfigResponse{Config: cfg}, nil
}

// --- Events -----------------------------------------------------------------

func (s *Server) Publish(ctx context.Context, req *pb.PublishRequest) (*emptypb.Empty, error) {
	if err := s.require(PermEvents); err != nil {
		return nil, err
	}
	if s.deps.Events == nil {
		return nil, s.unavailable("the event bus")
	}
	if req.GetEventName() == "" {
		return nil, status.Error(codes.InvalidArgument, "event name is required")
	}

	err := s.deps.Events.Publish(s.key, Event{
		Name:            req.GetEventName(),
		Data:            req.GetData(),
		SourcePluginKey: s.key,
		TraceID:         traceFrom(ctx, req.GetTraceId()),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) Subscribe(req *pb.SubscribeRequest, stream pb.HostServices_SubscribeServer) error {
	if err := s.require(PermEvents); err != nil {
		return err
	}
	if s.deps.Events == nil {
		return s.unavailable("the event bus")
	}
	if req.GetEventName() == "" {
		return status.Error(codes.InvalidArgument, "event name is required")
	}

	return s.deps.Events.Subscribe(stream.Context(), s.key, req.GetEventName(), func(ev Event) error {
		return stream.Send(&pb.Event{
			EventName:       ev.Name,
			Data:            ev.Data,
			SourcePluginKey: ev.SourcePluginKey,
			TraceId:         ev.TraceID,
		})
	})
}

// --- Observability ----------------------------------------------------------

// Log accepts a stream of structured records. It needs no permission: a plugin
// being able to describe what it is doing is not a capability worth gating,
// and refusing it would only make failures harder to diagnose.
func (s *Server) Log(stream pb.HostServices_LogServer) error {
	if s.deps.Obs == nil {
		// Drain rather than reject, so a plugin logging into a Core without an
		// observability sink is not disrupted by it.
		for {
			if _, err := stream.Recv(); err != nil {
				return stream.SendAndClose(&emptypb.Empty{})
			}
		}
	}

	for {
		rec, err := stream.Recv()
		if err != nil {
			return stream.SendAndClose(&emptypb.Empty{})
		}
		ts := time.Unix(0, rec.GetTimestampUnixNanos())
		if rec.GetTimestampUnixNanos() == 0 {
			ts = time.Now()
		}
		s.deps.Obs.Log(s.key, LogRecord{
			Level:     logLevelName(rec.GetLevel()),
			Message:   rec.GetMessage(),
			Fields:    rec.GetFields(),
			TraceID:   rec.GetTraceId(),
			Timestamp: ts,
		})
	}
}

func (s *Server) RecordMetric(_ context.Context, req *pb.MetricRequest) (*emptypb.Empty, error) {
	if s.deps.Obs == nil {
		return &emptypb.Empty{}, nil
	}
	s.deps.Obs.RecordMetric(s.key, Metric{
		Name:   req.GetName(),
		Kind:   metricKindName(req.GetKind()),
		Value:  req.GetValue(),
		Labels: req.GetLabels(),
	})
	return &emptypb.Empty{}, nil
}

func logLevelName(l pb.LogLevel) string {
	switch l {
	case pb.LogLevel_LOG_DEBUG:
		return "debug"
	case pb.LogLevel_LOG_WARN:
		return "warn"
	case pb.LogLevel_LOG_ERROR:
		return "error"
	default:
		return "info"
	}
}

func metricKindName(k pb.MetricKind) string {
	switch k {
	case pb.MetricKind_METRIC_GAUGE:
		return "gauge"
	case pb.MetricKind_METRIC_HISTOGRAM:
		return "histogram"
	default:
		return "counter"
	}
}

// --- Outbound HTTP ----------------------------------------------------------

func (s *Server) Fetch(ctx context.Context, req *pb.FetchRequest) (*pb.FetchResponse, error) {
	if err := s.require(PermHTTPEgress); err != nil {
		return nil, err
	}
	if s.deps.Egress == nil {
		return nil, s.unavailable("outbound HTTP")
	}

	headers := make(map[string][]string, len(req.GetHeaders()))
	for k, hv := range req.GetHeaders() {
		headers[k] = hv.GetValues()
	}

	resp, err := s.deps.Egress.Fetch(ctx, s.key, EgressRequest{
		Method:  req.GetMethod(),
		URL:     req.GetUrl(),
		Headers: headers,
		Body:    req.GetBody(),
		Timeout: time.Duration(req.GetTimeoutMs()) * time.Millisecond,
		TraceID: traceFrom(ctx, req.GetTraceId()),
	})
	if err != nil {
		return nil, egressErr(err)
	}

	out := make(map[string]*pb.HeaderValues, len(resp.Headers))
	for k, vs := range resp.Headers {
		out[k] = &pb.HeaderValues{Values: vs}
	}
	return &pb.FetchResponse{
		StatusCode: int32(resp.StatusCode),
		Headers:    out,
		Body:       resp.Body,
	}, nil
}

// egressErr classifies an outbound failure into a gRPC code.
//
// Fetch was the one capability that returned its backend error raw, so
// grpc-go wrapped everything in codes.Unknown and a plugin could not tell a
// permanent refusal from a rate limit from a remote server that was down.
// Every other capability classifies; this brings the last one into line.
func egressErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrEgressNotAllowed):
		// Permanent: the manifest has to change and be approved again.
		return status.Errorf(codes.PermissionDenied, "%v", err)
	case errors.Is(err, ErrEgressRateLimited):
		return status.Errorf(codes.ResourceExhausted, "%v", err)
	case errors.Is(err, ErrEgressBadRequest):
		return status.Errorf(codes.InvalidArgument, "%v", err)
	case errors.Is(err, context.DeadlineExceeded):
		return status.Errorf(codes.DeadlineExceeded, "%v", err)
	default:
		// The remote host: unreachable, refused, TLS failure. Retryable in a
		// way the three above are not.
		return status.Errorf(codes.Unavailable, "%v", err)
	}
}
