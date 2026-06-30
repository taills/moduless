package tunnel

import (
	"archive/zip"
	"bytes"
	"context"
	"net"
	"testing"

	pb "github.com/taills/moduless/proto/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeAuth is a stub Authenticator that returns a fixed result, letting the
// approval branches be exercised without a database.
type fakeAuth struct {
	result AuthResult
	err    error
}

func (f fakeAuth) Authenticate(ctx context.Context, req *pb.RegisterRequest) (AuthResult, error) {
	return f.result, f.err
}

// startServer spins up an in-process TunnelServer (optionally with an
// Authenticator) and returns a connected client plus the manager.
func startServer(t *testing.T, auth Authenticator) (pb.ExtensionTunnelClient, *TunnelManager) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	mgr := NewTunnelManager()
	ts := NewTunnelServer(mgr)
	ts.Auth = auth
	srv := grpc.NewServer()
	pb.RegisterExtensionTunnelServer(srv, ts)
	go srv.Serve(lis)
	t.Cleanup(func() { srv.Stop(); lis.Close() })

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("did not connect: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewExtensionTunnelClient(conn), mgr
}

func dummyZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("index.html")
	f.Write([]byte("hello world"))
	zw.Close()
	return buf.Bytes()
}

// TestRegisterAndUpload exercises the open-mode (no Auth) production flow: Core
// approves immediately, asks for the frontend via a RegisterDecision, then
// acknowledges after the upload.
func TestRegisterAndUpload(t *testing.T) {
	client, mgr := startServer(t, nil)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	zipBytes := dummyZip(t)
	if err := stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterReq{
		RegisterReq: &pb.RegisterRequest{
			ExtensionKey: "test-ext",
			Version:      "1.0.0",
			IsDev:        false,
			ZipFileSize:  uint64(len(zipBytes)),
		},
	}}); err != nil {
		t.Fatalf("Send Register failed: %v", err)
	}

	// Core asks for the frontend upload via a decision.
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv decision failed: %v", err)
	}
	dec, ok := msg.Payload.(*pb.TunnelMessage_RegisterDecision)
	if !ok || dec.RegisterDecision.Status != "approved" || !dec.RegisterDecision.UploadFrontend {
		t.Fatalf("expected approved+upload decision, got: %v", msg.Payload)
	}

	if err := stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_FileChunk{
		FileChunk: &pb.FileChunk{Content: zipBytes},
	}}); err != nil {
		t.Fatalf("Send Chunk failed: %v", err)
	}
	if err := stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterComplete{
		RegisterComplete: &pb.RegisterComplete{},
	}}); err != nil {
		t.Fatalf("Send Complete failed: %v", err)
	}

	msg, err = stream.Recv()
	if err != nil {
		t.Fatalf("Recv response failed: %v", err)
	}
	resp, ok := msg.Payload.(*pb.TunnelMessage_RegisterResp)
	if !ok || !resp.RegisterResp.Success {
		t.Fatalf("expected registration success, got: %v", msg.Payload)
	}

	content, ok := mgr.GetUiFile("test-ext", "/index.html")
	if !ok || string(content) != "hello world" {
		t.Fatalf("expected cache file 'hello world', got: %q", string(content))
	}
}

// TestRegisterDevOpenMode covers the dev path: Core completes immediately with a
// skip-upload success and the tunnel becomes routable.
func TestRegisterDevOpenMode(t *testing.T) {
	client, mgr := startServer(t, nil)
	stream, _ := client.Connect(context.Background())
	if err := stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterReq{
		RegisterReq: &pb.RegisterRequest{ExtensionKey: "dev-ext", IsDev: true},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	resp, ok := msg.Payload.(*pb.TunnelMessage_RegisterResp)
	if !ok || !resp.RegisterResp.Success {
		t.Fatalf("expected success, got %v", msg.Payload)
	}
	if mgr.CountReplicas("dev-ext") != 1 {
		t.Fatalf("expected 1 routable replica, got %d", mgr.CountReplicas("dev-ext"))
	}
}

// TestRegisterPending parks a first-time extension as pending: it is held but
// not routable.
func TestRegisterPending(t *testing.T) {
	client, mgr := startServer(t, fakeAuth{result: AuthResult{Action: AuthPending}})
	stream, _ := client.Connect(context.Background())
	if err := stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterReq{
		RegisterReq: &pb.RegisterRequest{ExtensionKey: "pending-ext", IsDev: true},
	}}); err != nil {
		t.Fatalf("send register: %v", err)
	}
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	dec, ok := msg.Payload.(*pb.TunnelMessage_RegisterDecision)
	if !ok || dec.RegisterDecision.Status != "pending" {
		t.Fatalf("expected pending decision, got %v", msg.Payload)
	}
	if mgr.CountPending("pending-ext") != 1 {
		t.Fatalf("expected 1 pending tunnel, got %d", mgr.CountPending("pending-ext"))
	}
	if mgr.CountReplicas("pending-ext") != 0 {
		t.Fatalf("pending tunnel must not be routable")
	}
}

// TestRegisterRejectedAndDenied covers the two terminal decisions.
func TestRegisterRejectedAndDenied(t *testing.T) {
	tests := []struct {
		name       string
		action     AuthAction
		wantStatus string // for decision-bearing outcomes
		wantResp   bool   // true when a RegisterResponse (deny) is expected
	}{
		{name: "rejected", action: AuthReject, wantStatus: "rejected"},
		{name: "denied", action: AuthDeny, wantResp: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := startServer(t, fakeAuth{result: AuthResult{Action: tc.action, Message: "no"}})
			stream, _ := client.Connect(context.Background())
			if err := stream.Send(&pb.TunnelMessage{Payload: &pb.TunnelMessage_RegisterReq{
				RegisterReq: &pb.RegisterRequest{ExtensionKey: "x", IsDev: true},
			}}); err != nil {
				t.Fatalf("send: %v", err)
			}
			msg, err := stream.Recv()
			if err != nil {
				t.Fatalf("recv: %v", err)
			}
			if tc.wantResp {
				resp, ok := msg.Payload.(*pb.TunnelMessage_RegisterResp)
				if !ok || resp.RegisterResp.Success {
					t.Fatalf("expected failure response, got %v", msg.Payload)
				}
				return
			}
			dec, ok := msg.Payload.(*pb.TunnelMessage_RegisterDecision)
			if !ok || dec.RegisterDecision.Status != tc.wantStatus {
				t.Fatalf("expected %s decision, got %v", tc.wantStatus, msg.Payload)
			}
		})
	}
}
