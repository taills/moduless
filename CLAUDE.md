# CLAUDE.md - Developer Guide

Moduless is a Go web gateway with a plugin system. Core fronts all HTTP traffic
and runs plugins as subprocesses it starts itself.

## Architecture

```
Browser ──HTTP──▶ Core (:80)
                    ├─ filter pipeline   plugins intercept request lifecycle phases
                    ├─ /api/plugins/*    routed to a plugin's HTTP handler
                    ├─ /plugins/*        plugin micro-frontends, read from disk
                    ├─ SSE               console learns about changes without reloading
                    └─ PluginHost ──exec──▶ plugin subprocess ×N
                         │  go-plugin: unix socket, AutoMTLS, SHA-256 verified
                         ▼
                       HostServices (reverse channel)
                         documents · queue · cache · locks · config
                         files · outbound HTTP · events · logs & metrics
```

Core listens on **one** port. Plugins open none — Core is their parent process
and talks to them over a private connection.

## Directory layout

```
proto/           plugin.proto (Host→Plugin) and host.proto (Plugin→Host)
pluginapi/       go-plugin glue shared by Core and every plugin binary
pathmatch/       zero-allocation glob matcher for filter path rules
manifest/        manifest.yaml parsing and validation
core/
  pluginhost/    launching, supervising and atomically swapping plugins
  pipeline/      the IIS-style request filter pipeline
  hostsvc/       capabilities Core exposes back to plugins
  gateway/       HTTP routing, admin API, console assets
  db/            document store, durable queue, migrations, sqlc
  auth/ event/ storage/ middleware/
  frontend/      the console (Vue 3 + qiankun)
sdk/plugin/      what plugin authors write against
extension-example/notes/     a complete example plugin
extension-example/ratelimit/ a pure-filter example plugin
tests/           end-to-end tests that fork real plugin processes
```

## Commands

```bash
# Generate protobuf stubs (needs protoc, protoc-gen-go, protoc-gen-go-grpc)
./scripts/gen-proto.sh

# Regenerate the type-safe query layer after editing db/query.sql
cd db && sqlc generate

# Build and test
go build ./...
go test ./... -race
go vet ./... && gofmt -l .

# Run the suite on Linux, which the target actually is. Some behaviour differs
# from macOS in ways that matter here — writing to a running executable fails
# with ETXTBSY, and Pdeathsig only exists at all — so a green run on the
# development machine is not the same as a green run in production.
docker run --rm -v "$(pwd)":/src -w /src -v "$HOME/go/pkg/mod":/go/pkg/mod \
  -e CGO_ENABLED=0 golang:1.25-alpine go test ./... -count=1

# Database-backed tests skip unless this points at a PostgreSQL instance.
# A throwaway one, on a port that will not collide with anything already running:
#   docker run -d --name moduless-test-db -p 15433:5432 \
#     -e POSTGRES_USER=moduless -e POSTGRES_PASSWORD=moduless \
#     -e POSTGRES_DB=moduless_test postgres:18-alpine
TEST_DATABASE_URL='postgres://moduless:moduless@localhost:15433/moduless_test?sslmode=disable' go test ./...

# Console
cd core/frontend && npm install && npm run build   # or npm run dev
```

## Running

```bash
# Without a database: plugins run, but data/queue/file capabilities report Unavailable
PLUGIN_DIR=./plugins go run ./core

# Full stack
DATABASE_URL='postgres://...' PLUGIN_DIR=./plugins go run ./core
```

The default admin is seeded only when the user table is empty — a database
carrying earlier test data will not get one.

`go run` leaves the real server process alive when the wrapper is killed. Build
a binary when you need to stop it reliably.

### Environment

| Variable | Default | Purpose |
|---|---|---|
| `HTTP_ADDR` | `:80` | Listen address |
| `DATABASE_URL` | — | Enables data, queue, files, auth and audit |
| `PLUGIN_DIR` | `./plugins` | Where plugin packages live |
| `PLUGIN_DATA_DIR` | — | Root for per-plugin writable directories |
| `PLUGIN_LOG_LEVEL` | `warn` | Plugin log verbosity |
| `PLUGIN_DEV_MODE` | off | Skips Pdeathsig; development only |
| `HOST_FRONTEND_DIR` | `./core/frontend/dist` | Built console |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | `admin` / `admin123` | Seeded on first run |
| `RUSTFS_*` | — | Object storage; without it the file capability is unavailable |

## Plugin model

A plugin package is a directory:

```
notes/
├── manifest.yaml
├── bin/plugin        # CGO_ENABLED=0 static binary
└── frontend/         # optional micro-frontend dist
```

Core scans `PLUGIN_DIR` at startup, validates each manifest, and starts the
plugins. Enable, disable, reload and rescan are admin API calls under
`/api/system/plugins/*`, surfaced in the console under 插件管理.

See [docs/plugin-development.md](docs/plugin-development.md) for the author's
guide. Three rules matter most, because each fails in a way that does not point
at its cause:

1. **A plugin must never write to stdout.** go-plugin reads the startup
   handshake from the first stdout line.
2. **Plugins must be built with `CGO_ENABLED=0`.** A dynamically linked binary
   fails to exec in the musl-based runtime image.
3. **Deploying a new version must replace the binary, not overwrite it.** The
   previous version is still serving until the upgrade commits, and writing
   into a file that is executing corrupts that process. Use `mv`, not `cp`.

## Conventions

**Plugins never touch PostgreSQL.** Collections are declared in `manifest.yaml`
and Core provisions `ext_<key>_<collection>` tables. This is why `DATABASE_URL`
is deliberately withheld from plugin processes: if a plugin could read it, Core
would no longer own schema, migrations or isolation.

**Binary content does not travel through the plugin transport on reads.**
Uploads go to `/api/system/files/upload`; a plugin asks for a short-lived
download URL and the browser fetches from Core directly. Download URLs use
clean path parameters — `/api/system/files/download/<file_id>/<token>` — with no
query string.

**Trust model.** Plugins are reviewed before installation and run with Core's
own privileges, like an ISAPI filter inside the IIS worker process. Core
enforces permissions, data namespacing and the egress allow-list on its own
side of the connection, but there is no uid downgrade, cgroup limit or seccomp
profile. Genuinely untrusted code belongs behind a container boundary.

**Go style.** `gofmt` and `go vet` are mandatory. Table-driven tests, standard
library `testing`, no assertion frameworks. Migrations are `go-migrate` format
and embedded; queries go through sqlc.
