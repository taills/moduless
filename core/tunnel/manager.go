package tunnel

import (
	"archive/zip"
	"bytes"
	"errors"
	"sync"
	"time"

	pb "github.com/taills/moduless/proto/tunnel"
)

// ActiveTunnel represents a single live extension connection.
type ActiveTunnel struct {
	ExtensionKey  string
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
type TunnelManager struct {
	mu          sync.RWMutex
	tunnels     map[string]*ActiveTunnel
	uiCache     map[string]map[string][]byte // ExtensionKey -> FilePath -> Content
	pendingZips map[string]*bytes.Buffer
	metadata    map[string]*pb.RegisterRequest // ExtensionKey -> latest register info
}

func NewTunnelManager() *TunnelManager {
	return &TunnelManager{
		tunnels:     make(map[string]*ActiveTunnel),
		uiCache:     make(map[string]map[string][]byte),
		pendingZips: make(map[string]*bytes.Buffer),
		metadata:    make(map[string]*pb.RegisterRequest),
	}
}

func (m *TunnelManager) Register(key string, stream pb.ExtensionTunnel_ConnectServer, meta *pb.RegisterRequest) *ActiveTunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &ActiveTunnel{
		ExtensionKey: key,
		Stream:       stream,
		LastPing:     time.Now(),
	}
	m.tunnels[key] = t
	if meta != nil {
		m.metadata[key] = meta
	}
	return t
}

func (m *TunnelManager) Unregister(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tunnels, key)
	delete(m.uiCache, key)
	delete(m.pendingZips, key)
	delete(m.metadata, key)
}

func (m *TunnelManager) GetTunnel(key string) (*ActiveTunnel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tunnels[key]
	return t, ok
}

// ListTunnels returns a snapshot of active tunnel keys with last-ping times.
func (m *TunnelManager) ListTunnels() map[string]time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]time.Time, len(m.tunnels))
	for k, t := range m.tunnels {
		out[k] = t.LastPing
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
}

// ListExtensions returns metadata for all currently-registered extensions.
func (m *TunnelManager) ListExtensions() []ExtensionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ExtensionInfo, 0, len(m.metadata))
	for key, meta := range m.metadata {
		// A live tunnel is kept only while the extension is connected (the
		// gracefulUnregister window drops stale ones), so its presence is the
		// online signal — extensions do not send periodic heartbeats yet.
		_, online := m.tunnels[key]
		info := ExtensionInfo{Key: key, Online: online}
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

// TouchPing updates the last heartbeat timestamp for an extension.
func (m *TunnelManager) TouchPing(key string) {
	m.mu.RLock()
	t, ok := m.tunnels[key]
	m.mu.RUnlock()
	if ok {
		t.LastPing = time.Now()
	}
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

// ExtractZipCache decompresses the accumulated zip into the in-memory cache.
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
