package tests

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
	sdk "github.com/taills/moduless/sdk/go"
	"google.golang.org/grpc"
)

// TestFrontendUploadE2E exercises the production frontend path end to end: the
// SDK zips a local dist dir, streams it over the tunnel, Core extracts it, and
// the gateway serves the files at /extensions/<key>/<path>.
func TestFrontendUploadE2E(t *testing.T) {
	// Bundle a fake built frontend.
	dist := t.TempDir()
	mustWrite(t, filepath.Join(dist, "index.html"), "<html>go_example</html>")
	mustWrite(t, filepath.Join(dist, "assets", "app.js"), "export const x=1")

	// Start Core gRPC tunnel server.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	mgr := tunnel.NewTunnelManager()
	grpcSrv := grpc.NewServer()
	pb.RegisterExtensionTunnelServer(grpcSrv, tunnel.NewTunnelServer(mgr))
	go grpcSrv.Serve(lis)
	defer grpcSrv.Stop()

	// Start the SDK in production mode pointing at the dist dir.
	go sdk.Start(http.NewServeMux(), sdk.Config{
		ExtensionKey: "go_example",
		CoreGrpcURL:  lis.Addr().String(),
		FrontendDir:  dist,
	})

	waitForTunnel(t, mgr, "go_example", 5*time.Second)

	// The gateway should serve the uploaded assets once extraction completes.
	gw := httptest.NewServer(gateway.NewGatewayHandler(mgr))
	defer gw.Close()

	waitForUI(t, gw.URL+"/extensions/go_example/index.html", "<html>go_example</html>", 5*time.Second)
	waitForUI(t, gw.URL+"/extensions/go_example/assets/app.js", "export const x=1", 5*time.Second)
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// waitForUI polls until the gateway returns the expected body or times out.
// Extraction happens asynchronously after RegisterComplete, so we retry.
func waitForUI(t *testing.T, url, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			body := readAll(resp)
			if body == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("UI %s not served as %q within %v", url, want, timeout)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func readAll(resp *http.Response) string {
	defer resp.Body.Close()
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return string(buf)
}
