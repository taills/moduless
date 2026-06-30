package tunnel

import (
	"archive/zip"
	"bytes"
	"context"
	"net"
	"testing"

	pb "github.com/ty-lab/go-web-module/proto/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestRegisterAndUpload(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mgr := NewTunnelManager()
	srv := grpc.NewServer()
	pb.RegisterExtensionTunnelServer(srv, NewTunnelServer(mgr))
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewExtensionTunnelClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// 1. Send Register (non-dev so it expects an upload).
	if err := stream.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_RegisterReq{
			RegisterReq: &pb.RegisterRequest{
				ExtensionKey: "test-ext",
				Version:      "1.0.0",
				IsDev:        false,
			},
		},
	}); err != nil {
		t.Fatalf("Send Register failed: %v", err)
	}

	// Create a dummy zip.
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("index.html")
	f.Write([]byte("hello world"))
	zw.Close()

	// 2. Send zip chunk.
	if err := stream.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_FileChunk{
			FileChunk: &pb.FileChunk{Content: zipBuf.Bytes()},
		},
	}); err != nil {
		t.Fatalf("Send Chunk failed: %v", err)
	}

	// 3. Complete.
	if err := stream.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_RegisterComplete{
			RegisterComplete: &pb.RegisterComplete{},
		},
	}); err != nil {
		t.Fatalf("Send Complete failed: %v", err)
	}

	// 4. Expect success response.
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv response failed: %v", err)
	}
	resp, ok := msg.Payload.(*pb.TunnelMessage_RegisterResp)
	if !ok || !resp.RegisterResp.Success {
		t.Fatalf("Expected registration success, got: %v", msg.Payload)
	}

	// Verify cache.
	content, ok := mgr.GetUiFile("test-ext", "/index.html")
	if !ok || string(content) != "hello world" {
		t.Fatalf("Expected cache file 'hello world', got: %s", string(content))
	}
}
