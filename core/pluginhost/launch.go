// Package pluginhost owns the lifecycle of plugin subprocesses: launching
// them, watching them, and swapping them atomically under live traffic.
package pluginhost

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/taills/moduless/pluginapi"
	pb "github.com/taills/moduless/proto/plugin"
)

// LaunchSpec is everything needed to start one plugin process.
type LaunchSpec struct {
	Key        string
	InstanceID string

	// BinaryPath is the executable inside the verified, content-addressed
	// plugin directory.
	BinaryPath string

	// Checksum is the expected SHA-256 of BinaryPath. When set, go-plugin
	// re-hashes the file immediately before exec, so the bytes that run are
	// the bytes that were verified at install time. Leaving it empty disables
	// that check and is only appropriate in tests.
	Checksum []byte

	// HostImpl serves this instance's HostServices. It must already have the
	// plugin key and granted permissions closed over.
	HostImpl pb.HostServicesServer

	Config             map[string]string
	GrantedPermissions []string
	DataDir            string
	LogLevel           string

	// MaxMessageBytes overrides pluginapi.DefaultMaxMessageBytes.
	MaxMessageBytes int

	// Env is the complete environment for the child process. It is not merged
	// with Core's own environment: SkipHostEnv is set, because go-plugin
	// otherwise forwards everything Core has — including DATABASE_URL,
	// ADMIN_PASSWORD and the object-store credentials — to what may be a
	// third-party binary.
	Env []string

	// Stdout and Stderr capture the child's streams. Note that go-plugin
	// consumes the first line of stdout for its handshake; everything after
	// that is forwarded here.
	Stdout io.Writer
	Stderr io.Writer

	// ConfigureTimeout bounds the initial Configure call.
	ConfigureTimeout time.Duration

	// DevMode relaxes isolation for local development. It currently skips
	// Pdeathsig, because air restarts Core on every rebuild and killing every
	// plugin along with it makes the edit loop painful. Never set it in
	// production: without Pdeathsig a crashed Core leaves orphaned plugins.
	DevMode bool
}

// Instance is a live plugin process plus the client used to drive it.
type Instance struct {
	Key        string
	InstanceID string

	Client   *pluginapi.Client
	goClient *goplugin.Client
}

// Launch starts the plugin process, completes the handshake, dispenses the
// client, and performs the initial Configure handshake that also establishes
// the plugin's reverse connection to HostServices.
//
// On any failure the child process is killed before returning, so a partially
// started plugin never leaks.
func Launch(ctx context.Context, spec LaunchSpec) (inst *Instance, err error) {
	if spec.HostImpl == nil {
		return nil, fmt.Errorf("pluginhost: LaunchSpec.HostImpl is required for %s", spec.Key)
	}
	maxMsg := spec.MaxMessageBytes
	if maxMsg <= 0 {
		maxMsg = pluginapi.DefaultMaxMessageBytes
	}

	cmd := exec.Command(spec.BinaryPath)
	cmd.Env = spec.Env
	applySandbox(cmd, spec) // no-op outside Linux; see sandbox_*.go

	cfg := &goplugin.ClientConfig{
		HandshakeConfig: pluginapi.Handshake,
		Plugins: goplugin.PluginSet{
			pluginapi.DispenseName: &pluginapi.GRPCPlugin{
				HostImpl:        spec.HostImpl,
				MaxMessageBytes: maxMsg,
			},
		},
		Cmd:              cmd,
		AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
		AutoMTLS:         true,
		// Core builds the child environment explicitly; never inherit.
		SkipHostEnv: true,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "plugin." + spec.Key,
			Level:  hclogLevel(spec.LogLevel),
			Output: spec.Stderr,
		}),
		SyncStdout: spec.Stdout,
		SyncStderr: spec.Stderr,
		GRPCDialOptions: []grpc.DialOption{
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(maxMsg),
				grpc.MaxCallSendMsgSize(maxMsg),
			),
		},
	}
	if len(spec.Checksum) > 0 {
		cfg.SecureConfig = &goplugin.SecureConfig{
			Checksum: spec.Checksum,
			Hash:     sha256.New(),
		}
	}

	client := goplugin.NewClient(cfg)
	defer func() {
		if err != nil {
			client.Kill()
		}
	}()

	rpcClient, err := client.Client()
	if err != nil {
		return nil, fmt.Errorf("plugin %s: handshake failed: %w", spec.Key, err)
	}
	raw, err := rpcClient.Dispense(pluginapi.DispenseName)
	if err != nil {
		return nil, fmt.Errorf("plugin %s: dispense failed: %w", spec.Key, err)
	}
	pc, ok := raw.(*pluginapi.Client)
	if !ok {
		return nil, fmt.Errorf("plugin %s: unexpected dispensed type %T", spec.Key, raw)
	}

	timeout := spec.ConfigureTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := pc.Configure(cctx, &pb.ConfigureRequest{
		PluginKey:          spec.Key,
		InstanceId:         spec.InstanceID,
		Config:             spec.Config,
		GrantedPermissions: spec.GrantedPermissions,
		DataDir:            spec.DataDir,
		LogLevel:           spec.LogLevel,
	})
	if err != nil {
		return nil, fmt.Errorf("plugin %s: configure failed: %w", spec.Key, err)
	}
	if !resp.GetReady() {
		return nil, fmt.Errorf("plugin %s: not ready: %s", spec.Key, resp.GetError())
	}

	return &Instance{
		Key:        spec.Key,
		InstanceID: spec.InstanceID,
		Client:     pc,
		goClient:   client,
	}, nil
}

// Kill terminates the plugin process. It is safe to call more than once.
func (i *Instance) Kill() {
	if i != nil && i.goClient != nil {
		i.goClient.Kill()
	}
}

// Exited reports whether the plugin process has terminated.
//
// go-plugin offers only this boolean poll — there is no exit notification
// channel — which is why the supervisor pairs it with a gRPC health watch.
func (i *Instance) Exited() bool {
	return i == nil || i.goClient == nil || i.goClient.Exited()
}

func hclogLevel(s string) hclog.Level {
	switch s {
	case "debug":
		return hclog.Debug
	case "info":
		return hclog.Info
	case "error":
		return hclog.Error
	default:
		return hclog.Warn
	}
}
