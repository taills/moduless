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

	visibility := time.Duration(req.GetVisibilityTimeoutSeconds()) * time.Second

	return s.deps.Queue.Consume(
		stream.Context(),
		s.key,
		req.GetTopic(),
		int(req.GetPrefetch()),
		visibility,
		func(ctx context.Context) error {
			return s.awaitRoom(ctx, int(req.GetPrefetch()), visibility)
		},
		func(m Message) error {
			if err := stream.Send(&pb.QueueMessage{
				MessageId:     m.ID,
				Topic:         m.Topic,
				Payload:       m.Payload,
				Attempt:       int32(m.Attempt),
				MaxAttempts:   int32(m.MaxAttempts),
				ParentTraceId: m.ParentTraceID,
				// A fresh id for this delivery attempt, linked to the request
				// that enqueued the work through ParentTraceId.
				TraceId: newDeliveryTraceID(m.ID, m.Attempt),
			}); err != nil {
				return err
			}
			s.enteredFlight()
			return nil
		},
	)
}

// awaitRoom blocks while this consumer already holds its prefetch in
// unacknowledged messages. Called before each claim.
//
// It waits without reserving anything: the count rises when a message is
// actually delivered, so a claim that finds an empty queue costs nothing.
//
// Bounded by the visibility timeout rather than waiting forever. A plugin that
// dies holding a message never acknowledges it, and the message becomes
// claimable again when its visibility lapses; blocking past that point would
// leave this consumer idle while its work is redelivered to somebody else.
func (s *Server) awaitRoom(ctx context.Context, prefetch int, visibility time.Duration) error {
	if prefetch <= 0 {
		prefetch = 1
	}
	if visibility <= 0 {
		visibility = 30 * time.Second
	}
	deadline := time.Now().Add(visibility)

	for {
		s.inflightMu.Lock()
		n := s.inflight
		s.inflightMu.Unlock()
		if n < prefetch || time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// enteredFlight records a message handed to the plugin.
func (s *Server) enteredFlight() {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.inflight++
}

// leftFlight records one acknowledged, however it ended.
func (s *Server) leftFlight() {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if s.inflight > 0 {
		s.inflight--
	}
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
	s.leftFlight()
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
	s.leftFlight()
	return &emptypb.Empty{}, nil
}

// newDeliveryTraceID derives a stable id for one delivery attempt, so the same
// attempt correlates across the plugin's logs and Core's without needing a
// random value the plugin cannot reproduce.
func newDeliveryTraceID(messageID int64, attempt int) string {
	return "q-" + strconv.FormatInt(messageID, 10) + "-" + strconv.Itoa(attempt)
}
