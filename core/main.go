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

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/db"
	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/event"
	"github.com/taills/moduless/core/extension"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/middleware"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/core/storage"
	"github.com/taills/moduless/core/tunnel"
	pluginpb "github.com/taills/moduless/proto/plugin"
	pb "github.com/taills/moduless/proto/tunnel"
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
	tunnelSrv := tunnel.NewTunnelServer(manager)
	gw := gateway.NewGatewayHandler(manager)

	// provision reconciles CMDS schema and registers UI slots from a manifest. It
	// runs both when an approved extension (re)connects and when an admin approves
	// a pending one, so the tunnel server and the approval coordinator share it.
	provision := func(req *pb.RegisterRequest) error { return nil }

	var queries *sqlc.Queries
	var conn *sql.DB
	var cmds *db.CMDSManager
	var extStore *extension.Store
	var coordinator *extension.Coordinator
	var authStore *auth.Store

	// Capabilities Core exposes back to plugins. Cache and locks are
	// in-process: Core is single-instance and every plugin replica is its
	// child, so that is already correct for the only topology there is.
	pluginConfig := hostsvc.NewStaticConfig()
	hostDeps := hostsvc.Deps{
		Cache:  hostsvc.NewMemoryCache(0),
		Locks:  hostsvc.NewMemoryLocks(),
		Config: pluginConfig,
		Events: hostsvc.NewBusEvents(bus),
		Obs:    hostsvc.NewLogObservability(env("PLUGIN_LOG_LEVEL", "info")),
	}
	hostsvc.SetLogger(func(format string, args ...any) {
		log.Printf("[hostsvc] "+format, args...)
	})
	// A subscriber falling behind is best-effort by design, but it must not be
	// invisible: anything that cannot be lost belongs on the durable queue.
	bus.OnDrop(func(name string) {
		log.Printf("[hostsvc] event %q dropped: a subscriber is not keeping up", name)
	})

	// Database-backed services (CMDS, files, audit, approval) are optional so Core
	// can run a pure tunnel demo without PostgreSQL/RustFS.
	if databaseURL != "" {
		var err error
		conn, err = db.InitDB(databaseURL)
		if err != nil {
			log.Fatalf("database init failed: %v", err)
		}
		defer conn.Close()
		queries = sqlc.New(conn)
		cmds = db.NewCMDSManager(conn)

		// Capabilities that need PostgreSQL. Without a database these stay nil
		// and report Unavailable rather than failing obscurely.
		txRegistry := db.NewTxRegistry()
		txRegistry.StartReaper(time.Second)
		defer txRegistry.Close()
		hostDeps.Data = hostsvc.NewCMDSData(conn, cmds, txRegistry)

		queue := hostsvc.NewPGQueue(db.NewQueue(conn))
		queue.StartMaintenance(context.Background(), 30*time.Second, 24*time.Hour)
		hostDeps.Queue = queue
	} else {
		log.Println("[core] DATABASE_URL not set; running tunnel + event bus only (open registration)")
	}

	// The schema/slot provisioning closure (no-op without a database).
	provision = func(req *pb.RegisterRequest) error {
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
	tunnelSrv.OnRegister = provision
	tunnelSrv.OnUnregister = slots.Unregister

	// The data-plane interceptor gates DB/File/Event calls; when a registry is
	// available it also rejects calls from keys that are not approved.
	if queries != nil {
		extStore = extension.NewStore(queries)
		tunnelSrv.Auth = extStore
		coordinator = &extension.Coordinator{
			Store:        extStore,
			Manager:      manager,
			Provision:    provision,
			OnUnregister: slots.Unregister,
		}
	}

	var grpcInterceptor grpc.UnaryServerInterceptor = tunnel.ExtensionKeyUnaryInterceptor
	if extStore != nil {
		grpcInterceptor = tunnel.ApprovedKeyUnaryInterceptor(extStore)
	}
	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(grpcInterceptor))
	pb.RegisterExtensionTunnelServer(grpcSrv, tunnelSrv)
	pb.RegisterEventBusServiceServer(grpcSrv, tunnel.NewEventServer(bus))

	if queries != nil {
		pb.RegisterDatabaseServiceServer(grpcSrv, tunnel.NewDbServer(cmds))
		pb.RegisterFileServiceServer(grpcSrv, tunnel.NewFileServer(queries))
		log.Println("[core] CMDS DatabaseService + FileService enabled")

		// Real authentication: verify against system_users, seed a default admin,
		// and let the gateway inject the authenticated identity.
		authStore = auth.NewStore(queries)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if seeded, err := authStore.SeedDefaultAdmin(ctx, env("ADMIN_USERNAME", "admin"), env("ADMIN_PASSWORD", "admin123")); err != nil {
			log.Printf("[core] seed admin failed: %v", err)
		} else if seeded {
			log.Printf("[core] seeded default admin user %q (set ADMIN_PASSWORD to override)", env("ADMIN_USERNAME", "admin"))
		}
		cancel()
		gw.Auth = authStore

		authHandler := gateway.NewAuthHandler(authStore)
		gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/auth/login" }, authHandler.Login)
		gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/auth/me" }, authHandler.Me)
		gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/auth/logout" }, authHandler.Logout)
		log.Println("[core] auth endpoints enabled (/api/system/auth/*)")

		// Admin baseline: user management and extension approval management.
		usersHandler := gateway.NewUsersHandler(authStore)
		gw.RegisterSystemRoute(func(p string) bool {
			return p == "/api/system/users" || hasPrefix(p, "/api/system/users/")
		}, usersHandler.Serve)
		extHandler := gateway.NewExtensionsHandler(authStore, coordinator)
		gw.RegisterSystemRoute(func(p string) bool {
			return p == "/api/system/extensions" || hasPrefix(p, "/api/system/extensions/")
		}, extHandler.Serve)
		log.Println("[core] admin endpoints enabled (/api/system/users/*, /api/system/extensions/*)")

		if rustfs := buildStorage(); rustfs != nil {
			hostDeps.Files = hostsvc.NewFiles(conn, queries, rustfs)
			fileHandler := gateway.NewFileHandler(rustfs, queries)
			gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/files/upload" }, fileHandler.Upload)
			gw.RegisterSystemRoute(func(p string) bool { return hasPrefix(p, "/api/system/files/download/") }, fileHandler.Download)
			log.Println("[core] RustFS file upload/download routes enabled")
		}
	}

	// System routes available regardless of DB (triggering air reload).
	gw.RegisterSystemRoute(func(p string) bool { return p == "/healthz" }, healthz)
	gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/ui/slots" }, slots.Handler)
	gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/ui/apps" }, gw.AppsHandler)
	gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/diagnostics" }, gateway.GetDiagnostics(manager))

	// Serve the qiankun host app (and its SPA routes) at the web root.
	gw.Host = gateway.NewHostHandler(env("HOST_FRONTEND_DIR", "./core/frontend/dist"))

	// ---- Plugin subsystem (go-plugin subprocesses) -------------------------
	//
	// Runs alongside the legacy reverse tunnel during the migration. Plugins
	// are launched by Core rather than dialling in, which is what makes hot
	// load, unload and upgrade possible.
	pluginhost.SetLogger(func(format string, args ...any) {
		log.Printf("[plugin] "+format, args...)
	})

	registry := pluginhost.NewRegistry()
	pluginDir := env("PLUGIN_DIR", "./plugins")
	pluginManager := pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         pluginDir,
		DataDirRoot: env("PLUGIN_DATA_DIR", ""),
		LogLevel:    env("PLUGIN_LOG_LEVEL", "warn"),
		// DevMode skips Pdeathsig so air's rebuild loop does not cold-start
		// every plugin on each edit. It must stay off in production.
		DevMode: os.Getenv("PLUGIN_DEV_MODE") == "1",
	}, registry, func(pkg *pluginhost.Package) pluginpb.HostServicesServer {
		// Create the collections the plugin declared before it starts, so its
		// first write does not fail on a missing table. ReconcileSchema is
		// idempotent, so a restart or upgrade simply re-checks them.
		if data, ok := hostDeps.Data.(*hostsvc.CMDSData); ok {
			if err := data.ProvisionSchema(pkg.Key(), collectionsOf(pkg)); err != nil {
				log.Printf("[core] plugin %s: %v", pkg.Key(), err)
			}
		}
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, hostDeps)
	})
	defer pluginManager.Close()

	// Outbound HTTP reads its allow list from the plugin's own manifest, so
	// enabling a new version picks up a changed list without a restart.
	egress := hostsvc.NewHTTPEgress(func(key string) []string {
		if pkg, ok := pluginManager.Package(key); ok {
			return pkg.Manifest.EgressAllow
		}
		return nil
	})
	egress.OnRequest = func(pluginKey, method, url string, statusCode int, err error) {
		if err != nil {
			log.Printf("[hostsvc] egress refused: %s %s %s: %v", pluginKey, method, url, err)
		}
	}
	hostDeps.Egress = egress

	pluginManager.Scan()
	if err := pluginManager.EnableAll(context.Background()); err != nil {
		log.Printf("[core] some plugins failed to start: %v", err)
	}
	for _, st := range pluginManager.List() {
		if st.LoadError != "" {
			log.Printf("[core] plugin %s not loaded: %s", st.Key, st.LoadError)
			continue
		}
		log.Printf("[core] plugin %s v%s: %d/%d replica(s) ready, %d filter(s)",
			st.Key, st.Version, st.Ready, st.Replicas, st.Filters)
	}

	// Static assets for plugin micro-frontends. Unlike the tunnel's in-memory
	// cache these live on disk, so they survive a Core restart instead of
	// waiting for an extension to reconnect and re-upload them.
	gw.Plugins = pluginManager
	gw.RegisterSystemRoute(
		func(p string) bool { return hasPrefix(p, gateway.PluginAssetPrefix) },
		gateway.PluginAssetHandler(pluginManager),
	)

	// Admin plugin management, and the event stream that lets the console
	// reflect an enable or disable without the user reloading the page.
	//
	// authStore is assigned through a typed variable rather than passed
	// directly: a nil *auth.Store stored in an interface is not a nil
	// interface, so `if h.Auth != nil` would pass and the call would panic on
	// a Core running without a database.
	var adminAuth gateway.UserResolver
	if authStore != nil {
		adminAuth = authStore
	}
	pluginsHandler := gateway.NewPluginsHandler(adminAuth, pluginManager)
	gw.RegisterSystemRoute(func(p string) bool {
		return p == gateway.PluginsAPIPrefix || hasPrefix(p, gateway.PluginsAPIPrefix+"/")
	}, pluginsHandler.Serve)

	uiEvents := gateway.NewUIEvents()
	gw.RegisterSystemRoute(func(p string) bool { return p == gateway.UIEventsPath }, uiEvents.Handler)
	registry.OnChange(func(*pluginhost.Snapshot) { uiEvents.Publish("registry.changed") })
	log.Println("[core] plugin endpoints enabled (/api/system/plugins/*, /api/system/ui/events)")

	pluginGateway := &gateway.PluginHandler{
		Registry: registry,
		Runner: &pipeline.Runner{
			OnFilterError: func(f *pipeline.Filter, err error) {
				// A fail-open filter that is broken would otherwise disappear
				// from operations precisely because it failed open.
				log.Printf("[plugin] filter %s: %v", f.Label(), err)
			},
		},
	}
	if authStore != nil {
		pluginGateway.Auth = authStore
	}

	// Wrap the gateway with the audit middleware when a recorder is available.
	var httpHandler http.Handler = pluginGateway.Middleware(gw)
	if queries != nil {
		httpHandler = middleware.AuditLogger(queries)(httpHandler)
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

	// Stop plugins after the HTTP listener closes, so in-flight requests get
	// a chance to finish rather than being cut off mid-response.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	registry.DrainAll(drainCtx, 20*time.Second)
	drainCancel()

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

// collectionsOf converts a plugin's declared collections into the store's
// schema type.
func collectionsOf(pkg *pluginhost.Package) []db.CollectionSchema {
	cols := make([]db.CollectionSchema, 0, len(pkg.Manifest.Database.Collections))
	for _, c := range pkg.Manifest.Database.Collections {
		idxs := make([]db.Index, 0, len(c.Indexes))
		for _, idx := range c.Indexes {
			idxs = append(idxs, db.Index{Fields: idx.Fields, Unique: idx.Unique})
		}
		cols = append(cols, db.CollectionSchema{Name: c.Name, Indexes: idxs})
	}
	return cols
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// healthz is a liveness/readiness probe for process supervisors (k8s, systemd,
// Docker HEALTHCHECK). It reports OK as soon as the HTTP gateway is serving.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
