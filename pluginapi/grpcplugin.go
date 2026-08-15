package pluginapi

import (
	"context"
	"fmt"
	"sync"

	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/taills/moduless/proto/plugin"
)

// HostBinder is called inside the plugin process once the reverse connection
// back to Core is established. sdk/go uses it to wire up its DB/Files/Queue
// clients. It is invoked exactly once, during Configure.
type HostBinder func(conn *grpc.ClientConn)

// GRPCPlugin implements plugin.GRPCPlugin for both ends of the connection.
// The same type is used on both sides, with different fields populated:
//
//	Core side:   HostImpl (+ MaxMessageBytes)
//	Plugin side: Impl, HostBinder (+ MaxMessageBytes)
type GRPCPlugin struct {
	// NetRPCUnsupportedPlugin refuses the legacy net/rpc protocol, forcing
	// gRPC. Without it a protocol downgrade would silently produce a plugin
	// that fails at first call instead of at handshake.
	plugin.NetRPCUnsupportedPlugin

	// Impl is the plugin-side implementation (plugin process only).
	Impl PluginImpl

	// HostBinder receives the reverse connection (plugin process only).
	HostBinder HostBinder

	// HostImpl serves this plugin's HostServices (Core process only).
	//
	// Core constructs one HostImpl per plugin instance, with the plugin key
	// and its granted permissions already closed over. That is what makes the
	// plugin's identity structural rather than self-asserted: there is no key
	// field on the wire for a plugin to forge.
	HostImpl pb.HostServicesServer

	// MaxMessageBytes overrides DefaultMaxMessageBytes when positive.
	MaxMessageBytes int
}

func (p *GRPCPlugin) maxMsg() int {
	if p.MaxMessageBytes > 0 {
		return p.MaxMessageBytes
	}
	return DefaultMaxMessageBytes
}

// GRPCServer runs inside the plugin process: it exposes PluginImpl so Core can
// drive it.
func (p *GRPCPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	if p.Impl == nil {
		return fmt.Errorf("pluginapi: GRPCPlugin.Impl is nil in the plugin process")
	}
	pb.RegisterPluginServiceServer(s, &pluginServer{
		impl:   p.Impl,
		broker: broker,
		binder: p.HostBinder,
		maxMsg: p.maxMsg(),
	})
	return nil
}

// GRPCClient runs inside Core. Besides wrapping the plugin stub it stands up
// this instance's HostServices on a brokered stream and remembers the stream
// id, which is handed to the plugin in Configure so it can dial back.
func (p *GRPCPlugin) GRPCClient(_ context.Context, broker *plugin.GRPCBroker, conn *grpc.ClientConn) (any, error) {
	if p.HostImpl == nil {
		return nil, fmt.Errorf("pluginapi: GRPCPlugin.HostImpl is nil in Core")
	}
	maxMsg := p.maxMsg()
	hostBrokerID := broker.NextId()

	// AcceptAndServe blocks until the plugin knocks, so it must run in its own
	// goroutine: the plugin cannot knock until it receives the id, and it only
	// receives the id once Configure is called on the client we return here.
	go broker.AcceptAndServe(hostBrokerID, func(opts []grpc.ServerOption) *grpc.Server {
		opts = append(opts,
			grpc.MaxRecvMsgSize(maxMsg),
			grpc.MaxSendMsgSize(maxMsg),
		)
		srv := grpc.NewServer(opts...)
		pb.RegisterHostServicesServer(srv, p.HostImpl)
		return srv
	})

	return &Client{
		stub:         pb.NewPluginServiceClient(conn),
		hostBrokerID: hostBrokerID,
		maxMsg:       maxMsg,
	}, nil
}

// pluginServer adapts the generated server interface onto PluginImpl inside
// the plugin process.
type pluginServer struct {
	pb.UnimplementedPluginServiceServer

	impl   PluginImpl
	broker *plugin.GRPCBroker
	binder HostBinder
	maxMsg int

	dialOnce sync.Once
	dialErr  error
}

// Configure dials back to Core before delegating, so that by the time the
// plugin's own Configure runs its host clients are already usable.
func (s *pluginServer) Configure(ctx context.Context, req *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	s.dialOnce.Do(func() {
		if s.binder == nil {
			return
		}
		conn, err := s.broker.DialWithOptions(
			req.GetHostBrokerId(),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(s.maxMsg),
				grpc.MaxCallSendMsgSize(s.maxMsg),
			),
		)
		if err != nil {
			s.dialErr = fmt.Errorf("dial host services (broker id %d): %w", req.GetHostBrokerId(), err)
			return
		}
		s.binder(conn)
	})
	if s.dialErr != nil {
		return &pb.ConfigureResponse{Ready: false, Error: s.dialErr.Error()}, s.dialErr
	}
	return s.impl.Configure(ctx, req)
}

func (s *pluginServer) HandleHTTP(ctx context.Context, req *pb.HttpRequest) (*pb.HttpResponse, error) {
	return s.impl.HandleHTTP(ctx, req)
}

func (s *pluginServer) Filter(ctx context.Context, req *pb.FilterRequest) (*pb.FilterResponse, error) {
	return s.impl.Filter(ctx, req)
}

func (s *pluginServer) RunJob(ctx context.Context, req *pb.JobRequest) (*pb.JobResponse, error) {
	return s.impl.RunJob(ctx, req)
}

func (s *pluginServer) OnConfigChanged(ctx context.Context, req *pb.ConfigChangeEvent) (*emptypb.Empty, error) {
	if err := s.impl.OnConfigChanged(ctx, req); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (s *pluginServer) Shutdown(ctx context.Context, req *pb.ShutdownRequest) (*emptypb.Empty, error) {
	if err := s.impl.Shutdown(ctx, req); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}
