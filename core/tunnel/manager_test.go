package tunnel

import (
	"testing"

	pb "github.com/taills/moduless/proto/tunnel"
)

func TestWeightedRoundRobinDistribution(t *testing.T) {
	m := NewTunnelManager()
	a := m.Register("ext", nil, &pb.RegisterRequest{ExtensionKey: "ext", Weight: 1})
	b := m.Register("ext", nil, &pb.RegisterRequest{ExtensionKey: "ext", Weight: 2})
	c := m.Register("ext", nil, &pb.RegisterRequest{ExtensionKey: "ext", Weight: 3})

	counts := map[string]int{}
	const n = 600 // multiple of total weight (6) -> exact proportions
	for i := 0; i < n; i++ {
		picked, ok := m.PickTunnel("ext")
		if !ok {
			t.Fatal("PickTunnel returned no replica")
		}
		counts[picked.InstanceID]++
	}

	if counts[a.InstanceID] != 100 || counts[b.InstanceID] != 200 || counts[c.InstanceID] != 300 {
		t.Fatalf("weighted distribution off, want 100/200/300 got %d/%d/%d",
			counts[a.InstanceID], counts[b.InstanceID], counts[c.InstanceID])
	}
}

func TestDefaultWeightIsOne(t *testing.T) {
	m := NewTunnelManager()
	a := m.Register("ext", nil, &pb.RegisterRequest{ExtensionKey: "ext"})            // no weight -> 1
	b := m.Register("ext", nil, &pb.RegisterRequest{ExtensionKey: "ext", Weight: 0}) // 0 -> 1
	if a.Weight != 1 || b.Weight != 1 {
		t.Fatalf("default weight should be 1, got %d/%d", a.Weight, b.Weight)
	}
}

func TestReplicaAddAndRemove(t *testing.T) {
	m := NewTunnelManager()
	a := m.Register("ext", nil, &pb.RegisterRequest{ExtensionKey: "ext"})
	b := m.Register("ext", nil, &pb.RegisterRequest{ExtensionKey: "ext"})

	if !m.HasTunnel("ext", a) || !m.HasTunnel("ext", b) {
		t.Fatal("both replicas should be registered")
	}

	if last := m.RemoveTunnel("ext", a); last {
		t.Fatal("removing one of two replicas is not the last")
	}
	if m.HasTunnel("ext", a) {
		t.Fatal("replica a should be gone")
	}
	// Routing should now always pick b.
	if p, ok := m.PickTunnel("ext"); !ok || p != b {
		t.Fatal("expected remaining replica b")
	}

	if last := m.RemoveTunnel("ext", b); !last {
		t.Fatal("removing the final replica should report lastGone")
	}
	if _, ok := m.PickTunnel("ext"); ok {
		t.Fatal("no replicas should remain")
	}
}
