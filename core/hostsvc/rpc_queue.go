package hostsvc

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/taills/moduless/proto/plugin"
)

func (s *Server) Enqueue(ctx context.Context, req *pb.EnqueueRequest) (*pb.EnqueueResponse, error) {
	if err := s.require(PermQueue); err != nil {
		return nil, err
	}
	if s.deps.Queue == nil {
		return nil, s.unavailable("the durable queue")
	}
	if req.GetTopic() == "" {
		return nil, status.Error(codes.InvalidArgument, "topic is required")
	}

	id, deduplicated, err := s.deps.Queue.Enqueue(ctx, s.key, req.GetTopic(), req.GetPayload(), EnqueueOptions{
		Delay:       time.Duration(req.GetDelaySeconds()) * time.Second,
		DedupKey:    req.GetDedupKey(),
		Priority:    int(req.GetPriority()),
		MaxAttempts: int(req.GetMaxAttempts()),
		TraceID:     traceFrom(ctx, req.GetTraceId()),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "enqueue: %v", err)
	}
	return &pb.EnqueueResponse{MessageId: id, Deduplicated: deduplicated}, nil
}

// Consume streams messages until the plugin disconnects or Core shuts down.
//
// Delivery is at-least-once. A message is handed over as soon as it is
// claimed, and acknowledgement is a separate call, so a plugin that dies
// mid-handler lets the visibility timeout lapse and the message comes back
// rather than disappearing with the connection. Handlers must be idempotent.
func (s *Server) Consume(req *pb.ConsumeRequest, stream pb.HostServices_ConsumeServer) error {
	if err := s.require(PermQueue); err != nil {
		return err
	}
	if s.deps.Queue == nil {
		return s.unavailable("the durable queue")
	}
	if req.GetTopic() == "" {
		return status.Error(codes.InvalidArgument, "topic is required")
	}

	return s.deps.Queue.Consume(
		stream.Context(),
		s.key,
		req.GetTopic(),
		int(req.GetPrefetch()),
		time.Duration(req.GetVisibilityTimeoutSeconds())*time.Second,
		func(m Message) error {
			return stream.Send(&pb.QueueMessage{
				MessageId:     m.ID,
				Topic:         m.Topic,
				Payload:       m.Payload,
				Attempt:       int32(m.Attempt),
				MaxAttempts:   int32(m.MaxAttempts),
				ParentTraceId: m.ParentTraceID,
				// A fresh id for this delivery attempt, linked to the request
				// that enqueued the work through ParentTraceId.
				TraceId: newDeliveryTraceID(m.ID, m.Attempt),
			})
		},
	)
}

func (s *Server) Ack(ctx context.Context, req *pb.AckRequest) (*emptypb.Empty, error) {
	if err := s.require(PermQueue); err != nil {
		return nil, err
	}
	if s.deps.Queue == nil {
		return nil, s.unavailable("the durable queue")
	}
	if err := s.deps.Queue.Ack(ctx, s.key, req.GetMessageId()); err != nil {
		return nil, status.Errorf(codes.Internal, "ack: %v", err)
	}
	return &emptypb.Empty{}, nil
}

func (s *Server) Nack(ctx context.Context, req *pb.NackRequest) (*emptypb.Empty, error) {
	if err := s.require(PermQueue); err != nil {
		return nil, err
	}
	if s.deps.Queue == nil {
		return nil, s.unavailable("the durable queue")
	}
	if err := s.deps.Queue.Nack(ctx, s.key, req.GetMessageId(), req.GetError(),
		time.Duration(req.GetRetryAfterSeconds())*time.Second); err != nil {
		return nil, status.Errorf(codes.Internal, "nack: %v", err)
	}
	return &emptypb.Empty{}, nil
}

// newDeliveryTraceID derives a stable id for one delivery attempt, so the same
// attempt correlates across the plugin's logs and Core's without needing a
// random value the plugin cannot reproduce.
func newDeliveryTraceID(messageID int64, attempt int) string {
	return "q-" + strconv.FormatInt(messageID, 10) + "-" + strconv.Itoa(attempt)
}
