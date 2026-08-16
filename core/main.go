// Command core is the Moduless gateway.
//
// It fronts all HTTP traffic, hosts the console, and owns the lifecycle of
// plugin subprocesses. Plugins are launched by Core rather than dialling in,
// so Core listens on exactly one port — the reverse tunnel and its separate
// gRPC listener are gone.
package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/taills/moduless/core/auth"
	"github.com/taills/moduless/core/db"
	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/event"
	"github.com/taills/moduless/core/gateway"
	"github.com/taills/moduless/core/hostsvc"
	"github.com/taills/moduless/core/middleware"
	"github.com/taills/moduless/core/pipeline"
	"github.com/taills/moduless/core/pluginhost"
	"github.com/taills/moduless/core/storage"
	pluginpb "github.com/taills/moduless/proto/plugin"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	httpAddr := env("HTTP_ADDR", ":80")
	databaseURL := os.Getenv("DATABASE_URL")

	bus := event.NewEventBus()
	gw := gateway.NewGatewayHandler()

	var (
		queries   *sqlc.Queries
		conn      *sql.DB
		authStore *auth.Store
	)

	// Capabilities Core exposes back to plugins. Cache and locks are
	// in-process: Core is single-instance and every plugin replica is its
	// child, so that is already correct for the only topology there is.
	hostDeps := hostsvc.Deps{
		Cache: hostsvc.NewMemoryCache(0),
		Locks: hostsvc.NewMemoryLocks(),
		// Config is deliberately absent here: it is per-plugin, and is filled
		// in below from the manager so there is exactly one answer to what a
		// plugin's configuration is.
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

	// Database-backed capabilities are optional so Core can run without
	// PostgreSQL; they then report Unavailable rather than failing obscurely.
	var pluginConfig *hostsvc.DBConfig
	var pluginQueue *hostsvc.PGQueue
	// deadLetters backs the admin endpoint that lists and retries what the
	// queue gave up on. Nil without a database, and the endpoint says so.
	var deadLetters *db.Queue
	if databaseURL != "" {
		var err error
		conn, err = db.InitDB(databaseURL)
		if err != nil {
			log.Fatalf("database init failed: %v", err)
		}
		defer conn.Close()
		queries = sqlc.New(conn)

		// Settings an operator sets have to outlive the process. Without a
		// database there is nowhere to write them, and only the defaults a
		// manifest declares ever take effect.
		pluginConfig = hostsvc.NewDBConfig(conn)

		txRegistry := db.NewTxRegistry()
		txRegistry.StartReaper(time.Second)
		defer txRegistry.Close()
		hostDeps.Data = hostsvc.NewCMDSData(conn, db.NewCMDSManager(conn), txRegistry)

		rawQueue := db.NewQueue(conn)
		// The moment work that was accepted will never be done. Nothing marked
		// it before: the handler had already returned an error and moved on,
		// and the depth the console shows counts pending and processing, so
		// giving up on a backlog made the number go down.
		rawQueue.OnDeadLetter = func(pluginKey, topic string, id int64, attempts int, reason string) {
			log.Printf("[queue] gave up on message %d for %s/%s after %d attempts: %s",
				id, pluginKey, topic, attempts, reason)
		}
		queue := hostsvc.NewPGQueue(rawQueue)
		queue.StartMaintenance(context.Background(), 30*time.Second, 24*time.Hour)
		hostDeps.Queue = queue
		pluginQueue = queue
		deadLetters = rawQueue

		log.Println("[core] document store and durable queue enabled")
	} else {
		log.Println("[core] DATABASE_URL not set; data, queue and file capabilities are unavailable")
	}

	if queries != nil {
		// Real authentication: verify against system_users, seeding a default
		// admin the first time the user table is empty.
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

		usersHandler := gateway.NewUsersHandler(authStore)
		gw.RegisterSystemRoute(func(p string) bool {
			return p == "/api/system/users" || hasPrefix(p, "/api/system/users/")
		}, usersHandler.Serve)
		log.Println("[core] auth and user management enabled")

		if rustfs := buildStorage(); rustfs != nil {
			hostDeps.Files = hostsvc.NewFiles(conn, queries, rustfs)
			fileHandler := gateway.NewFileHandler(rustfs, queries)
			fileHandler.Auth = authStore
			gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/files/upload" }, fileHandler.Upload)
			gw.RegisterSystemRoute(func(p string) bool { return hasPrefix(p, "/api/system/files/download/") }, fileHandler.Download)
			log.Println("[core] file upload/download enabled")
		}
	}

	// ---- Plugin subsystem --------------------------------------------------

	pluginhost.SetLogger(func(format string, args ...any) {
		log.Printf("[plugin] "+format, args...)
	})

	registry := pluginhost.NewRegistry()
	// Declared before it is built because the HostServices factory below has to
	// ask the manager what a plugin's configuration is.
	var pluginManager *pluginhost.Manager
	pluginManager = pluginhost.NewManager(pluginhost.ManagerConfig{
		Dir:         env("PLUGIN_DIR", "./plugins"),
		DataDirRoot: env("PLUGIN_DATA_DIR", ""),
		LogLevel:    env("PLUGIN_LOG_LEVEL", "warn"),
		// What a plugin is launched with. Read at every launch rather than
		// cached, so a restarted or upgraded plugin picks up whatever the
		// operator set in the meantime.
		// A backlog growing toward the queue's ceiling should be visible
		// before Enqueue starts refusing.
		QueueDepth: func(pluginKey string) int64 {
			if pluginQueue == nil {
				return 0
			}
			return pluginQueue.Depth(pluginKey)
		},
		QueueDead: func(pluginKey string) int64 {
			if pluginQueue == nil {
				return 0
			}
			return pluginQueue.Dead(pluginKey)
		},
		ConfigSource: func(pluginKey string) map[string]string {
			if pluginConfig == nil {
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			values, err := pluginConfig.Get(ctx, pluginKey)
			if err != nil {
				// Launching with the manifest defaults beats not launching:
				// a plugin that is down cannot be reconfigured back up.
				log.Printf("[core] plugin %s: reading stored config: %v", pluginKey, err)
				return nil
			}
			return values
		},
	}, registry, func(pkg *pluginhost.Package) pluginpb.HostServicesServer {
		// Create the collections the plugin declared before it starts, so its
		// first write does not fail on a missing table. ReconcileSchema is
		// idempotent, so a restart or upgrade simply re-checks them.
		if data, ok := hostDeps.Data.(*hostsvc.CMDSData); ok {
			if err := data.ProvisionSchema(pkg.Key(), collectionsOf(pkg)); err != nil {
				log.Printf("[core] plugin %s: %v", pkg.Key(), err)
			}
		}
		// A plugin reads its configuration two ways — handed to it at
		// Configure, and asked for over the reverse channel whenever it likes
		// — and those must agree. Both are answered from the manager, so a
		// plugin that re-reads its own settings cannot get a different answer
		// from the one it was started with.
		deps := hostDeps
		deps.Config = hostsvc.ConfigFunc(func(_ context.Context, key string) (map[string]string, error) {
			return pluginManager.ConfigFor(key), nil
		})
		return hostsvc.New(pkg.Key(), pkg.Manifest.Permissions, deps)
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
	egress.OnRequest = func(pluginKey, method, url string, _ int, err error) {
		if err != nil {
			log.Printf("[hostsvc] egress refused: %s %s %s: %v", pluginKey, method, url, err)
		}
	}
	hostDeps.Egress = egress

	pluginManager.Scan()
	if err := pluginManager.EnableAll(context.Background()); err != nil {
		log.Printf("[core] some plugins failed to start: %v", err)
	}

	// Cron jobs declared in manifests. Core owns the schedule rather than each
	// plugin running its own timer: that is what makes a job stop when its
	// plugin is disabled, and run once rather than once per replica.
	scheduler := pluginhost.NewScheduler(registry, pluginManager)
	scheduler.OnJobResult = func(pluginKey, jobName string, err error) {
		if err != nil {
			// A nightly job that has been failing for a week is invisible
			// unless something says so: nobody is watching at 03:17.
			log.Printf("[core] plugin %s job %s failed: %v", pluginKey, jobName, err)
		}
	}
	scheduler.Start(context.Background())
	defer scheduler.Stop()

	for _, st := range pluginManager.List() {
		if st.LoadError != "" {
			log.Printf("[core] plugin %s not loaded: %s", st.Key, st.LoadError)
			continue
		}
		log.Printf("[core] plugin %s v%s: %d/%d replica(s) ready, %d filter(s)",
			st.Key, st.Version, st.Ready, st.Replicas, st.Filters)
	}

	// Plugin micro-frontends are served from their package directory, so they
	// survive a Core restart.
	gw.Plugins = pluginManager
	gw.RegisterSystemRoute(
		func(p string) bool { return hasPrefix(p, gateway.PluginAssetPrefix) },
		gateway.PluginAssetHandler(pluginManager),
	)

	// A nil *auth.Store in an interface is not a nil interface, so it is
	// assigned through a typed variable: otherwise the admin check would pass
	// and then panic on a Core running without a database.
	var adminAuth gateway.UserResolver
	if authStore != nil {
		adminAuth = authStore
	}

	pluginsHandler := gateway.NewPluginsHandler(adminAuth, pluginManager)
	if pluginConfig != nil {
		pluginsHandler.Config = pluginConfig
	}
	if deadLetters != nil {
		pluginsHandler.DeadLetters = deadLetters
	}
	gw.RegisterSystemRoute(func(p string) bool {
		return p == gateway.PluginsAPIPrefix || hasPrefix(p, gateway.PluginsAPIPrefix+"/")
	}, pluginsHandler.Serve)

	uiEvents := gateway.NewUIEvents()
	gw.RegisterSystemRoute(func(p string) bool { return p == gateway.UIEventsPath }, uiEvents.Handler)
	registry.OnChange(func(*pluginhost.Snapshot) { uiEvents.Publish("registry.changed") })

	gw.RegisterSystemRoute(func(p string) bool { return p == "/healthz" }, healthz)
	gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/ui/apps" }, gw.AppsHandler)
	log.Println("[core] plugin endpoints enabled (/api/system/plugins/*, /api/system/ui/events)")

	// The console SPA at the web root.
	gw.Host = gateway.NewHostHandler(env("HOST_FRONTEND_DIR", "./core/frontend/dist"))

	// The filter pipeline wraps the whole gateway rather than only plugin
	// routes: filters are global by design, so a rate limiter has to see
	// requests Core itself serves too.
	pluginGateway := &gateway.PluginHandler{
		Registry: registry,
		Auth:     adminAuth,
		Runner: &pipeline.Runner{
			OnFilterError: func(f *pipeline.Filter, err error) {
				// A fail-open filter that is broken would otherwise disappear
				// from operations precisely because it failed open.
				log.Printf("[plugin] filter %s: %v", f.Label(), err)
			},
		},
	}

	var httpHandler http.Handler = pluginGateway.Middleware(gw)
	if queries != nil {
		httpHandler = middleware.AuditLogger(queries, middleware.AuditOptions{
			// The prefix comes from whoever owns the route. Keeping a second
			// copy here is how auditing silently stopped working once already.
			Prefix: gateway.PluginAPIPrefix,
			// Identity is resolved the same way every other handler resolves
			// it. Anything a client can put in a header is a name it chose.
			Identify: func(r *http.Request) string {
				if adminAuth == nil {
					return ""
				}
				user, ok := adminAuth.Resolve(gateway.SessionToken(r))
				if !ok {
					return ""
				}
				return strconv.Itoa(int(user.ID))
			},
		})(httpHandler)
	}

	httpSrv := &http.Server{Addr: httpAddr, Handler: httpHandler}
	go func() {
		log.Printf("[core] HTTP gateway listening on %s", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("[core] shutting down...")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)

	// Plugins stop after the listener closes, so in-flight requests get a
	// chance to finish rather than being cut off mid-response.
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
	registry.DrainAll(drainCtx, 20*time.Second)
	drainCancel()

	log.Println("[core] stopped")
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

// buildStorage constructs the object-store client when its env vars are set.
func buildStorage() *storage.RustFSClient {
	endpoint := os.Getenv("RUSTFS_ENDPOINT")
	bucket := os.Getenv("RUSTFS_BUCKET")
	if endpoint == "" || bucket == "" {
		log.Println("[core] RUSTFS_* not set; file storage disabled")
		return nil
	}
	client, err := storage.NewRustFSClient(endpoint, bucket,
		os.Getenv("RUSTFS_ACCESS_KEY"), os.Getenv("RUSTFS_SECRET_KEY"))
	if err != nil {
		log.Printf("[core] object store init failed: %v", err)
		return nil
	}
	return client
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// healthz is a liveness probe for process supervisors.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
