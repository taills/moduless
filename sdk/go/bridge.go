package sdk

import (
	"bytes"
	"context"
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
			// The approval secret persisted from a previous run lets Core skip the
			// approval workflow and route this instance immediately. An explicit
			// Config/env value takes precedence over the manifest copy.
			if cfg.ExtensionSecret == "" {
				cfg.ExtensionSecret = m.Secret
			}
		}
	}
	registerReq.ExtensionSecret = cfg.ExtensionSecret

	// In production mode the SDK ships the built micro-frontend to Core so the
	// gateway can serve it. The zip is built once and uploaded only after Core
	// approves the extension (signalled by a RegisterDecision).
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

		sess := &tunnelSession{cs: cs, handler: handler, cfg: cfg, frontendZip: frontendZip}
		backoff := handleTunnel(sess)
		// Adopt any secret Core issued during this session so the next reconnect
		// authenticates immediately.
		if sess.issuedSecret != "" {
			registerReq.ExtensionSecret = sess.issuedSecret
		}
		log.Printf("tunnel connection ended. Reconnecting in %s...", backoff)
		time.Sleep(backoff)
	}
}

// tunnelSession carries the per-connection state the message loop needs to drive
// the approval workflow (secret persistence, deferred frontend upload).
type tunnelSession struct {
	cs           *clientStream
	handler      http.Handler
	cfg          Config
	frontendZip  []byte
	issuedSecret string
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

// handleTunnel runs the per-connection message loop and returns the backoff to
// wait before reconnecting. It drives the approval workflow: a pending decision
// is awaited, an approval persists the issued secret and (in production) uploads
// the frontend, and a rejection backs off harder to avoid hammering Core.
func handleTunnel(sess *tunnelSession) time.Duration {
	const (
		normalBackoff   = 2 * time.Second
		rejectedBackoff = 30 * time.Second
	)
	for {
		msg, err := sess.cs.stream.Recv()
		if err != nil {
			return normalBackoff
		}

		switch payload := msg.Payload.(type) {
		case *pb.TunnelMessage_RegisterDecision:
			d := payload.RegisterDecision
			switch d.Status {
			case "pending":
				log.Printf("registration pending administrator approval...")
			case "approved":
				sess.onApproved(d)
			case "rejected":
				log.Printf("registration rejected by administrator; backing off")
				return rejectedBackoff
			}

		case *pb.TunnelMessage_RegisterResp:
			if !payload.RegisterResp.Success {
				log.Printf("registration failed: %s", payload.RegisterResp.ErrorMessage)
				return normalBackoff
			}
			log.Printf("registration success")

		case *pb.TunnelMessage_HttpReqChunk:
			go serveBridgedRequest(sess.cs, payload.HttpReqChunk, sess.handler)

		case *pb.TunnelMessage_Pong:
			// Heartbeat acknowledged.
		}
	}
}

// onApproved persists the issued secret and, when Core asks for it, uploads the
// bundled micro-frontend.
func (sess *tunnelSession) onApproved(d *pb.RegisterDecision) {
	if d.IssuedSecret != "" {
		sess.issuedSecret = d.IssuedSecret
		if sess.cfg.ManifestPath != "" {
			if err := manifest.SaveSecret(sess.cfg.ManifestPath, d.IssuedSecret); err != nil {
				log.Printf("warning: failed to persist issued secret to %s: %v", sess.cfg.ManifestPath, err)
			} else {
				log.Printf("approved; persisted issued secret to %s", sess.cfg.ManifestPath)
			}
		} else {
			log.Printf("approved; no ManifestPath set, secret held in memory only (re-approval needed after restart)")
		}
	}
	if d.UploadFrontend && len(sess.frontendZip) > 0 {
		if err := uploadFrontendZip(sess.cs, sess.frontendZip); err != nil {
			log.Printf("frontend upload failed: %v", err)
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
