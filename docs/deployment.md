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
