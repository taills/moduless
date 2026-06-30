# Core Gateway image. Migrations are embedded in the binary (go:embed). The
# qiankun host (master) app is built and bundled in; Core serves it at the web
# root via HOST_FRONTEND_DIR. Build from the repo root:
#   docker build -f core/Dockerfile -t moduless-core .

# 1. Build the qiankun host (master) app.
FROM node:20-alpine AS host
WORKDIR /host
COPY core/frontend/package.json ./
RUN npm install --no-audit --no-fund
COPY core/frontend/ ./
RUN npm run build

# 2. Build the Core binary.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/core ./core

# 3. Runtime: Core binary + bundled host app.
FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates
WORKDIR /app
COPY --from=build /out/core /app/core
COPY --from=host /host/dist /app/host
ENV HOST_FRONTEND_DIR=/app/host
# HTTP gateway and gRPC tunnel.
EXPOSE 80 9000
ENTRYPOINT ["/app/core"]
