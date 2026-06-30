# Deployment

Core is deployed as a **single instance**. For a mid-size team this is the
right trade-off: Core is a near-stateless gateway/tunnel forwarder, so
availability comes from a process supervisor restarting it plus the SDK's
forever-reconnect loop — not from multi-instance HA. Put your HA budget into
PostgreSQL and RustFS (the stateful parts), e.g. a managed/replicated database.

What Core already provides for resilience:

- `GET /healthz` — liveness/readiness probe (200 once the gateway serves).
- Graceful shutdown — on `SIGTERM`, Core stops accepting new work, drains
  in-flight HTTP requests, and calls `grpcSrv.GracefulStop()`.
- Extensions reconnect automatically: a Core restart drops the tunnels and the
  SDKs re-register within seconds.

## Docker Compose (local / single host)

```bash
docker compose up --build
# Core gateway: http://localhost:8080  (health: /healthz)
```

`restart: unless-stopped` plus the `/healthz` healthcheck cover crash recovery.

## Console & login

Core serves the **qiankun host app (console)** at the web root `/`. It is a Vue 3
master app: a login page, a sidebar menu built from `GET /api/system/ui/apps`,
and a qiankun container that loads each extension as a micro-frontend.

Authentication is real: `POST /api/system/auth/login` verifies credentials
against `system_users` (bcrypt) and issues an in-memory session token. The
gateway resolves the token (Authorization header or `moduless_token` cookie) and
injects the authenticated identity into extension requests; unauthenticated
`/api/extensions/*` calls are rejected with 401.

On first start with an empty `system_users` table, Core seeds a default admin:

- username `admin`, password `admin123` — override with `ADMIN_USERNAME` /
  `ADMIN_PASSWORD`. **Change the default before exposing Core.**

The console is bundled into the Core image (built from `core/frontend`) and
served from `HOST_FRONTEND_DIR` (default `/app/host` in the image). When the
build is absent, Core serves a placeholder page.

## Kubernetes (single replica)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: moduless-core
spec:
  replicas: 1
  selector:
    matchLabels: { app: moduless-core }
  template:
    metadata:
      labels: { app: moduless-core }
    spec:
      terminationGracePeriodSeconds: 30   # allow in-flight drain
      containers:
        - name: core
          image: moduless-core:latest
          ports:
            - { containerPort: 80 }
            - { containerPort: 9000 }
          env:
            - { name: DATABASE_URL, value: "postgres://…?sslmode=disable" }
          readinessProbe:
            httpGet: { path: /healthz, port: 80 }
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: 80 }
            periodSeconds: 10
```

## systemd (bare VM)

```ini
[Service]
ExecStart=/usr/local/bin/core
Environment=DATABASE_URL=postgres://…?sslmode=disable
Restart=always
RestartSec=2
KillSignal=SIGTERM
TimeoutStopSec=30
```

## Extension images

Each example bundles its built micro-frontend into the backend image; on startup
the SDK streams it to Core (set via `FRONTEND_DIR`), which serves it at
`/extensions/<key>/`. Extensions open no ports — they only dial `CORE_URL`
(default `core:9000`). Build individually with:

```bash
docker build -f extension-example/go/Dockerfile     -t go-example .
docker build -f extension-example/python/Dockerfile -t python-example .
docker build -f extension-example/java/Dockerfile   -t java-example .
```

## Scaling an extension (load balancing)

Run multiple replicas of one extension and Core load-balances API traffic across
them. Each replica dials Core and registers under the same extension key; Core
keeps the full replica set (not just the latest) and routes with **smooth
weighted round-robin**.

```bash
docker compose up -d --scale go-example=3
# 30 requests to /api/extensions/go_example/* spread ~10/10/10 across replicas
```

Set a per-replica `weight` in `manifest.yaml` (default 1) to send proportionally
more traffic to heavier replicas:

```yaml
key: go_example
weight: 2   # this build's replicas each get weight 2
```

`GET /api/system/diagnostics` lists every connected replica (instance id +
weight); `GET /api/system/ui/apps` reports each extension's `replicas` count.

Note: this is single-Core, in-process load balancing across an extension's
replicas — it is not Core HA (Core stays single-instance; see above). When the
routed replica dies, Core drops it and routes to the others.
