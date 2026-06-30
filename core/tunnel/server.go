package tunnel

import (
	"errors"
	"io"
	"log"
	"time"

	pb "github.com/taills/moduless/proto/tunnel"
)

// TunnelServer implements the ExtensionTunnel gRPC service.
type TunnelServer struct {
	pb.UnimplementedExtensionTunnelServer
	Manager *TunnelManager

	// OnRegister, when set, runs at registration time so Core can provision
	// CMDS schema and register UI slots from the manifest declarations carried
	// in the RegisterRequest. A returned error rejects the registration.
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
			if key != "" {
				s.gracefulUnregister(key, currentTunnel)
			}
			return err
		}

		switch payload := msg.Payload.(type) {
		case *pb.TunnelMessage_RegisterReq:
			key = payload.RegisterReq.ExtensionKey
			currentTunnel = s.Manager.Register(key, stream, payload.RegisterReq)
			log.Printf("[tunnel] extension registered: %s (dev=%v)", key, payload.RegisterReq.IsDev)

			if s.OnRegister != nil {
				if err := s.OnRegister(payload.RegisterReq); err != nil {
					log.Printf("[tunnel] OnRegister failed for %s: %v", key, err)
					if sendErr := currentTunnel.Send(&pb.TunnelMessage{
						Payload: &pb.TunnelMessage_RegisterResp{
							RegisterResp: &pb.RegisterResponse{Success: false, ErrorMessage: err.Error()},
						},
					}); sendErr != nil {
						return sendErr
					}
					s.Manager.RemoveTunnel(key, currentTunnel)
					return err
				}
			}

			if payload.RegisterReq.IsDev {
				// Dev mode skips zip upload; respond success immediately.
				if err := currentTunnel.Send(&pb.TunnelMessage{
					Payload: &pb.TunnelMessage_RegisterResp{
						RegisterResp: &pb.RegisterResponse{Success: true, SkipUpload: true},
					},
				}); err != nil {
					return err
				}
			}

		case *pb.TunnelMessage_FileChunk:
			if key == "" {
				return errors.New("file chunk sent before registration")
			}
			s.Manager.SaveZipChunk(currentTunnel, payload.FileChunk.Content)

		case *pb.TunnelMessage_RegisterComplete:
			if key == "" {
				return errors.New("complete sent before registration")
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
			}
			if currentTunnel != nil {
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

	if key != "" {
		s.gracefulUnregister(key, currentTunnel)
	}
	return nil
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
