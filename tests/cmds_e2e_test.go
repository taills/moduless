package tests

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
	sdk "github.com/taills/moduless/sdk/go"
	"google.golang.org/grpc"
)

// TestCMDSEndToEnd exercises the full manifest -> reconcile -> CMDS path:
// an extension registers carrying manifest collections, Core provisions the
// table, and a tunnelled HTTP handler reads/writes documents via the SDK DB
// client. Skips unless TEST_DATABASE_URL is set.
func TestCMDSEndToEnd(t *testing.T) {
	connStr := os.Getenv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping CMDS E2E test")
	}

	conn, err := db.InitDB(connStr)
	if err != nil {
		t.Skipf("cannot init database: %v", err)
	}
	defer conn.Close()
	defer conn.Exec("DROP TABLE IF EXISTS ext_e2eapp_items;")

	cmds := db.NewCMDSManager(conn)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	mgr := tunnel.NewTunnelManager()
	tsrv := tunnel.NewTunnelServer(mgr)
	tsrv.OnRegister = func(req *pb.RegisterRequest) error {
		cols := make([]db.CollectionSchema, 0, len(req.Collections))
		for _, c := range req.Collections {
			cols = append(cols, db.CollectionSchema{Name: c.Name})
		}
		return cmds.ReconcileSchema(req.ExtensionKey, cols)
	}

	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(tunnel.ExtensionKeyUnaryInterceptor))
	pb.RegisterExtensionTunnelServer(grpcSrv, tsrv)
	pb.RegisterDatabaseServiceServer(grpcSrv, tunnel.NewDbServer(cmds))
	go grpcSrv.Serve(lis)
	defer grpcSrv.Stop()

	// Write a temp manifest declaring the items collection.
	manifestPath := filepath.Join(t.TempDir(), "manifest.yaml")
	os.WriteFile(manifestPath, []byte("key: e2eapp\ndatabase:\n  collections:\n    - name: items\n"), 0o644)

	mux := http.NewServeMux()
	mux.HandleFunc("/save", func(w http.ResponseWriter, r *http.Request) {
		if err := sdk.DB.Put(r.Context(), "items", "doc1", map[string]string{"hello": "world"}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc("/load", func(w http.ResponseWriter, r *http.Request) {
		var out map[string]string
		found, err := sdk.DB.Get(r.Context(), "items", "doc1", &out)
		if err != nil || !found {
			http.Error(w, "not found", 404)
			return
		}
		w.Write([]byte(out["hello"]))
	})

	go sdk.Start(mux, sdk.Config{
		ExtensionKey: "e2eapp",
		CoreGrpcURL:  lis.Addr().String(),
		IsDev:        true,
		ManifestPath: manifestPath,
	})

	waitForTunnel(t, mgr, "e2eapp", 3*time.Second)

	gw := httptest.NewServer(gateway.NewGatewayHandler(mgr))
	defer gw.Close()

	resp, err := http.Get(gw.URL + "/api/extensions/e2eapp/save")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("save failed: status=%v err=%v", resp.StatusCode, err)
	}

	resp, err = http.Get(gw.URL + "/api/extensions/e2eapp/load")
	if err != nil {
		t.Fatalf("load request: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "world" {
		t.Fatalf("expected 'world', got %q (status %d)", string(body), resp.StatusCode)
	}

	// Ensure the SDK actually attached extension-key isolation metadata: the
	// document must be visible directly via CMDS for the same key.
	_ = context.Background()
	if raw, ok, _ := cmds.Get("e2eapp", "items", "doc1"); !ok || len(raw) == 0 {
		t.Fatalf("document not found in CMDS under correct extension key")
	}
}
