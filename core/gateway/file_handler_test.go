package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDownloadRejectsQueryParams(t *testing.T) {
	h := &FileHandler{} // Storage/Queries unused on this branch.
	req := httptest.NewRequest("GET", "/api/system/files/download/abc/tok?x=1", nil)
	rr := httptest.NewRecorder()
	h.Download(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for query params, got %d", rr.Code)
	}
}

func TestDownloadRejectsMalformedPath(t *testing.T) {
	h := &FileHandler{}
	req := httptest.NewRequest("GET", "/api/system/files/download/only-one-part", nil)
	rr := httptest.NewRecorder()
	h.Download(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed path, got %d", rr.Code)
	}
}

func TestUploadRejectsNonPost(t *testing.T) {
	h := &FileHandler{}
	req := httptest.NewRequest("GET", "/api/system/files/upload", nil)
	rr := httptest.NewRecorder()
	h.Upload(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", rr.Code)
	}
}
