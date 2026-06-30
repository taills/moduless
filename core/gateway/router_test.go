package gateway

import (
	"archive/zip"
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/taills/moduleless/core/tunnel"
)

func TestGatewayStaticFileCache(t *testing.T) {
	mgr := tunnel.NewTunnelManager()

	// Pre-populate dummy zip for test-ext.
	var zipBuf bytes.Buffer
	zw := zip.NewWriter(&zipBuf)
	f, _ := zw.Create("app.js")
	f.Write([]byte("console.log('test')"))
	zw.Close()

	mgr.SaveZipChunk("test-ext", zipBuf.Bytes())
	if err := mgr.ExtractZipCache("test-ext"); err != nil {
		t.Fatalf("ExtractZipCache failed: %v", err)
	}

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

func TestGatewayOfflineExtension(t *testing.T) {
	mgr := tunnel.NewTunnelManager()
	handler := NewGatewayHandler(mgr)

	req := httptest.NewRequest("GET", "/api/extensions/ghost/hello", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for offline extension, got %d", rr.Code)
	}
}
