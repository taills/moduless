# Build from the repository root:
#   docker build -f extension-example/go/Dockerfile -t go-example .
# The frontend dist is bundled in; on startup the SDK streams it to Core, which
# serves it at /extensions/go_example/. The container opens no port.

# 1. Build the micro-frontend.
FROM node:20-alpine AS frontend
WORKDIR /fe
COPY extension-example/go/frontend/package.json ./
RUN npm install --no-audit --no-fund
COPY extension-example/go/frontend/ ./
RUN npm run build

# 2. Build the Go backend (module rooted at the repo root).
FROM golang:1.25-alpine AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/extension ./extension-example/go/backend

# 3. Minimal runtime: backend binary + built frontend + manifest.
FROM alpine:3.20
WORKDIR /app
COPY --from=backend /out/extension /app/extension
COPY --from=frontend /fe/dist /app/frontend
COPY extension-example/go/manifest.yaml /app/manifest.yaml
ENV CORE_URL=core:9000 \
    FRONTEND_DIR=/app/frontend \
    MANIFEST_PATH=/app/manifest.yaml
ENTRYPOINT ["/app/extension"]
