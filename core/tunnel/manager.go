package tunnel

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"sync"
	"time"

	pb "github.com/taills/moduless/proto/tunnel"
)

// ActiveTunnel represents a single live extension replica connection. Multiple
// replicas may share one ExtensionKey; the manager load-balances across them.
type ActiveTunnel struct {
	ExtensionKey  string
	InstanceID    string // unique per replica connection
	Weight        int    // load-balancing weight (>=1)
	currentWeight int    // smooth weighted round-robin state, guarded by manager.mu
	Stream        pb.ExtensionTunnel_ConnectServer
	ResponseChans sync.Map // Map[stream_id]chan *pb.HttpResponseChunk
	LastPing      time.Time
	sendMu        sync.Mutex // serialize Stream.Send across goroutines
}

// Send writes a message to the tunnel stream in a goroutine-safe manner.
// gRPC streams do not allow concurrent Send calls, so we serialize them.
func (t *ActiveTunnel) Send(msg *pb.TunnelMessage) error {
	t.sendMu.Lock()
	defer t.sendMu.Unlock()
	return t.Stream.Send(msg)
}

// TunnelManager tracks active tunnels and the in-memory micro-frontend cache.
// Each extension key maps to a set of replica tunnels, balanced by weight.
type TunnelManager struct {
	mu          sync.RWMutex
	tunnels     map[string][]*ActiveTunnel     // ExtensionKey -> replicas
	uiCache     map[string]map[string][]byte   // ExtensionKey -> FilePath -> Content
	pendingZips map[string]*bytes.Buffer       // InstanceID -> uploading zip buffer
	metadata    map[string]*pb.RegisterRequest // ExtensionKey -> latest register info
	instanceSeq uint64
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels:     make(map[string][]*ActiveTunnel),
		uiCache:     make(map[string]map[string][]byte),
		pendingZips: make(map[string]*bytes.Buffer),
		metadata:    make(map[string]*pb.RegisterRequest),
	}
}

// Register adds a replica for key and returns its tunnel. Replicas accumulate
// rather than overwrite, so multiple instances of an extension are balanced.
func (m *TunnelManager) Register(key string, stream pb.ExtensionTunnel_ConnectServer, meta *pb.RegisterRequest) *ActiveTunnel {
	m.mu.Lock()
	defer m.mu.Unlock()

	weight := 1
	if meta != nil && meta.Weight > 0 {
		weight = int(meta.Weight)
	}
	m.instanceSeq++
	t := &ActiveTunnel{
		ExtensionKey: key,
		InstanceID:   fmt.Sprintf("%s-%d", key, m.instanceSeq),
		Weight:       weight,
		Stream:       stream,
		LastPing:     time.Now(),
	}
	m.tunnels[key] = append(m.tunnels[key], t)
	if meta != nil {
		m.metadata[key] = meta
	}
	return t
}

// RemoveTunnel drops a single replica and reports whether that was the last
// replica for the key. When it was, the shared UI cache and metadata are
// cleared too (callers should also drop key-scoped state like UI slots).
func (m *TunnelManager) RemoveTunnel(key string, target *ActiveTunnel) (lastGone bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if target != nil {
		delete(m.pendingZips, target.InstanceID)
	}
	replicas := m.tunnels[key]
	kept := replicas[:0]
	for _, r := range replicas {
		if r != target {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		delete(m.tunnels, key)
		delete(m.uiCache, key)
		delete(m.metadata, key)
		return true
	}
	m.tunnels[key] = kept
	return false
}

// HasTunnel reports whether the given replica is still registered for key.
func (m *TunnelManager) HasTunnel(key string, target *ActiveTunnel) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.tunnels[key] {
		if r == target {
			return true
		}
	}
	return false
}

// GetTunnel returns the first replica for key (existence check / single-replica
// callers). Use PickTunnel for request routing.
func (m *TunnelManager) GetTunnel(key string) (*ActiveTunnel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	replicas := m.tunnels[key]
	if len(replicas) == 0 {
		return nil, false
	}
	return replicas[0], true
}

// PickTunnel selects a replica for key using smooth weighted round-robin
// (nginx's algorithm), so traffic is spread proportionally to each replica's
// weight while staying evenly interleaved.
func (m *TunnelManager) PickTunnel(key string) (*ActiveTunnel, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	replicas := m.tunnels[key]
	if len(replicas) == 0 {
		return nil, false
	}
	if len(replicas) == 1 {
		return replicas[0], true
	}

	total := 0
	var best *ActiveTunnel
	for _, t := range replicas {
		t.currentWeight += t.Weight
		total += t.Weight
		if best == nil || t.currentWeight > best.currentWeight {
			best = t
		}
	}
	best.currentWeight -= total
	return best, true
}

// ReplicaInfo describes one connected replica for diagnostics.
type ReplicaInfo struct {
	Key        string
	InstanceID string
	Weight     int
	LastPing   time.Time
}

// ListReplicas returns a snapshot of every connected replica.
func (m *TunnelManager) ListReplicas() []ReplicaInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ReplicaInfo, 0)
	for key, replicas := range m.tunnels {
		for _, t := range replicas {
			out = append(out, ReplicaInfo{
				Key:        key,
				InstanceID: t.InstanceID,
				Weight:     t.Weight,
				LastPing:   t.LastPing,
			})
		}
	}
	return out
}

// ExtensionInfo is the registration metadata the host app needs to build its
// menu and register qiankun micro-apps.
type ExtensionInfo struct {
	Key         string
	DisplayName string
	MenuIcon    string
	MenuPath    string
	Online      bool
	Replicas    int
}

// ListExtensions returns metadata for all currently-registered extensions.
func (m *TunnelManager) ListExtensions() []ExtensionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ExtensionInfo, 0, len(m.metadata))
	for key, meta := range m.metadata {
		// A live tunnel is kept only while a replica is connected (the
		// gracefulUnregister window drops stale ones), so a non-empty replica
		// set is the online signal — extensions do not heartbeat yet.
		replicas := len(m.tunnels[key])
		info := ExtensionInfo{Key: key, Online: replicas > 0, Replicas: replicas}
		if meta != nil {
			info.DisplayName = meta.DisplayName
			info.MenuIcon = meta.MenuIcon
			info.MenuPath = meta.MenuPath
		}
		out = append(out, info)
	}
	return out
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

// Touch updates the last heartbeat timestamp for a specific replica.
func (m *TunnelManager) Touch(t *ActiveTunnel) {
	if t == nil {
		return
	}
	m.mu.Lock()
	t.LastPing = time.Now()
	m.mu.Unlock()
}

// SaveZipChunk appends a frontend zip chunk for a specific replica. Buffers are
// keyed by replica so concurrent uploads from multiple replicas never interleave.
func (m *TunnelManager) SaveZipChunk(t *ActiveTunnel, chunk []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf, ok := m.pendingZips[t.InstanceID]
	if !ok {
		buf = new(bytes.Buffer)
		m.pendingZips[t.InstanceID] = buf
	}
	buf.Write(chunk)
}

// ExtractZipCache decompresses a replica's uploaded zip into the shared UI cache
// for its extension key.
func (m *TunnelManager) ExtractZipCache(t *ActiveTunnel) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf, ok := m.pendingZips[t.InstanceID]
	if !ok {
		return errors.New("no zip data uploaded")
	}
	defer delete(m.pendingZips, t.InstanceID)

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
	m.uiCache[t.ExtensionKey] = files
	return nil
}
