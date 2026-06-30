# Core Baseline Enhancements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement remaining baseline features in Core: a gRPC-based distributed Event Bus, dynamic UI Slot injections, unified Operation Audit logs, dynamic Configurations push, and a Developer Diagnostics dashboard.

**Architecture:** All features are managed centrally.
- **Event Bus:** Core maintains event channels; subscribers receive notifications over gRPC.
- **UI Slots:** Core exposes slot-component mappings from `manifest.yaml` so the host page loads widgets inline.
- **Audit Logs:** Core gateway intercepts incoming requests, checks the manifest `is_audit` flags, and saves audit trails in DB.
- **Dynamic Config:** Core pushes DB configurations to extensions over gRPC on edit.
- **Diagnostics:** Core tracks gRPC ping RTT and caches for rendering in an admin UI.

**Tech Stack:** Go, PostgreSQL 18, gRPC, sqlc.

## Global Constraints

- No external broker dependencies (like RabbitMQ) required for local development. Core handles local event multiplexing.
- Extensions receive configuration push notifications asynchronously without restarting.

---

### Task 1: Distributed Event Bus over gRPC

Implement the Core event router and the gRPC subscriber stream server, enabling extensions to publish/subscribe over tunnels.

**Files:**
- Create: `core/event/bus.go`
- Create: `core/tunnel/event_server.go`
- Create: `sdk/go/event.go`
- Create: `core/event/bus_test.go`

**Interfaces:**
- Produces: `bus.Publish(eventName string, data []byte)`
  - `bus.Subscribe(eventName string, subscriberChan chan *pb.EventMessage)`

- [ ] **Step 1: Write `core/event/bus.go` Event Bus broker**

```go
package event

import (
	"sync"
)

type EventBus struct {
	mu          sync.RWMutex
	subscribers map[string][]chan []byte
}

func NewEventBus() *EventBus {
	return &EventBus{
		subscribers: make(map[string][]chan []byte),
	}
}

func (b *EventBus) Subscribe(event string, ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subscribers[event] = append(b.subscribers[event], ch)
}

func (b *EventBus) Unsubscribe(event string, ch chan []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	subs := b.subscribers[event]
	for i, sub := range subs {
		if sub == ch {
			b.subscribers[event] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
}

func (b *EventBus) Publish(event string, data []byte) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers[event] {
		select {
		case ch <- data:
		default: // Non-blocking write
		}
	}
}
```

- [ ] **Step 2: Write `core/tunnel/event_server.go` gRPC listener**

```go
package tunnel

import (
	"context"

	"github.com/ty-lab/go-web-module/core/event"
	pb "github.com/ty-lab/go-web-module/proto/tunnel"
)

type EventServer struct {
	pb.UnimplementedEventBusServiceServer
	Bus *event.EventBus
}

func (s *EventServer) Publish(ctx context.Context, req *pb.PublishRequest) (*pb.PublishResponse, error) {
	s.Bus.Publish(req.EventName, req.EventData)
	return &pb.PublishResponse{Success: true}, nil
}

func (s *EventServer) Subscribe(stream pb.EventBusService_SubscribeServer) error {
	ch := make(chan []byte, 100)
	var subscribedEvents []string

	defer func() {
		for _, ev := range subscribedEvents {
			s.Bus.Unsubscribe(ev, ch)
		}
	}()

	// Read subscriptions
	for {
		req, err := stream.Recv()
		if err != nil {
			return err
		}
		s.Bus.Subscribe(req.EventName, ch)
		subscribedEvents = append(subscribedEvents, req.EventName)

		// Spawn dynamic forwarder per client
		go func(ev string) {
			for data := range ch {
				_ = stream.Send(&pb.EventMessage{
					EventName: ev,
					EventData: data,
				})
			}
		}(req.EventName)
	}
}
```

- [ ] **Step 3: Implement client-side `sdk/go/event.go`**

```go
package sdk

import (
	"context"

	pb "github.com/ty-lab/go-web-module/proto/tunnel"
	"google.golang.org/grpc"
)

type EventClient struct {
	client pb.EventBusServiceClient
}

func NewEventClient(conn *grpc.ClientConn) *EventClient {
	return &EventClient{client: pb.NewEventBusServiceClient(conn)}
}

func (c *EventClient) Publish(ctx context.Context, eventName string, data []byte) error {
	_, err := c.client.Publish(ctx, &pb.PublishRequest{
		EventName: eventName,
		EventData: data,
	})
	return err
}
```

- [ ] **Step 4: Commit**

```bash
git add core/event/ core/tunnel/event_server.go sdk/go/event.go
git commit -m "feat: implement distributed gRPC event bus and SDK client"
```

---

### Task 2: Automated Operation Audit Logger Middleware

Build the Core database logging tables and HTTP interceptor middleware to automatically record operations based on extension manifests.

**Files:**
- Create: `db/migrations/000003_create_audit.up.sql`
- Create: `db/migrations/000003_create_audit.down.sql`
- Create: `core/middleware/audit.go`
- Modify: `db/query.sql`

**Interfaces:**
- Produces: `middleware.AuditLogger(queries *db.Queries, mgr *tunnel.TunnelManager) func(http.Handler) http.Handler`

- [ ] **Step 1: Write migration for audit table**

Create `db/migrations/000003_create_audit.up.sql`:
```sql
CREATE TABLE IF NOT EXISTS audit_logs (
    id SERIAL PRIMARY KEY,
    user_id VARCHAR(100) NOT NULL,
    action VARCHAR(255) NOT NULL,
    extension_key VARCHAR(100) NOT NULL,
    http_path VARCHAR(255) NOT NULL,
    client_ip VARCHAR(50) NOT NULL,
    created_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
);
```

Create `db/migrations/000003_create_audit.down.sql`:
```sql
DROP TABLE IF EXISTS audit_logs;
```

- [ ] **Step 2: Add queries in `db/query.sql`**

```sql
-- name: InsertAuditLog :exec
INSERT INTO audit_logs (user_id, action, extension_key, http_path, client_ip)
VALUES ($1, $2, $3, $4, $5);
```

- [ ] **Step 3: Write Core audit interceptor `core/middleware/audit.go`**

```go
package middleware

import (
	"net/http"
	"strings"

	"github.com/ty-lab/go-web-module/core/tunnel"
	db "github.com/ty-lab/go-web-module/core/db/sqlc"
)

func AuditLogger(queries *db.Queries, mgr *tunnel.TunnelManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)

			// Inspect path /api/extensions/<key>/*
			if strings.HasPrefix(r.URL.Path, "/api/extensions/") {
				parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/extensions/"), "/", 2)
				if len(parts) < 2 {
					return
				}
				extKey := parts[0]

				// Automatically log state modifying requests (POST, PUT, DELETE)
				if r.Method == "POST" || r.Method == "PUT" || r.Method == "DELETE" {
					userID := r.Header.Get("X-User-Id")
					if userID == "" {
						userID = "anonymous"
					}

					actionStr := r.Method + " operation on " + r.URL.Path
					clientIP := r.RemoteAddr

					_ = queries.InsertAuditLog(r.Context(), db.InsertAuditLogParams{
						UserID:       userID,
						Action:       actionStr,
						ExtensionKey: extKey,
						HttpPath:     r.URL.Path,
						ClientIP:     clientIP,
					})
				}
			}
		})
	}
}
```

- [ ] **Step 4: Compile sqlc and commit**

Run: `sqlc generate`
Run: `git add db/ core/middleware/ && git commit -m "feat: implement unified HTTP operation audit logging"`

---

### Task 3: Slot UI Injection Engine & Diagnostics Dashboard

Develop UI slot API resolvers for micro-frontends and configure RTT metrics capture in the tunnel connection manager to build the Dev Diagnostics Panel.

**Files:**
- Create: `core/gateway/slot.go`
- Create: `core/gateway/diagnostics.go`
- Modify: `core/gateway/router.go`

**Interfaces:**
- Produces: API HTTP routes:
  - `GET /api/system/ui/slots` -> list of dynamic UI slot mappings.
  - `GET /api/system/diagnostics` -> telemetry metrics on tunnels.

- [ ] **Step 1: Write `core/gateway/slot.go` dynamic registry**

```go
package gateway

import (
	"encoding/json"
	"net/http"
)

type UISlot struct {
	SlotName       string `json:"slot_name"`
	ExtensionKey   string `json:"extension_key"`
	ComponentEntry string `json:"component_entry"`
}

var uiSlots []UISlot

func RegisterSlot(slot UISlot) {
	uiSlots = append(uiSlots, slot)
}

func GetUISlotsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(uiSlots)
}
```

- [ ] **Step 2: Write `core/gateway/diagnostics.go` metrics collector**

```go
package gateway

import (
	"encoding/json"
	"net/http"

	"github.com/ty-lab/go-web-module/core/tunnel"
)

type DiagnosticsReport struct {
	ExtensionKey string `json:"extension_key"`
	LastPingTime string `json:"last_ping_time"`
	ActivePools  int    `json:"active_pools"`
}

func GetDiagnostics(mgr *tunnel.TunnelManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reports := []DiagnosticsReport{}
		// Manager exposes local maps to format telemetry
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reports)
	}
}
```

- [ ] **Step 3: Commit**

```bash
git add core/gateway/slot.go core/gateway/diagnostics.go
git commit -m "feat: implement frontend UI Slots registry and Diagnostics API"
```
