# CLAUDE.md - Developer Guide

Welcome to the Multi-Language Modular Framework project. This guide outlines build, test, and style guidelines for working with the Core Gateway and its multi-language SDKs.

## Directory Layout

```
/
├── proto/                    # gRPC Protobuf contracts
├── core/                     # Go Core Gateway (HTTP Router, Tunnel server)
├── sdk/                      # Multi-language SDKs
│   ├── go/                   # Go Extension SDK
│   ├── python/               # Python Extension SDK (FastAPI / ASGI)
│   └── java/                 # Java Extension SDK (Spring Boot / Servlet)
├── extension-example/        # Example micro-frontend + backend modules
│   ├── go/
│   ├── python/
│   └── java/
├── scripts/                  # Protobuf build and code gen scripts
└── docs/                     # Spec and implementation plans
```

---

## Build & Run Commands

### 1. Protobuf Code Generation
Generate the gRPC and Protobuf stubs before running any compiler:
* **Go**: Run `./scripts/gen-proto.sh`
* **Python**: Run `./scripts/gen-proto-python.sh`
* **Java**: Run `cd sdk/java && mvn protobuf:compile protobuf:compile-custom`

### 2. Running Core Gateway
* **Run in Dev Mode**: `go run core/main.go` (listens to HTTP :80 and gRPC :9000 by default)

Core serves the **qiankun host app (console)** at `/`. Build it once with
`cd core/frontend && npm install && npm run build` (Core reads it from
`HOST_FRONTEND_DIR`, default `./core/frontend/dist`), or run its dev server with
`npm run dev` (proxies `/api` and `/extensions` to a running Core). With a
database configured, Core seeds a default admin on first start — `admin` /
`admin123` (override via `ADMIN_USERNAME` / `ADMIN_PASSWORD`). Login issues a
session token; the gateway enforces it on `/api/extensions/*`.

The console includes **baseline admin features** (admin role only): **用户管理**
(`/api/system/users/*`) and **扩展管理** (`/api/system/extensions/*`).

#### Extension registration & approval

Extensions are **not** auto-trusted. Registration follows an admin approval flow
(enforced whenever `DATABASE_URL` is set; open registration without it):

1. An extension dials Core with no secret → recorded as **`待注册` (pending)**,
   connection held open but **not routed**.
2. Admin clicks **批准** in 扩展管理 → Core mints a per-instance secret, pushes it
   over the tunnel (the SDK persists it into `manifest.yaml` as `secret:`),
   provisions schema/slots, and routes it (**`已注册`**).
3. Reconnects replay the persisted secret for immediate routing.
4. **拒绝** revokes secrets + disconnects; **删除** lets the extension re-apply.

One extension key may own **multiple secrets** (one per instance). Generate extra
secrets in the console and pass them to replicas via the `EXTENSION_SECRET` env.
A no-secret dial to an approved key is **not routed**; it is parked as a **pending
instance** for admin re-approval (so a restarted replica that lost its persisted
secret recovers by re-approval — Core mints it a fresh secret). Key hijacking is
still prevented: an unauthenticated dial can neither downgrade the approved row
nor be routed without an explicit admin **批准**. See
[docs/deployment.md](docs/deployment.md) for secret persistence in containers.

#### Extension menus

An extension declares its host-app menu tree in `manifest.yaml` under `menus:` —
a nested list of nodes, each with `path`, `title`, `icon`, `order`, `entry`
(the micro-frontend html path; empty = a pure organizational node), `roles`
(when non-empty, Core filters the node out for users lacking the role before
sending the tree to the host), and `children`. The legacy single `menu:`
(`icon`/`path`) still works and is auto-promoted to a one-node `menus:` tree, so
old manifests need no change. Core persists the tree (migration `000005`,
backfilled from legacy fields by `000006`), merges nodes across extensions by
`path` (first declarer wins a shared parent's title/icon), and the console renders
the role-filtered result. Paths must be unique **within** one extension;
cross-extension path collisions are expected and merged by Core.

### 3. Running Extensions
Extensions do not listen to ports. Run them in IDEs or terminals by passing the `CORE_URL` or configuration:
* **Go Extension**: `go run extension-example/go/backend/main.go`
* **Python Extension**: `python3 extension-example/python/backend/main.py`
* **Java Extension**: `mvn spring-boot:run -pl extension-example/java/backend`

In dev mode the micro-frontend runs from its own Vite dev server (`npm run dev`). In **production** the built `dist/` is bundled into the backend image and the SDK uploads it to Core on startup — set `FRONTEND_DIR` to the dist path (the examples read this env var). Each example frontend uses `vite-plugin-qiankun` so the console can load it as a micro-app. Each example ships a multi-stage `Dockerfile`; `docker compose up --build` runs Core + PostgreSQL + all three examples. See [docs/deployment.md](docs/deployment.md) for container, Kubernetes, and systemd deployment.

---

## Testing Commands

Ensure all tests pass before proposing code changes:

### Go (Core & SDK)
* **Run all Go tests**: `go test ./core/... ./sdk/go/... ./tests/... -v`
* **Run specific package test**: `go test -v ./core/tunnel/...`

### Python (SDK)
* **Run Python tests**: `pytest sdk/python/`

### Java (SDK)
* **Run Java tests**: `mvn test -pl sdk/java`

---

## Development & Code Style Guidelines

All code contributions must strictly adhere to the following rules:

### 1. Networking & Ports
* **Rule**: Extensions must **never** expose or bind to local TCP network ports in both dev and production.
* **Mechanism**: Extensions only dial outward to Core's gRPC port (`:9000` by default). All API traffic is routed back through the reverse gRPC tunnel.

### 2. Database (CMDS) Usage
* **Rule**: Extensions must **never** connect to PostgreSQL or other databases directly.
* **Mechanism**: Use the SDK Database Client (`sdk.DB` or `sdk.db`). Define collections and indexes declaratively in the extension's `manifest.yaml`. Core automatically manages table provisioning, index alignment, and schema migrations.

### 3. Storage & Files
* **Rule**: Uploads and downloads are managed entirely by Core. No binary file buffers should travel through the gRPC tunnel.
* **Upload**: Frontend uploads directly to `/api/system/files/upload`. Core stores it in RustFS (S3) and returns a `file_id` which the extension stores.
* **Download**: Extensions request a short-lived download token via `sdk.Files.GenerateDownloadURL`. Download URLs must strictly use clean path parameters: `/api/system/files/download/<file_id>/<temp_token>` (no query parameters like `?` are allowed).

### 4. Language Idioms & Code Conventions
* **Go**: Follow standard Go formatting (`go fmt`). Use type-safe SQL compiles generated by `sqlc`. Maintain migrations in `go-migrate` format.
* **Python**: Format code using PEP8 rules. Use type hints and standard Pydantic models for request validation in FastAPI.
* **Java**: Write clean Spring Boot code. Manage dependencies strictly via Maven POMs. Use ThreadLocal variables for request scopes.
