package tunnel

import (
	"context"
	"errors"
	"io"
	"log"
	"time"

	pb "github.com/taills/moduless/proto/tunnel"
)

// AuthAction is the decision the extension registry returns for an incoming
// registration request.
type AuthAction int

const (
	// AuthApprove: the request carried a valid secret for an approved key; the
	// tunnel should be routed immediately.
	AuthApprove AuthAction = iota
	// AuthPending: a first-time or not-yet-approved extension; hold the tunnel
	// open and park it for admin review.
	AuthPending
	// AuthReject: the key was rejected by an admin; inform the SDK and disconnect.
	AuthReject
	// AuthDeny: a bad/missing secret on a known key; disconnect without parking.
	AuthDeny
)

// AuthResult is the outcome of authenticating a registration request.
type AuthResult struct {
	Action  AuthAction
	Message string
}

// Authenticator decides how Core should treat a registration request. It is
// implemented by the extension registry and is nil in the database-less demo
// mode, where registration stays open.
type Authenticator interface {
	Authenticate(ctx context.Context, req *pb.RegisterRequest) (AuthResult, error)
}

// TunnelServer implements the ExtensionTunnel gRPC service.
type TunnelServer struct {
	pb.UnimplementedExtensionTunnelServer
	Manager *TunnelManager

	// Auth, when set, gates registration through the extension approval workflow.
	// When nil (database-less demo / tests), registration is open and every
	// extension is accepted immediately.
	Auth Authenticator

	// OnRegister, when set, runs at activation time so Core can provision CMDS
	// schema and register UI slots from the manifest declarations carried in the
	// RegisterRequest. A returned error rejects the registration.
	OnRegister func(req *pb.RegisterRequest) error
	// OnUnregister, when set, runs after an extension is removed.
	OnUnregister func(extKey string)
}

func NewTunnelServer(m *TunnelManager) *TunnelServer {
	return &TunnelServer{Manager: m}
}

// Connect handles the long-lived bidirectional stream from an extension.
func (s *TunnelServer) Connect(stream pb.ExtensionTunnel_ConnectServer) error {
	var currentTunnel *ActiveTunnel
	var key string

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.cleanup(key, currentTunnel)
			return err
		}

		switch payload := msg.Payload.(type) {
		case *pb.TunnelMessage_RegisterReq:
			req := payload.RegisterReq
			key = req.ExtensionKey
			t, err := s.handleRegister(stream, req)
			if err != nil {
				return err
			}
			if t == nil {
				// Rejected/denied: a decision/response was already sent; close.
				return nil
			}
			currentTunnel = t

		case *pb.TunnelMessage_FileChunk:
			if key == "" {
				return errors.New("file chunk sent before registration")
			}
			s.Manager.SaveZipChunk(currentTunnel, payload.FileChunk.Content)

		case *pb.TunnelMessage_RegisterComplete:
			if key == "" {
				return errors.New("complete sent before registration")
			}
			// Only an approved (routable) tunnel may activate its frontend. A
			// pending tunnel never reaches here because the SDK uploads only
			// after approval.
			if !currentTunnel.Approved.Load() {
				continue
			}
			err := s.Manager.ExtractZipCache(currentTunnel)
			var resp *pb.RegisterResponse
			if err != nil {
				resp = &pb.RegisterResponse{Success: false, ErrorMessage: err.Error()}
			} else {
				resp = &pb.RegisterResponse{Success: true}
			}
			if err := currentTunnel.Send(&pb.TunnelMessage{
				Payload: &pb.TunnelMessage_RegisterResp{RegisterResp: resp},
			}); err != nil {
				return err
			}

		case *pb.TunnelMessage_HttpRespChunk:
			if currentTunnel != nil {
				ch, ok := currentTunnel.ResponseChans.Load(payload.HttpRespChunk.StreamId)
				if ok {
					ch.(chan *pb.HttpResponseChunk) <- payload.HttpRespChunk
				}
			}

		case *pb.TunnelMessage_Ping:
			if currentTunnel != nil {
				s.Manager.Touch(currentTunnel)
				if err := currentTunnel.Send(&pb.TunnelMessage{
					Payload: &pb.TunnelMessage_Pong{
						Pong: &pb.Pong{Timestamp: time.Now().UnixNano()},
					},
				}); err != nil {
					return err
				}
			}
		}
	}

	s.cleanup(key, currentTunnel)
	return nil
}

// handleRegister applies the approval workflow to an incoming RegisterRequest.
// It returns the tunnel to track for the rest of the connection, or nil when the
// registration was rejected/denied and the stream should close.
func (s *TunnelServer) handleRegister(stream pb.ExtensionTunnel_ConnectServer, req *pb.RegisterRequest) (*ActiveTunnel, error) {
	key := req.ExtensionKey

	// Open mode (no registry): accept everything, preserving the demo behavior.
	if s.Auth == nil {
		t := s.Manager.Register(key, stream, req)
		t.Approved.Store(true)
		log.Printf("[tunnel] extension registered (open mode): %s (dev=%v)", key, req.IsDev)
		if err := s.activate(t, req); err != nil {
			s.Manager.RemoveTunnel(key, t)
			return nil, err
		}
		return t, nil
	}

	res, err := s.Auth.Authenticate(stream.Context(), req)
	if err != nil {
		log.Printf("[tunnel] authenticate %s failed: %v", key, err)
		_ = sendDecision(stream, &pb.RegisterDecision{Status: "rejected", IssuedSecret: ""})
		return nil, nil
	}

	switch res.Action {
	case AuthApprove:
		t := s.Manager.Register(key, stream, req)
		t.Approved.Store(true)
		log.Printf("[tunnel] extension reconnected with valid secret: %s", key)
		if err := s.activate(t, req); err != nil {
			s.Manager.RemoveTunnel(key, t)
			return nil, err
		}
		return t, nil

	case AuthPending:
		t := s.Manager.AddPending(key, stream, req)
		log.Printf("[tunnel] extension awaiting approval: %s", key)
		if err := sendDecision(stream, &pb.RegisterDecision{Status: "pending"}); err != nil {
			s.Manager.RemovePending(key, t)
			return nil, err
		}
		return t, nil

	case AuthReject:
		log.Printf("[tunnel] registration rejected for %s: %s", key, res.Message)
		_ = sendDecision(stream, &pb.RegisterDecision{Status: "rejected"})
		return nil, nil

	default: // AuthDeny
		log.Printf("[tunnel] registration denied for %s: %s", key, res.Message)
		_ = sendResponse(stream, &pb.RegisterResponse{Success: false, ErrorMessage: res.Message})
		return nil, nil
	}
}

// activate provisions schema/slots for a routable tunnel and acknowledges the
// registration: dev (or frontend-less) extensions complete immediately, while
// production extensions are asked to upload their micro-frontend bundle.
func (s *TunnelServer) activate(t *ActiveTunnel, req *pb.RegisterRequest) error {
	if s.OnRegister != nil {
		if err := s.OnRegister(req); err != nil {
			log.Printf("[tunnel] OnRegister failed for %s: %v", req.ExtensionKey, err)
			_ = t.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterResp{
				RegisterResp: &pb.RegisterResponse{Success: false, ErrorMessage: err.Error()},
			}})
			return err
		}
	}
	if req.IsDev || req.ZipFileSize == 0 {
		return t.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterResp{
			RegisterResp: &pb.RegisterResponse{Success: true, SkipUpload: true},
		}})
	}
	return t.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterDecision{
		RegisterDecision: &pb.RegisterDecision{Status: "approved", UploadFrontend: true},
	}})
}

func sendDecision(stream pb.ExtensionTunnel_ConnectServer, d *pb.RegisterDecision) error {
	return stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterDecision{RegisterDecision: d}})
}

func sendResponse(stream pb.ExtensionTunnel_ConnectServer, r *pb.RegisterResponse) error {
	return stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterResp{RegisterResp: r}})
}

// cleanup drops a tunnel on disconnect: pending tunnels are removed immediately,
// routable tunnels go through the graceful unload window.
func (s *TunnelServer) cleanup(key string, t *ActiveTunnel) {
	if key == "" || t == nil {
		return
	}
	if s.Manager.IsPending(key, t) {
		s.Manager.RemovePending(key, t)
		return
	}
	s.gracefulUnregister(key, t)
}

// gracefulUnregister applies a 10s graceful unload buffer: if the replica
// reconnects within the window, it survives; otherwise it is removed. Key-scoped
// state (UI slots) is dropped only when the last replica for the key is gone.
func (s *TunnelServer) gracefulUnregister(key string, t *ActiveTunnel) {
	go func() {
		time.Sleep(10 * time.Second)
		if !s.Manager.HasTunnel(key, t) {
			return
		}
		lastGone := s.Manager.RemoveTunnel(key, t)
		log.Printf("[tunnel] replica unregistered after grace period: %s (%s)", key, t.InstanceID)
		if lastGone && s.OnUnregister != nil {
			s.OnUnregister(key)
		}
	}()
}
