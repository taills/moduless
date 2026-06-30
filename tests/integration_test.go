package tests

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ty-lab/go-web-module/core/gateway"
	"github.com/ty-lab/go-web-module/core/tunnel"
	pb "github.com/ty-lab/go-web-module/proto/tunnel"
	sdk "github.com/ty-lab/go-web-module/sdk/go"
	"google.golang.org/grpc"
)

func TestFullE2ETunnel(t *testing.T) {
	// 1. Start Core gRPC server.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gRPC listen failed: %v", err)
	}
	defer lis.Close()

	mgr := tunnel.NewTunnelManager()
	grpcSrv := grpc.NewServer()
	pb.RegisterExtensionTunnelServer(grpcSrv, tunnel.NewTunnelServer(mgr))
	go grpcSrv.Serve(lis)
	defer grpcSrv.Stop()

	// 2. Start Go extension SDK using a mock router.
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		u := sdk.GetUser(r.Context())
		if u == nil {
			http.Error(w, "missing auth", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"hello":"` + u.UserID + `"}`))
	})

	go sdk.Start(mux, sdk.Config{
		ExtensionKey: "my-test-app",
		CoreGrpcURL:  lis.Addr().String(),
		IsDev:        true,
	})

	// Wait for the SDK to dial and register.
	waitForTunnel(t, mgr, "my-test-app", 3*time.Second)

	// 3. Test HTTP gateway forwarding.
	gatewayHandler := gateway.NewGatewayHandler(mgr)
	gatewayHTTPServer := httptest.NewServer(gatewayHandler)
	defer gatewayHTTPServer.Close()

	resp, err := http.Get(gatewayHTTPServer.URL + "/api/extensions/my-test-app/hello")
	if err != nil {
		t.Fatalf("HTTP request to gateway failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"hello":"10001"}` {
		t.Fatalf("expected hello 10001 response, got %s", string(body))
	}
}

func waitForTunnel(t *testing.T, mgr *tunnel.TunnelManager, key string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for {
		if _, ok := mgr.GetTunnel(key); ok {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("tunnel %q not registered within %v", key, timeout)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
