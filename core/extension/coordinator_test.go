package extension

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/taills/moduless/core/db"
	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
	"google.golang.org/grpc"
)

// jsonUnmarshal is a thin alias so test bodies stay short.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// fakeStream is a minimal ExtensionTunnel_ConnectServer that records sent
// messages so approval pushes can be asserted.
type fakeStream struct {
	grpc.ServerStream
	sent []*pb.TunnelMessage
}

func (f *fakeStream) Send(m *pb.TunnelMessage) error { f.sent = append(f.sent, m); return nil }
func (f *fakeStream) Recv() (*pb.TunnelMessage, error) {
	return nil, context.Canceled
}
func (f *fakeStream) Context() context.Context { return context.Background() }

func testQueries(t *testing.T) *sqlc.Queries {
	t.Helper()
	connStr := os.Getenv("TEST_DATABASE_URL")
	if connStr == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping extension registry integration test")
	}
	conn, err := db.InitDB(connStr) // runs migrations
	if err != nil {
		t.Skipf("cannot init test database: %v", err)
	}
	if _, err := conn.Exec("TRUNCATE extension_secrets, extensions RESTART IDENTITY CASCADE"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return sqlc.New(conn)
}

func TestAuthenticateLifecycle(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()
	store := NewStore(q)

	// First contact without a secret → pending, recording a registry row.
	res, err := store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k1", DisplayName: "K1", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	if res.Action != tunnel.AuthPending {
		t.Fatalf("expected pending, got %v", res.Action)
	}
	ext, err := q.GetExtension(ctx, "k1")
	if err != nil || ext.Status != StatusPending {
		t.Fatalf("expected pending row, got %+v err=%v", ext, err)
	}

	// A no-secret connection to an already-approved key is parked as a pending
	// instance for re-approval (e.g. a restarted replica that lost its secret),
	// not hard-denied — the approved registry row is left intact.
	mustSetStatus(t, q, "k1", StatusApproved)
	if res, _ = store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k1"}); res.Action != tunnel.AuthPending {
		t.Fatalf("expected pending for no-secret approved key, got %v", res.Action)
	}
	// The approved row must not be downgraded by the no-secret dial.
	if ext, err := q.GetExtension(ctx, "k1"); err != nil || ext.Status != StatusApproved {
		t.Fatalf("no-secret dial must not change approved status, got %+v err=%v", ext, err)
	}

	// Mint a secret; presenting it authenticates, a wrong one is denied.
	secret, err := store.mintSecret(ctx, "k1", "inst-1")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if res, _ = store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k1", ExtensionSecret: secret}); res.Action != tunnel.AuthApprove {
		t.Fatalf("expected approve with valid secret, got %v", res.Action)
	}
	if res, _ = store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k1", ExtensionSecret: "ext_bogus"}); res.Action != tunnel.AuthDeny {
		t.Fatalf("expected deny for bad secret, got %v", res.Action)
	}

	// Rejected key → reject decision on a no-secret contact.
	mustSetStatus(t, q, "k1", StatusRejected)
	if res, _ = store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k1"}); res.Action != tunnel.AuthReject {
		t.Fatalf("expected reject, got %v", res.Action)
	}
}

func TestCoordinatorApproveActivatesPending(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()
	store := NewStore(q)
	mgr := tunnel.NewTunnelManager()
	provisioned := 0
	coord := &Coordinator{
		Store:     store,
		Manager:   mgr,
		Provision: func(*pb.RegisterRequest) error { provisioned++; return nil },
	}

	meta := &pb.RegisterRequest{ExtensionKey: "k2", DisplayName: "K2", IsDev: true}
	if err := store.recordPending(ctx, meta); err != nil {
		t.Fatalf("record pending: %v", err)
	}
	fs := &fakeStream{}
	mgr.AddPending("k2", fs, meta)

	issued, err := coord.Approve(ctx, "k2")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if len(issued) != 1 || issued[0].Secret == "" {
		t.Fatalf("expected one issued secret, got %+v", issued)
	}
	if provisioned != 1 {
		t.Fatalf("expected provision to run once, got %d", provisioned)
	}
	if mgr.CountReplicas("k2") != 1 {
		t.Fatalf("expected pending tunnel promoted to routable")
	}
	foundSecret := ""
	for _, m := range fs.sent {
		if d := m.GetRegisterDecision(); d != nil && d.Status == "approved" {
			foundSecret = d.IssuedSecret
		}
	}
	if foundSecret != issued[0].Secret {
		t.Fatalf("approved decision did not carry the issued secret")
	}

	// The issued secret authenticates a reconnect...
	if res, _ := store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k2", ExtensionSecret: issued[0].Secret}); res.Action != tunnel.AuthApprove {
		t.Fatalf("issued secret should authenticate, got %v", res.Action)
	}
	// ...until Reject revokes it.
	if err := coord.Reject(ctx, "k2"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if res, _ := store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k2", ExtensionSecret: issued[0].Secret}); res.Action != tunnel.AuthDeny {
		t.Fatalf("revoked secret should be denied, got %v", res.Action)
	}
}

func mustSetStatus(t *testing.T, q *sqlc.Queries, key, status string) {
	t.Helper()
	if err := q.SetExtensionStatus(context.Background(), sqlc.SetExtensionStatusParams{Key: key, Status: status}); err != nil {
		t.Fatalf("set status %s: %v", status, err)
	}
}

func TestCoordinatorSecretAndListLifecycle(t *testing.T) {
	q := testQueries(t)
	ctx := context.Background()
	store := NewStore(q)
	mgr := tunnel.NewTunnelManager()
	coord := &Coordinator{Store: store, Manager: mgr, Provision: func(*pb.RegisterRequest) error { return nil }}

	// Generating a secret before approval is refused.
	if err := store.recordPending(ctx, &pb.RegisterRequest{ExtensionKey: "k3", DisplayName: "K3"}); err != nil {
		t.Fatalf("record pending: %v", err)
	}
	if _, err := coord.GenerateSecret(ctx, "k3", "extra"); err == nil {
		t.Fatal("expected error generating secret for unapproved extension")
	}

	// Approve (no pending tunnels → status flips, no secrets minted yet).
	if _, err := coord.Approve(ctx, "k3"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	// Now an operator can pre-mint a secret for an extra replica.
	secret, err := coord.GenerateSecret(ctx, "k3", "replica-2")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if res, _ := store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k3", ExtensionSecret: secret}); res.Action != tunnel.AuthApprove {
		t.Fatalf("pre-minted secret should authenticate, got %v", res.Action)
	}

	// List reports the approved extension.
	views, err := coord.List(ctx)
	if err != nil || len(views) != 1 || views[0].Status != StatusApproved {
		t.Fatalf("unexpected list: %+v err=%v", views, err)
	}

	// ListSecrets shows the active secret; revoking it denies authentication.
	secrets, err := coord.ListSecrets(ctx, "k3")
	if err != nil || len(secrets) != 1 {
		t.Fatalf("expected 1 secret, got %+v err=%v", secrets, err)
	}
	if err := coord.RevokeSecret(ctx, "k3", secrets[0].ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if res, _ := store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k3", ExtensionSecret: secret}); res.Action != tunnel.AuthDeny {
		t.Fatalf("revoked secret should be denied, got %v", res.Action)
	}

	// Delete removes the extension; a later contact is treated as fresh pending.
	if err := coord.Delete(ctx, "k3"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := q.GetExtension(ctx, "k3"); err == nil {
		t.Fatal("expected extension row gone after delete")
	}
	if res, _ := store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k3", DisplayName: "K3"}); res.Action != tunnel.AuthPending {
		t.Fatalf("post-delete contact should be pending, got %v", res.Action)
	}
}

// TestAggregateReplicas covers the per-replica → (lastPing, weight, summary)
// rollup used by Coordinator.List. It does not need a DB.
func TestAggregateReplicas(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-5 * time.Second)
	stale := now.Add(-45 * time.Second)

	cases := []struct {
		name            string
		replicas        []tunnel.ReplicaInfo
		wantPingNil     bool
		wantPingAt      time.Time // when wantPingNil is false, expected Newest
		wantWeight      int
		wantSummaries   int
		wantOnlineCount int
	}{
		{
			name:        "nil replicas → all zero",
			replicas:    nil,
			wantPingNil: true,
			wantWeight:  0,
		},
		{
			name: "single replica → reflects that replica",
			replicas: []tunnel.ReplicaInfo{
				{Key: "k", InstanceID: "k-1", Weight: 3, LastPing: fresh},
			},
			wantPingAt:      fresh,
			wantWeight:      3,
			wantSummaries:   1,
			wantOnlineCount: 1,
		},
		{
			name: "multiple replicas → newest ping, sum weight, all online flags",
			replicas: []tunnel.ReplicaInfo{
				{Key: "k", InstanceID: "k-1", Weight: 2, LastPing: now.Add(-10 * time.Second)},
				{Key: "k", InstanceID: "k-2", Weight: 5, LastPing: now.Add(-2 * time.Second)},
				{Key: "k", InstanceID: "k-3", Weight: 1, LastPing: now.Add(-20 * time.Second)},
			},
			wantPingAt:      now.Add(-2 * time.Second),
			wantWeight:      8,
			wantSummaries:   3,
			wantOnlineCount: 3,
		},
		{
			name: "stale replica → Online=false, but still counted in weight/summary",
			replicas: []tunnel.ReplicaInfo{
				{Key: "k", InstanceID: "k-1", Weight: 4, LastPing: fresh},
				{Key: "k", InstanceID: "k-2", Weight: 7, LastPing: stale},
			},
			wantPingAt:      fresh,
			wantWeight:      11,
			wantSummaries:   2,
			wantOnlineCount: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotPing, gotWeight, gotSummaries := aggregateReplicas(tc.replicas)
			if tc.wantPingNil {
				if gotPing != nil {
					t.Fatalf("want ping nil, got %v", *gotPing)
				}
			} else {
				if gotPing == nil {
					t.Fatalf("want ping at %v, got nil", tc.wantPingAt)
				}
				if !gotPing.Equal(tc.wantPingAt) {
					t.Fatalf("want ping %v, got %v", tc.wantPingAt, *gotPing)
				}
			}
			if gotWeight != tc.wantWeight {
				t.Fatalf("weight: want %d, got %d", tc.wantWeight, gotWeight)
			}
			if len(gotSummaries) != tc.wantSummaries {
				t.Fatalf("summaries: want %d, got %d", tc.wantSummaries, len(gotSummaries))
			}
			online := 0
			for _, s := range gotSummaries {
				if s.Online {
					online++
				}
			}
			if online != tc.wantOnlineCount {
				t.Fatalf("online count: want %d, got %d", tc.wantOnlineCount, online)
			}
		})
	}
}

// TestEncodeMenus covers the JSONB serialization for the extensions.menus
// column: proto menus take precedence, legacy icon/path fields fall back to a
// single-node tree, and the truly-empty case returns "[]".
func TestEncodeMenus(t *testing.T) {
	t.Run("empty request → empty array", func(t *testing.T) {
		got := encodeMenus(&pb.RegisterRequest{})
		if string(got) != "[]" {
			t.Fatalf("want [], got %s", got)
		}
	})

	t.Run("legacy icon+path → one-node tree", func(t *testing.T) {
		got := encodeMenus(&pb.RegisterRequest{DisplayName: "L", MenuPath: "/legacy", MenuIcon: "gear"})
		if string(got) == "[]" {
			t.Fatalf("expected non-empty tree, got []")
		}
		var arr []map[string]any
		if err := jsonUnmarshal(got, &arr); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(arr) != 1 || arr[0]["path"] != "/legacy" || arr[0]["icon"] != "gear" || arr[0]["title"] != "L" {
			t.Fatalf("unexpected tree: %+v", arr)
		}
	})

	t.Run("proto menus preferred over legacy fields", func(t *testing.T) {
		req := &pb.RegisterRequest{
			DisplayName: "P",
			MenuPath:    "/legacy",
			MenuIcon:    "old",
			Menus: []*pb.MenuItem{
				{Path: "/new", Title: "New", Order: 1},
			},
		}
		got := encodeMenus(req)
		var arr []map[string]any
		if err := jsonUnmarshal(got, &arr); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(arr) != 1 || arr[0]["path"] != "/new" {
			t.Fatalf("expected /new path, got %+v", arr)
		}
	})

	t.Run("proto menus with children serialize the tree", func(t *testing.T) {
		req := &pb.RegisterRequest{
			Menus: []*pb.MenuItem{
				{Path: "/system", Title: "System", Children: []*pb.MenuItem{
					{Path: "/system/a", Title: "A"},
				}},
			},
		}
		got := encodeMenus(req)
		var arr []map[string]any
		if err := jsonUnmarshal(got, &arr); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(arr) != 1 {
			t.Fatalf("want 1 root, got %d", len(arr))
		}
		children, _ := arr[0]["children"].([]any)
		if len(children) != 1 {
			t.Fatalf("want 1 child, got %d", len(children))
		}
	})
}
