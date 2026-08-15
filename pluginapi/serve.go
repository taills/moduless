package pluginapi

import (
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// ServeConfig configures a plugin binary's main().
type ServeConfig struct {
	// Impl is the plugin's implementation. Required.
	Impl PluginImpl

	// HostBinder receives the reverse connection to Core during Configure.
	// sdk/go supplies this to wire up its host clients.
	HostBinder HostBinder

	// MaxMessageBytes overrides DefaultMaxMessageBytes when positive. Core
	// also sends its own ceiling in ConfigureRequest.
	MaxMessageBytes int
}

// Serve runs the plugin. It blocks until Core kills the process.
//
// It must be the last call in main(), and nothing may write to stdout before
// or during it: go-plugin performs its handshake by reading a single line from
// the child's stdout, so a stray fmt.Println corrupts the handshake and the
// plugin fails to start with a confusing error. Use stderr for diagnostics —
// the standard library's log package already defaults there, and Core captures
// both streams.
func Serve(cfg ServeConfig) {
	if cfg.Impl == nil {
		panic("pluginapi: Serve requires a non-nil Impl")
	}
	maxMsg := cfg.MaxMessageBytes
	if maxMsg <= 0 {
		maxMsg = DefaultMaxMessageBytes
	}

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins: plugin.PluginSet{
			DispenseName: &GRPCPlugin{
				Impl:            cfg.Impl,
				HostBinder:      cfg.HostBinder,
				MaxMessageBytes: maxMsg,
			},
		},
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			opts = append(opts,
				grpc.MaxRecvMsgSize(maxMsg),
				grpc.MaxSendMsgSize(maxMsg),
			)
			// Do not register grpc.health.v1.Health here: go-plugin already
			// registers it on this server and gRPC panics on duplicate
			// service registration. Core watches that built-in health stream
			// to detect a dead or wedged plugin promptly, since go-plugin's
			// Client.Exited() is only a boolean poll with no notification.
			return grpc.NewServer(opts...)
		},
	})
}
