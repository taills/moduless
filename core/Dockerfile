# Core Gateway image.
#
# Core listens on one port. Plugins are subprocesses it starts itself, so there
# is no second listener for them to dial into.
#
# Build from the repo root:
#   docker build -f core/Dockerfile -t moduless-core .

# 1. Build the console (qiankun host app).
FROM node:20-alpine AS console
WORKDIR /console
COPY core/frontend/package.json ./
RUN npm install --no-audit --no-fund
COPY core/frontend/ ./
RUN npm run build

# 2. Build the Core binary. Migrations are embedded via go:embed.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/core ./core

# 3. Runtime.
FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates
WORKDIR /app

COPY --from=build /out/core /app/core
COPY --from=console /console/dist /app/console
ENV HOST_FRONTEND_DIR=/app/console

# Plugin packages live here. Mount a volume or bake them into a derived image;
# Core scans this directory at startup and on demand.
#
# Note for whatever delivers a new package version: replace the binary
# (rename, or unlink then write) rather than copying over it. The previous
# version is still serving requests until the upgrade commits, and writing into
# a file that is currently executing corrupts that process.
ENV PLUGIN_DIR=/app/plugins
ENV PLUGIN_DATA_DIR=/app/plugin-data
RUN mkdir -p /app/plugins /app/plugin-data
VOLUME ["/app/plugins", "/app/plugin-data"]

EXPOSE 80
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -qO- http://localhost:80/healthz || exit 1

ENTRYPOINT ["/app/core"]
