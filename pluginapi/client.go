package pluginapi

import (
	"context"

	pb "github.com/taills/moduless/proto/plugin"
	"google.golang.org/grpc"
)

// Client is Core's handle on one running plugin instance. It is what
// Dispense returns, and what core/pluginhost calls.
//
// Every method is a plain unary gRPC call over the plugin's multiplexed
// connection, so concurrent calls are safe and independent — unlike the legacy
// tunnel, which serialised sends behind a mutex and hand-rolled stream ids on
// a single bidirectional stream.
type Client struct {
	stub         pb.PluginServiceClient
	hostBrokerID uint32
	maxMsg       int
}

// HostBrokerID exposes the brokered stream id serving this instance's
// HostServices, for diagnostics.
func (c *Client) HostBrokerID() uint32 { return c.hostBrokerID }

// Configure fills in the broker id and negotiated message ceiling so callers
// never have to know they exist.
func (c *Client) Configure(ctx context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	req.HostBrokerId = c.hostBrokerID
	req.MaxMessageBytes = int32(c.maxMsg)
	return c.stub.Configure(ctx, req, c.callOpts()...)
}

func (c *Client) HandleHTTP(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
	return c.stub.HandleHTTP(ctx, req, c.callOpts()...)
}

func (c *Client) Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error) {
	return c.stub.Filter(ctx, req, c.callOpts()...)
}

func (c *Client) RunJob(ctx context.Context, req *pb.JobRequest) (*pb.JobResponse, error) {
	return c.stub.RunJob(ctx, req, c.callOpts()...)
}

func (c *Client) OnConfigChanged(ctx context.Context, req *pb.ConfigChangeEvent) error {
	_, err := c.stub.OnConfigChanged(ctx, req, c.callOpts()...)
	return err
}

func (c *Client) Shutdown(ctx context.Context, req *pb.ShutdownRequest) error {
	_, err := c.stub.Shutdown(ctx, req, c.callOpts()...)
	return err
}

func (c *Client) callOpts() []grpc.CallOption {
	return []grpc.CallOption{
		grpc.MaxCallRecvMsgSize(c.maxMsg),
		grpc.MaxCallSendMsgSize(c.maxMsg),
	}
}
