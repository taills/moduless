package main

import (
	"context"
	"database/sql"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/taills/moduleless/core/db"
	sqlc "github.com/taills/moduleless/core/db/sqlc"
	"github.com/taills/moduleless/core/event"
	"github.com/taills/moduleless/core/gateway"
	"github.com/taills/moduleless/core/middleware"
	"github.com/taills/moduleless/core/storage"
	"github.com/taills/moduleless/core/tunnel"
	pb "github.com/taills/moduleless/proto/tunnel"
	"google.golang.org/grpc"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	httpAddr := env("HTTP_ADDR", ":80")
	grpcAddr := env("GRPC_ADDR", ":9000")
	databaseURL := os.Getenv("DATABASE_URL")

	manager := tunnel.NewTunnelManager()
	bus := event.NewEventBus()
	slots := gateway.NewSlotRegistry()

	// gRPC server with per-extension identity interceptor.
	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(tunnel.ExtensionKeyUnaryInterceptor))
	tunnelSrv := tunnel.NewTunnelServer(manager)
	pb.RegisterExtensionTunnelServer(grpcSrv, tunnelSrv)
	pb.RegisterEventBusServiceServer(grpcSrv, tunnel.NewEventServer(bus))

	gw := gateway.NewGatewayHandler(manager)

	var queries *sqlc.Queries
	var conn *sql.DB
	var cmds *db.CMDSManager

	// Database-backed services (CMDS, files, audit) are optional so Core can
	// run a pure tunnel demo without PostgreSQL/RustFS.
	if databaseURL != "" {
		var err error
		conn, err = db.InitDB(databaseURL)
		if err != nil {
			log.Fatalf("database init failed: %v", err)
		}
		defer conn.Close()
		queries = sqlc.New(conn)

		cmds = db.NewCMDSManager(conn)
		pb.RegisterDatabaseServiceServer(grpcSrv, tunnel.NewDbServer(cmds))
		pb.RegisterFileServiceServer(grpcSrv, tunnel.NewFileServer(queries))
		log.Println("[core] CMDS DatabaseService + FileService enabled")

		if rustfs := buildStorage(); rustfs != nil {
			fileHandler := gateway.NewFileHandler(rustfs, queries)
			gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/files/upload" }, fileHandler.Upload)
			gw.RegisterSystemRoute(func(p string) bool { return hasPrefix(p, "/api/system/files/download/") }, fileHandler.Download)
			log.Println("[core] RustFS file upload/download routes enabled")
		}
	} else {
		log.Println("[core] DATABASE_URL not set; running tunnel + event bus only")
	}

	// On registration: provision CMDS schema from the manifest and register UI
	// slots. On unregister: drop the slots.
	tunnelSrv.OnRegister = func(req *pb.RegisterRequest) error {
		if cmds != nil && len(req.Collections) > 0 {
			cols := make([]db.CollectionSchema, 0, len(req.Collections))
			for _, c := range req.Collections {
				idxs := make([]db.Index, 0, len(c.Indexes))
				for _, idx := range c.Indexes {
					idxs = append(idxs, db.Index{Fields: idx.Fields, Unique: idx.Unique})
				}
				cols = append(cols, db.CollectionSchema{Name: c.Name, Indexes: idxs})
			}
			if err := cmds.ReconcileSchema(req.ExtensionKey, cols); err != nil {
				return err
			}
			log.Printf("[core] reconciled %d collection(s) for %s", len(cols), req.ExtensionKey)
		}
		if len(req.Slots) > 0 {
			uiSlots := make([]gateway.UISlot, 0, len(req.Slots))
			for _, s := range req.Slots {
				uiSlots = append(uiSlots, gateway.UISlot{
					SlotName:       s.SlotName,
					ExtensionKey:   req.ExtensionKey,
					ComponentEntry: s.ComponentEntry,
				})
			}
			slots.Register(req.ExtensionKey, uiSlots)
		}
		return nil
	}
	tunnelSrv.OnUnregister = func(extKey string) {
		slots.Unregister(extKey)
	}

	// System routes available regardless of DB.
	gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/ui/slots" }, slots.Handler)
	gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/diagnostics" }, gateway.GetDiagnostics(manager))

	// Wrap the gateway with the audit middleware when a recorder is available.
	var httpHandler http.Handler = gw
	if queries != nil {
		httpHandler = middleware.AuditLogger(queries)(gw)
	}

	// Start gRPC listener.
	grpcLis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		log.Fatalf("grpc listen %s: %v", grpcAddr, err)
	}
	go func() {
		log.Printf("[core] gRPC tunnel listening on %s", grpcAddr)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			log.Fatalf("grpc serve: %v", err)
		}
	}()

	// Start HTTP gateway.
	httpSrv := &http.Server{Addr: httpAddr, Handler: httpHandler}
	go func() {
		log.Printf("[core] HTTP gateway listening on %s", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http serve: %v", err)
		}
	}()

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("[core] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	grpcSrv.GracefulStop()
	log.Println("[core] stopped")
}

// buildStorage constructs the RustFS client when all S3 env vars are present.
func buildStorage() *storage.RustFSClient {
	endpoint := os.Getenv("RUSTFS_ENDPOINT")
	bucket := os.Getenv("RUSTFS_BUCKET")
	accessKey := os.Getenv("RUSTFS_ACCESS_KEY")
	secretKey := os.Getenv("RUSTFS_SECRET_KEY")
	if endpoint == "" || bucket == "" {
		log.Println("[core] RUSTFS_* not set; file service storage disabled")
		return nil
	}
	client, err := storage.NewRustFSClient(endpoint, bucket, accessKey, secretKey)
	if err != nil {
		log.Printf("[core] RustFS init failed: %v", err)
		return nil
	}
	return client
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
