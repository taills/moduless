package sdk

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/taills/moduless/manifest"
	pb "github.com/taills/moduless/proto/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// extKeyMetadataField mirrors the Core interceptor's expected metadata key.
const extKeyMetadataField = "x-extension-key"

// Shared client handles initialised by Start, reusable by DB / Files / Events.
var (
	conn   *grpc.ClientConn
	DB     *DBClient
	Files  *FilesClient
	Events *EventClient
)

// clientStream serializes concurrent Send calls on the gRPC client stream.
type clientStream struct {
	stream pb.ExtensionTunnel_ConnectClient
	mu     sync.Mutex
}

func (c *clientStream) Send(msg *pb.TunnelMessage) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stream.Send(msg)
}

// Start dials Core, registers the extension, and bridges tunnelled HTTP
// requests into the supplied handler. It blocks and reconnects forever.
func Start(handler http.Handler, cfg Config) {
	var err error
	conn, err = grpc.NewClient(cfg.CoreGrpcURL,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(extKeyUnaryInterceptor(cfg.ExtensionKey)),
		grpc.WithStreamInterceptor(extKeyStreamInterceptor(cfg.ExtensionKey)),
	)
	if err != nil {
		log.Fatalf("failed to dial Core gRPC: %v", err)
	}
	defer conn.Close()

	// Initialise auxiliary service clients over the shared connection.
	DB = NewDBClient(conn)
	Files = NewFilesClient(conn)
	Events = NewEventClient(conn)

	client := pb.NewExtensionTunnelClient(conn)
	version := cfg.Version
	if version == "" {
		version = "1.0.0"
	}

	registerReq := &pb.RegisterRequest{
		ExtensionKey:   cfg.ExtensionKey,
		Version:        version,
		IsDev:          cfg.IsDev,
		DevFrontendUrl: cfg.DevFEUrl,
	}
	if cfg.ManifestPath != "" {
		if m, err := manifest.Load(cfg.ManifestPath); err != nil {
			log.Printf("warning: failed to load manifest %s: %v", cfg.ManifestPath, err)
		} else {
			applyManifest(registerReq, m)
		}
	}

	// In production mode the SDK ships the built micro-frontend to Core so the
	// gateway can serve it. The zip is built once and re-streamed on reconnect.
	var frontendZip []byte
	if !cfg.IsDev {
		if cfg.FrontendDir != "" {
			data, sum, err := buildFrontendZip(cfg.FrontendDir)
			if err != nil {
				log.Fatalf("failed to bundle frontend: %v", err)
			}
			frontendZip = data
			registerReq.ZipFileSize = uint64(len(data))
			registerReq.ZipSha256 = sum
		} else {
			// No frontend bundled: register as dev so Core completes immediately
			// instead of waiting for a zip that will never arrive.
			log.Printf("no FrontendDir set with IsDev=false; registering without a micro-frontend")
			registerReq.IsDev = true
		}
	}

	for {
		stream, err := client.Connect(context.Background())
		if err != nil {
			log.Printf("tunnel connect failed: %v, retrying in 2s...", err)
			time.Sleep(2 * time.Second)
			continue
		}

		cs := &clientStream{stream: stream}
		if err := cs.Send(&pb.TunnelMessage{
			Payload: &pb.TunnelMessage_RegisterReq{RegisterReq: registerReq},
		}); err != nil {
			log.Printf("registration send failed: %v", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Stream the bundled micro-frontend, then signal completion so Core
		// extracts it and replies with the registration result.
		if len(frontendZip) > 0 {
			if err := uploadFrontendZip(cs, frontendZip); err != nil {
				log.Printf("frontend upload failed: %v, reconnecting in 2s...", err)
				time.Sleep(2 * time.Second)
				continue
			}
		}

		if err := handleTunnel(cs, handler); err != nil {
			log.Printf("tunnel connection lost: %v. Reconnecting in 2s...", err)
			time.Sleep(2 * time.Second)
		}
	}
}

// applyManifest copies manifest collection/slot declarations into the
// registration request.
func applyManifest(req *pb.RegisterRequest, m *manifest.Manifest) {
	if m.Weight > 0 {
		req.Weight = int32(m.Weight)
	}
	for _, c := range m.Database.Collections {
		col := &pb.CollectionSchema{Name: c.Name}
		for _, idx := range c.Indexes {
			col.Indexes = append(col.Indexes, &pb.IndexSchema{
				Fields: idx.Fields,
				Unique: idx.Unique,
			})
		}
		req.Collections = append(req.Collections, col)
	}
	for _, s := range m.UISlots {
		req.Slots = append(req.Slots, &pb.SlotSchema{
			SlotName:       s.SlotName,
			ComponentEntry: s.ComponentEntry,
		})
	}
	if m.Menu.Icon != "" {
		req.MenuIcon = m.Menu.Icon
	}
	if m.Menu.Path != "" {
		req.MenuPath = m.Menu.Path
	}
	if m.DisplayName != "" {
		req.DisplayName = m.DisplayName
	}
}

func handleTunnel(cs *clientStream, handler http.Handler) error {
	for {
		msg, err := cs.stream.Recv()
		if err != nil {
			return err
		}

		switch payload := msg.Payload.(type) {
		case *pb.TunnelMessage_RegisterResp:
			if !payload.RegisterResp.Success {
				log.Printf("registration rejected: %s", payload.RegisterResp.ErrorMessage)
				return io.EOF
			}
			log.Printf("registration success")

		case *pb.TunnelMessage_HttpReqChunk:
			go serveBridgedRequest(cs, payload.HttpReqChunk, handler)

		case *pb.TunnelMessage_Pong:
			// Heartbeat acknowledged.
		}
	}
}

func serveBridgedRequest(cs *clientStream, reqChunk *pb.HttpRequestChunk, handler http.Handler) {
	target := reqChunk.Path
	if reqChunk.Query != "" {
		target += "?" + reqChunk.Query
	}
	req, err := http.NewRequest(reqChunk.Method, target, bytes.NewReader(reqChunk.BodyChunk))
	if err != nil {
		log.Printf("failed to reconstruct request: %v", err)
		return
	}
	for k, v := range reqChunk.Headers {
		req.Header.Set(k, v)
	}

	// Parse user headers and inject context.
	if userID := req.Header.Get("X-User-Id"); userID != "" {
		userCtx := &UserContext{
			UserID:      userID,
			Roles:       splitNonEmpty(req.Header.Get("X-User-Roles")),
			Permissions: splitNonEmpty(req.Header.Get("X-User-Permissions")),
		}
		req = req.WithContext(context.WithValue(req.Context(), userContextKey, userCtx))
	}

	w := newMockResponseWriter()
	handler.ServeHTTP(w, req)

	headers := make(map[string]string)
	for k, v := range w.Header() {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	_ = cs.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_HttpRespChunk{
			HttpRespChunk: &pb.HttpResponseChunk{
				StreamId:   reqChunk.StreamId,
				IsFirst:    true,
				IsLast:     true,
				StatusCode: int32(w.statusCode),
				Headers:    headers,
				BodyChunk:  w.body.Bytes(),
			},
		},
	})
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// extKeyUnaryInterceptor attaches the extension identity to outgoing unary RPCs.
func extKeyUnaryInterceptor(extKey string) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply interface{}, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx, extKeyMetadataField, extKey)
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// extKeyStreamInterceptor attaches the extension identity to outgoing streams.
func extKeyStreamInterceptor(extKey string) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx, extKeyMetadataField, extKey)
		return streamer(ctx, desc, cc, method, opts...)
	}
}
