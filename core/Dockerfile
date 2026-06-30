# Core Gateway image. Migrations are embedded in the binary (go:embed), so the
# image only ships the static binary. Build from the repo root:
#   docker build -f core/Dockerfile -t moduless-core .

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/core ./core

FROM alpine:3.20
RUN apk add --no-cache wget ca-certificates
WORKDIR /app
COPY --from=build /out/core /app/core
# HTTP gateway and gRPC tunnel.
EXPOSE 80 9000
ENTRYPOINT ["/app/core"]
