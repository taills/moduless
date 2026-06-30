package extension

import (
	"context"
	"os"
	"testing"

	"github.com/taills/moduless/core/db"
	sqlc "github.com/taills/moduless/core/db/sqlc"
	"github.com/taills/moduless/core/tunnel"
	pb "github.com/taills/moduless/proto/tunnel"
	"google.golang.org/grpc"
)

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

	// A no-secret connection to an already-approved key is denied.
	mustSetStatus(t, q, "k1", StatusApproved)
	if res, _ = store.Authenticate(ctx, &pb.RegisterRequest{ExtensionKey: "k1"}); res.Action != tunnel.AuthDeny {
		t.Fatalf("expected deny for no-secret approved key, got %v", res.Action)
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
