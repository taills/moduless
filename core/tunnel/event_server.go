package tunnel

import (
	"github.com/ty-lab/go-web-module/core/event"
	pb "github.com/ty-lab/go-web-module/proto/tunnel"
	"context"
)

// EventServer implements the EventBusService gRPC API.
type EventServer struct {
	pb.UnimplementedEventBusServiceServer
	Bus *event.EventBus
}

func NewEventServer(bus *event.EventBus) *EventServer {
	return &EventServer{Bus: bus}
}

// Publish broadcasts an event to all subscribers.
func (s *EventServer) Publish(ctx context.Context, req *pb.PublishRequest) (*pb.PublishResponse, error) {
	s.Bus.Publish(req.EventName, req.EventData)
	return &pb.PublishResponse{Success: true}, nil
}

// Subscribe reads subscription requests from the client and forwards matching
// events back. A single buffered channel multiplexes every subscription; each
// Event carries its own name so the client receives correctly-tagged messages.
func (s *EventServer) Subscribe(stream pb.EventBusService_SubscribeServer) error {
	ch := make(chan event.Event, 100)
	subscribed := make(map[string]struct{})
	defer func() {
		for ev := range subscribed {
			s.Bus.Unsubscribe(ev, ch)
		}
	}()

	// Single forwarder goroutine drains the shared channel to the stream.
	errCh := make(chan error, 1)
	go func() {
		for e := range ch {
			if err := stream.Send(&pb.EventMessage{
				EventName: e.Name,
				EventData: e.Data,
			}); err != nil {
				errCh <- err
				return
			}
		}
	}()

	for {
		// Stop if the forwarder reported a send error.
		select {
		case err := <-errCh:
			return err
		default:
		}

		req, err := stream.Recv()
		if err != nil {
			return err
		}
		if _, ok := subscribed[req.EventName]; !ok {
			s.Bus.Subscribe(req.EventName, ch)
			subscribed[req.EventName] = struct{}{}
		}
	}
}
