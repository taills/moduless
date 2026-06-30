package sdk

import (
	"context"

	pb "github.com/ty-lab/go-web-module/proto/tunnel"
	"google.golang.org/grpc"
)

// EventClient publishes and subscribes to the Core distributed event bus.
type EventClient struct {
	client pb.EventBusServiceClient
}

func NewEventClient(conn *grpc.ClientConn) *EventClient {
	return &EventClient{client: pb.NewEventBusServiceClient(conn)}
}

// Publish emits an event to all subscribers across extensions.
func (c *EventClient) Publish(ctx context.Context, eventName string, data []byte) error {
	_, err := c.client.Publish(ctx, &pb.PublishRequest{
		EventName: eventName,
		EventData: data,
	})
	return err
}

// Subscribe opens a stream for the given event name and invokes handler for
// each delivered event until ctx is cancelled or the stream errors.
func (c *EventClient) Subscribe(ctx context.Context, eventName string, handler func(data []byte)) error {
	stream, err := c.client.Subscribe(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.SubscribeRequest{EventName: eventName}); err != nil {
		return err
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		handler(msg.EventData)
	}
}
