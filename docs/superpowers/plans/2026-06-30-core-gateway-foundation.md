# Core Gateway Foundation & Go SDK Base Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the minimum viable Core gateway (gRPC tunnel server, HTTP reverse proxy, and in-memory static file cache) and the Go SDK base to run a local Go micro-frontend extension through the Core gateway.

**Architecture:** Core runs an HTTP server (gateway) and a gRPC server (tunnel manager). Go Extensions connect to Core via gRPC stream. HTTP requests for extensions are converted to protobuf, sent over gRPC, handled by Go SDK routing standard Gin, and returned. Front-end assets are zipped, uploaded on register, and served from Core's memory cache.

**Tech Stack:** Go 1.20+, gRPC, Protobuf, Gin.

## Global Constraints

- Go Version: 1.20+
- No listening ports on Extensions in production or development (except standard outgoing gRPC connection).
- Zero-dependency on external DB/MQ for Phase 1 (use mock/in-memory).

---

### Task 1: Protobuf Contract & Code Generation

Setup the gRPC and Protobuf contract for the HTTP-over-gRPC tunnel, database client, and event bus, and configure the code generation scripts.

**Files:**
- Create: `proto/tunnel.proto`
- Create: `scripts/gen-proto.sh`
- Create: `go.mod`

**Interfaces:**
- Produces: Protobuf message structures and gRPC service client/server stubs in Go.

- [ ] **Step 1: Create `go.mod` at the root**

```go
module github.com/ty-lab/moduleless

go 1.20

require (
	google.golang.org/grpc v1.55.0
	google.golang.org/protobuf v1.30.0
)
```

- [ ] **Step 2: Create the `proto/tunnel.proto` contract file**

```protobuf
syntax = "proto3";

package moduleless;

option go_package = "github.com/ty-lab/moduleless/proto/tunnel";

service ExtensionTunnel {
  rpc Connect(stream TunnelMessage) returns (stream TunnelMessage);
}

message TunnelMessage {
  string message_id = 1;
  oneof payload {
    RegisterRequest register_req = 2;
    FileChunk file_chunk = 3;
    RegisterComplete register_complete = 4;
    RegisterResponse register_resp = 5;
    HttpRequestChunk http_req_chunk = 6;
    HttpResponseChunk http_resp_chunk = 7;
    Ping ping = 8;
    Pong pong = 9;
  }
}

message RegisterRequest {
  string extension_key = 1;
  string version = 2;
  string display_name = 3;
  string menu_icon = 4;
  string menu_path = 5;
  uint64 zip_file_size = 6;
  string zip_sha256 = 7;
  bool is_dev = 8;
  string dev_frontend_url = 9;
}

message FileChunk {
  bytes content = 1;
  uint32 chunk_index = 2;
}

message RegisterComplete {}

message RegisterResponse {
  bool success = 1;
  string error_message = 2;
  bool skip_upload = 3;
}

message HttpRequestChunk {
  string stream_id = 1;
  bool is_first = 2;
  bool is_last = 3;
  string method = 4;
  string path = 5;
  string query = 6;
  map<string, string> headers = 7;
  bytes body_chunk = 8;
}

message HttpResponseChunk {
  string stream_id = 1;
  bool is_first = 2;
  bool is_last = 3;
  int32 status_code = 4;
  map<string, string> headers = 5;
  bytes body_chunk = 6;
}

message Ping { int64 timestamp = 1; }
message Pong { int64 timestamp = 1; }
```

- [ ] **Step 3: Create `scripts/gen-proto.sh` script to run protobuf compiler**

```bash
#!/bin/bash
mkdir -p proto/tunnel
protoc --go_out=. --go_opt=paths=import \
       --go-grpc_out=. --go-grpc_opt=paths=import \
       proto/tunnel.proto
```

- [ ] **Step 4: Execute code generation and verify output**

Run: `chmod +x scripts/gen-proto.sh && ./scripts/gen-proto.sh`
Expected: Success, files created at `proto/tunnel/tunnel.pb.go` and `proto/tunnel/tunnel_grpc.pb.go`.

- [ ] **Step 5: Commit**

```bash
git add go.mod proto/tunnel.proto scripts/gen-proto.sh proto/tunnel/
git commit -m "feat: setup protobuf contract and go stubs"
```

---

### Task 2: Core Tunnel Server & Connection Manager

Implement the gRPC server and connection manager in the Core that registers extensions, receives frontend files, handles heartbeats, and maintains connection streams.

**Files:**
- Create: `core/tunnel/manager.go`
- Create: `core/tunnel/server.go`
- Create: `core/tunnel/server_test.go`

**Interfaces:**
- Consumes: Protobuf stubs from `proto/tunnel`
- Produces: `TunnelManager` and `TunnelServer` struct.
  - `NewTunnelManager() *TunnelManager`
  - `manager.Register(key string, stream grpc.ServerStream)`
  - `manager.SendHttpRequest(key string, req *pb.HttpRequestChunk) (chan *pb.HttpResponseChunk, error)`

- [ ] **Step 1: Write `core/tunnel/manager.go` connection manager**

```go
package tunnel

import (
	"archive/zip"
	"bytes"
	"errors"
	"sync"
	"time"

	pb "github.com/ty-lab/moduleless/proto/tunnel"
)

type ActiveTunnel struct {
	ExtensionKey string
	Stream       pb.ExtensionTunnel_ConnectServer
	ResponseChans sync.Map // Map[stream_id]chan *pb.HttpResponseChunk
	LastPing     time.Time
}

type TunnelManager struct {
	mu            sync.RWMutex
	tunnels       map[string]*ActiveTunnel
	uiCache       map[string]map[string][]byte // ExtensionKey -> FilePath -> Content
	pendingZips   map[string]*bytes.Buffer
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels:     make(map[string]*ActiveTunnel),
		uiCache:     make(map[string]map[string][]byte),
		pendingZips: make(map[string]*bytes.Buffer),
	}
}

func (m *TunnelManager) Register(key string, stream pb.ExtensionTunnel_ConnectServer) *ActiveTunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &ActiveTunnel{
		ExtensionKey: key,
		Stream:       stream,
		LastPing:     time.Now(),
	}
	m.tunnels[key] = t
	return t
}

func (m *TunnelManager) Unregister(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tunnels, key)
	delete(m.uiCache, key)
	delete(m.pendingZips, key)
}

func (m *TunnelManager) GetTunnel(key string) (*ActiveTunnel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tunnels[key]
	return t, ok
}

func (m *TunnelManager) GetUiFile(key, path string) ([]byte, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	files, ok := m.uiCache[key]
	if !ok {
		return nil, false
	}
	content, ok := files[path]
	return content, ok
}

func (m *TunnelManager) SaveZipChunk(key string, chunk []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf, ok := m.pendingZips[key]
	if !ok {
		buf = new(bytes.Buffer)
		m.pendingZips[key] = buf
	}
	buf.Write(chunk)
}

func (m *TunnelManager) ExtractZipCache(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf, ok := m.pendingZips[key]
	if !ok {
		return errors.New("no zip data uploaded")
	}
	defer delete(m.pendingZips, key)

	r, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		return err
	}

	files := make(map[string][]byte)
	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		var content bytes.Buffer
		_, err = content.ReadFrom(rc)
		rc.Close()
		if err != nil {
			return err
		}
		files["/"+f.Name] = content.Bytes()
	}
	m.uiCache[key] = files
	return nil
}
```

- [ ] **Step 2: Write `core/tunnel/server.go` implementing gRPC Connection**

```go
package tunnel

import (
	"errors"
	"io"
	"log"
	"time"

	pb "github.com/ty-lab/moduleless/proto/tunnel"
	"google.golang.org/grpc"
)

type TunnelServer struct {
	pb.UnimplementedExtensionTunnelServer
	Manager *TunnelManager
}

func NewTunnelServer(m *TunnelManager) *TunnelServer {
	return &TunnelServer{Manager: m}
}

func (s *TunnelServer) Connect(stream pb.ExtensionTunnel_ConnectServer) error {
	var currentTunnel *ActiveTunnel
	var key string

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		switch payload := msg.Payload.(type) {
		case *pb.TunnelMessage_RegisterReq:
			key = payload.RegisterReq.ExtensionKey
			currentTunnel = s.Manager.Register(key, stream)
			
			if payload.RegisterReq.IsDev {
				// Dev mode skips zip files
				err := stream.Send(&pb.TunnelMessage{
					Payload: &pb.TunnelMessage_RegisterResp{
						RegisterResp: &pb.RegisterResponse{Success: true},
					},
				})
				if err != nil {
					return err
				}
			}

		case *pb.TunnelMessage_FileChunk:
			if key == "" {
				return errors.New("file chunk sent before registration")
			}
			s.Manager.SaveZipChunk(key, payload.FileChunk.Content)

		case *pb.TunnelMessage_RegisterComplete:
			if key == "" {
				return errors.New("complete sent before registration")
			}
			err := s.Manager.ExtractZipCache(key)
			var resp *pb.RegisterResponse
			if err != nil {
				resp = &pb.RegisterResponse{Success: false, ErrorMessage: err.Error()}
			} else {
				resp = &pb.RegisterResponse{Success: true}
			}
			err = stream.Send(&pb.TunnelMessage{
				Payload: &pb.TunnelMessage_RegisterResp{
					RegisterResp: resp,
				},
			})
			if err != nil {
				return err
			}

		case *pb.TunnelMessage_HttpRespChunk:
			if currentTunnel != nil {
				ch, ok := currentTunnel.ResponseChans.Load(payload.HttpRespChunk.StreamId)
				if ok {
					ch.(chan *pb.HttpResponseChunk) <- payload.HttpRespChunk
				}
			}

		case *pb.TunnelMessage_Ping:
			err := stream.Send(&pb.TunnelMessage{
				Payload: &pb.TunnelMessage_Pong{
					Pong: &pb.Pong{Timestamp: time.Now().UnixNano()},
				},
			})
			if err != nil {
				return err
			}
		}
	}

	if key != "" {
		// Clean up immediately or queue for graceful timeout (to be implemented)
		s.Manager.Unregister(key)
	}
	return nil
}
```

- [ ] **Step 3: Write tests in `core/tunnel/server_test.go`**

```go
package tunnel

import (
	"archive/zip"
	"bytes"
	"context"
	"net"
	"testing"
	"time"

	pb "github.com/ty-lab/moduleless/proto/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestRegisterAndUpload(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer lis.Close()

	mgr := NewTunnelManager()
	srv := grpc.NewServer()
	pb.RegisterExtensionTunnelServer(srv, NewTunnelServer(mgr))
	go srv.Serve(lis)
	defer srv.Stop()

	conn, err := grpc.Dial(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("did not connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewExtensionTunnelClient(conn)
	stream, err := client.Connect(context.Background())
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}

	// 1. Send Register
	err = stream.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_RegisterReq{
			RegisterReq: &pb.RegisterRequest{
				ExtensionKey: "test-ext",
				Version:      "1.0.0",
				IsDev:        false,
			},
		},
	})
	if err != nil {
		t.Fatalf("Send Register failed: %v", err)
	}

	// Create a dummy zip
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("index.html")
	f.Write([]byte("hello world"))
	zw.Close()

	// 2. Send Zip Chunks
	err = stream.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_FileChunk{
			FileChunk: &pb.FileChunk{Content: zipBuf.Bytes()},
		},
	})
	if err != nil {
		t.Fatalf("Send Chunk failed: %v", err)
	}

	// 3. Complete
	err = stream.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_RegisterComplete{
			RegisterComplete: &pb.RegisterComplete{},
		},
	})
	if err != nil {
		t.Fatalf("Send Complete failed: %v", err)
	}

	// 4. Expect Response
	msg, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv response failed: %v", err)
	}

	resp, ok := msg.Payload.(*pb.TunnelMessage_RegisterResp)
	if !ok || !resp.RegisterResp.Success {
		t.Fatalf("Expected registration success, got: %v", msg.Payload)
	}

	// Verify Cache
	content, ok := mgr.GetUiFile("test-ext", "/index.html")
	if !ok || string(content) != "hello world" {
		t.Fatalf("Expected cache file 'hello world', got: %s", string(content))
	}
}
```

- [ ] **Step 4: Run test to verify passes**

Run: `go test -v ./core/tunnel/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add core/tunnel/
git commit -m "feat: implement Core tunnel connection manager and server"
```

---

### Task 3: HTTP Gateway Router & Micro-frontend Cache Loader

Implement the HTTP reverse proxy in Core that serves static UI assets from the memory cache and translates API calls into gRPC tunnel streams.

**Files:**
- Create: `core/gateway/router.go`
- Create: `core/gateway/router_test.go`

**Interfaces:**
- Consumes: `TunnelManager` from `core/tunnel`
- Produces: `gatewayHandler` HTTP handler.
  - `NewGatewayHandler(mgr *tunnel.TunnelManager) http.Handler`

- [ ] **Step 1: Write `core/gateway/router.go`**

```go
package gateway

import (
	"context"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/ty-lab/moduleless/core/tunnel"
	pb "github.com/ty-lab/moduleless/proto/tunnel"
)

type GatewayHandler struct {
	Manager *tunnel.TunnelManager
}

func NewGatewayHandler(mgr *tunnel.TunnelManager) *GatewayHandler {
	return &GatewayHandler{Manager: mgr}
}

func (h *GatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Static asset routing (/extensions/<key>/...)
	if strings.HasPrefix(r.URL.Path, "/extensions/") {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/extensions/"), "/", 2)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		extKey := parts[0]
		filePath := "/" + parts[1]

		content, ok := h.Manager.GetUiFile(extKey, filePath)
		if !ok {
			http.NotFound(w, r)
			return
		}

		contentType := mime.TypeByExtension(filepath.Ext(filePath))
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.Write(content)
		return
	}

	// 2. API Proxy Routing (/api/extensions/<key>/...)
	if strings.HasPrefix(r.URL.Path, "/api/extensions/") {
		parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/extensions/"), "/", 2)
		if len(parts) < 2 {
			http.NotFound(w, r)
			return
		}
		extKey := parts[0]
		subPath := "/" + parts[1]

		activeTunnel, ok := h.Manager.GetTunnel(extKey)
		if !ok {
			http.Error(w, "extension offline", http.StatusBadGateway)
			return
		}

		streamID := time.Now().Format("20060102150405.000000")
		respChan := make(chan *pb.HttpResponseChunk, 20)
		activeTunnel.ResponseChans.Store(streamID, respChan)
		defer activeTunnel.ResponseChans.Delete(streamID)

		// Read Body Chunk
		body, _ := io.ReadAll(r.Body)
		headers := make(map[string]string)
		for k, v := range r.Header {
			if len(v) > 0 {
				headers[k] = v[0]
			}
		}

		// Inject mock auth context
		headers["X-User-Id"] = "10001"
		headers["X-User-Roles"] = "admin"

		// Send HttpRequest首包
		err := activeTunnel.Stream.Send(&pb.TunnelMessage{
			Payload: &pb.TunnelMessage_HttpReqChunk{
				HttpReqChunk: &pb.HttpRequestChunk{
					StreamId:  streamID,
					IsFirst:   true,
					IsLast:    true,
					Method:    r.Method,
					Path:      subPath,
					Query:     r.URL.RawQuery,
					Headers:   headers,
					BodyChunk: body,
				},
			},
		})
		if err != nil {
			http.Error(w, "tunnel write error", http.StatusInternalServerError)
			return
		}

		// Await Response首包 & Subsequent chunks
		var writerInitialized bool
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()

		for {
			select {
			case chunk := <-respChan:
				if chunk.IsFirst {
					w.WriteHeader(int(chunk.StatusCode))
					for k, v := range chunk.Headers {
						w.Header().Set(k, v)
					}
					writerInitialized = true
				}
				if !writerInitialized {
					http.Error(w, "invalid protocol order", http.StatusInternalServerError)
					return
				}
				w.Write(chunk.BodyChunk)
				if chunk.IsLast {
					return
				}
			case <-ctx.Done():
				http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
				return
			}
		}
	}

	http.NotFound(w, r)
}
```

- [ ] **Step 2: Write tests in `core/gateway/router_test.go`**

```go
package gateway

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ty-lab/moduleless/core/tunnel"
)

func TestGatewayStaticFileCache(t *testing.T) {
	mgr := tunnel.NewTunnelManager()
	
	// Pre-populate dummy zip for test-ext
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("app.js")
	f.Write([]byte("console.log('test')"))
	zw.Close()

	mgr.SaveZipChunk("test-ext", zipBuf.Bytes())
	mgr.ExtractZipCache("test-ext")

	handler := NewGatewayHandler(mgr)

	req := httptest.NewRequest("GET", "/extensions/test-ext/app.js", nil)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rr.Code)
	}
	if rr.Body.String() != "console.log('test')" {
		t.Errorf("expected console output, got %s", rr.Body.String())
	}
}
```

- [ ] **Step 3: Run test to verify passes**

Run: `go test -v ./core/gateway/...`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add core/gateway/
git commit -m "feat: implement core gateway HTTP reverse proxy routing"
```

---

### Task 4: Go Extension SDK Connection & HTTP Router Bridge

Implement the Go SDK that establishes the outgoing gRPC tunnel connection to the Core and acts as the bridge connecting incoming gRPC request streams to Go's standard `http.Handler` routing engine.

**Files:**
- Create: `sdk/go/sdk.go`
- Create: `sdk/go/bridge.go`
- Create: `sdk/go/bridge_test.go`

**Interfaces:**
- Consumes: Protobuf stubs from `proto/tunnel`
- Produces: `sdk` Go API.
  - `sdk.Start(handler http.Handler, config Config)`
  - `sdk.GetUser(ctx context.Context) *UserContext`

- [ ] **Step 1: Write user context extraction helpers in `sdk/go/sdk.go`**

```go
package sdk

import (
	"context"
	"net/http"
	"strings"
)

type UserContext struct {
	UserID      string
	Roles       []string
	Permissions []string
}

type contextKey string
const userContextKey contextKey = "user_info"

func GetUser(ctx context.Context) *UserContext {
	if val := ctx.Value(userContextKey); val != nil {
		if u, ok := val.(*UserContext); ok {
			return u
		}
	}
	return nil
}

type Config struct {
	ExtensionKey string
	CoreGrpcURL  string
	IsDev        bool
	DevFEUrl     string
}

type mockResponseWriter struct {
	headers    http.Header
	statusCode int
	body       *bytes.Buffer
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{
		headers:    make(http.Header),
		statusCode: http.StatusOK,
		body:       new(bytes.Buffer),
	}
}

func (w *mockResponseWriter) Header() http.Header { return w.headers }
func (w *mockResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *mockResponseWriter) WriteHeader(statusCode int) { w.statusCode = statusCode }
```

- [ ] **Step 2: Write tunnel client and router bridging inside `sdk/go/bridge.go`**

```go
package sdk

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	pb "github.com/ty-lab/moduleless/proto/tunnel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func Start(handler http.Handler, cfg Config) {
	conn, err := grpc.Dial(cfg.CoreGrpcURL, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("failed to dial Core gRPC: %v", err)
	}
	defer conn.Close()

	client := pb.NewExtensionTunnelClient(conn)
	
	// Keep trying to connect/reconnect
	for {
		stream, err := client.Connect(context.Background())
		if err != nil {
			log.Printf("tunnel connect failed: %v, retrying in 2s...", err)
			time.Sleep(2 * time.Second)
			continue
		}

		// Register
		err = stream.Send(&pb.TunnelMessage{
			Payload: &pb.TunnelMessage_RegisterReq{
				RegisterReq: &pb.RegisterRequest{
					ExtensionKey:     cfg.ExtensionKey,
					Version:          "1.0.0",
					IsDev:            cfg.IsDev,
					DevFrontendUrl:  cfg.DevFEUrl,
				},
			},
		})
		if err != nil {
			log.Printf("registration send failed: %v", err)
			continue
		}

		// Handle Incoming Tunnel Packets
		err = handleTunnel(stream, handler)
		if err != nil {
			log.Printf("tunnel connection lost: %v. Reconnecting...", err)
		}
	}
}

func handleTunnel(stream pb.ExtensionTunnel_ConnectServer, handler http.Handler) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}

		switch payload := msg.Payload.(type) {
		case *pb.TunnelMessage_RegisterResp:
			if !payload.RegisterResp.Success {
				log.Printf("registration rejected: %s", payload.RegisterResp.ErrorMessage)
				return io.EOF
			}
			log.Printf("registration success")

		case *pb.TunnelMessage_HttpReqChunk:
			go serveBridgedRequest(stream, payload.HttpReqChunk, handler)

		case *pb.TunnelMessage_Pong:
			// Heartbeat pong received
		}
	}
}

func serveBridgedRequest(stream pb.ExtensionTunnel_ConnectServer, reqChunk *pb.HttpRequestChunk, handler http.Handler) {
	// Reconstruct request
	req, _ := http.NewRequest(reqChunk.Method, reqChunk.Path+"?"+reqChunk.Query, bytes.NewReader(reqChunk.BodyChunk))
	for k, v := range reqChunk.Headers {
		req.Header.Set(k, v)
	}

	// Parse user headers and inject context
	userID := req.Header.Get("X-User-Id")
	if userID != "" {
		roles := strings.Split(req.Header.Get("X-User-Roles"), ",")
		permissions := strings.Split(req.Header.Get("X-User-Permissions"), ",")
		userCtx := &UserContext{
			UserID:      userID,
			Roles:       roles,
			Permissions: permissions,
		}
		ctx := context.WithValue(req.Context(), userContextKey, userCtx)
		req = req.WithContext(ctx)
	}

	w := newMockResponseWriter()
	handler.ServeHTTP(w, req)

	// Send Response chunk back
	headers := make(map[string]string)
	for k, v := range w.Header() {
		if len(v) > 0 {
			headers[k] = v[0]
		}
	}

	_ = stream.Send(&pb.TunnelMessage{
		Payload: &pb.TunnelMessage_HttpRespChunk{
			HttpRespChunk: &pb.HttpResponseChunk{
				StreamId:   reqChunk.StreamId,
				IsFirst:    true,
				IsLast:     true,
				StatusCode: int32(w.statusCode),
				Headers:    headers,
				BodyChunk:  w.body.Bytes(),
			},
		},
	})
}
```

- [ ] **Step 3: Write tests in `sdk/go/bridge_test.go`**

```go
package sdk

import (
	"context"
	"net/http"
	"testing"
)

func TestContextExtraction(t *testing.T) {
	req, _ := http.NewRequest("GET", "/test", nil)
	req.Header.Set("X-User-Id", "10001")
	req.Header.Set("X-User-Roles", "admin,user")

	// Manually inject using similar logic as bridge
	userCtx := &UserContext{
		UserID: req.Header.Get("X-User-Id"),
		Roles:  strings.Split(req.Header.Get("X-User-Roles"), ","),
	}
	ctx := context.WithValue(req.Context(), userContextKey, userCtx)

	user := GetUser(ctx)
	if user == nil || user.UserID != "10001" {
		t.Fatalf("expected user id 10001, got %v", user)
	}
	if len(user.Roles) != 2 || user.Roles[0] != "admin" {
		t.Fatalf("expected admin role, got %v", user.Roles)
	}
}
```

- [ ] **Step 4: Run test to verify passes**

Run: `go test -v ./sdk/go/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/go/
git commit -m "feat: implement Go SDK connection and HTTP server bridging"
```

---

### Task 5: End-to-End Integration Integration Test

Create an E2E test file linking the Core gateway, gRPC tunnel server, and Go SDK client together to verify full functionality.

**Files:**
- Create: `tests/integration_test.go`

**Interfaces:**
- Consumes: `core/tunnel`, `core/gateway`, `sdk/go`

- [ ] **Step 1: Write integration tests in `tests/integration_test.go`**

```go
package tests

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ty-lab/moduleless/core/gateway"
	"github.com/ty-lab/moduleless/core/tunnel"
	"github.com/ty-lab/moduleless/sdk/go"
	pb "github.com/ty-lab/moduleless/proto/tunnel"
	"google.golang.org/grpc"
)

func TestFullE2ETunnel(t *testing.T) {
	// 1. Start Core gRPC Server
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gRPC listen failed: %v", err)
	}
	defer lis.Close()

	mgr := tunnel.NewTunnelManager()
	grpcSrv := grpc.NewServer()
	pb.RegisterExtensionTunnelServer(grpcSrv, tunnel.NewTunnelServer(mgr))
	go grpcSrv.Serve(lis)
	defer grpcSrv.Stop()

	// 2. Start Go Extension SDK using mock router
	mux := http.NewServeMux()
	mux.HandleFunc("/hello", func(w http.ResponseWriter, r *http.Request) {
		u := sdk.GetUser(r.Context())
		if u == nil {
			http.Error(w, "missing auth", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"hello":"` + u.UserID + `"}`))
	})

	go sdk.Start(mux, sdk.Config{
		ExtensionKey: "my-test-app",
		CoreGrpcURL:  lis.Addr().String(),
		IsDev:        true,
	})

	// Wait for SDK to dial and register
	time.Sleep(200 * time.Millisecond)

	// 3. Test HTTP Gateway forwarding
	gatewayHandler := gateway.NewGatewayHandler(mgr)
	gatewayHttpServer := httptest.NewServer(gatewayHandler)
	defer gatewayHttpServer.Close()

	resp, err := http.Get(gatewayHttpServer.URL + "/api/extensions/my-test-app/hello")
	if err != nil {
		t.Fatalf("HTTP request to gateway failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"hello":"10001"}` {
		t.Fatalf("expected hello 10001 response, got %s", string(body))
	}
}
```

- [ ] **Step 2: Run all tests to verify integration**

Run: `go test -v ./tests/...`
Expected: PASS

- [ ] **Step 3: Commit**

```bash
git add tests/
git commit -m "test: add full core-to-sdk integration E2E test"
```
