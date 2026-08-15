package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSystemRoutesMatchInOrder(t *testing.T) {
	gw := NewGatewayHandler()

	var hit string
	gw.RegisterSystemRoute(func(p string) bool { return p == "/healthz" },
		func(w http.ResponseWriter, _ *http.Request) { hit = "healthz"; w.WriteHeader(200) })
	gw.RegisterSystemRoute(func(p string) bool { return p == "/api/system/thing" },
		func(w http.ResponseWriter, _ *http.Request) { hit = "thing"; w.WriteHeader(200) })

	tests := []struct {
		path string
		want string
	}{
		{path: "/healthz", want: "healthz"},
		{path: "/api/system/thing", want: "thing"},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			hit = ""
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, httptest.NewRequest("GET", tc.path, nil))
			if hit != tc.want {
				t.Errorf("routed to %q, want %q", hit, tc.want)
			}
		})
	}
}

// An unmatched API path must 404 rather than fall through to the SPA. Falling
// through would answer with index.html, turning a typo in an API path into a
// confusing "why is my API returning HTML".
func TestUnmatchedAPIPathDoesNotReachTheSPA(t *testing.T) {
	gw := NewGatewayHandler()
	gw.Host = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("<html>console</html>"))
	})

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest("GET", "/api/system/nonexistent", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec.Body.Len() > 0 && rec.Body.String()[0] == '<' {
		t.Error("an unmatched API path was answered with the SPA")
	}
}

func TestNonAPIPathReachesTheSPA(t *testing.T) {
	gw := NewGatewayHandler()
	gw.Host = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte("console"))
	})

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest("GET", "/system/plugins", nil))

	if rec.Code != 200 || rec.Body.String() != "console" {
		t.Errorf("status = %d body = %q; the console SPA should have served this", rec.Code, rec.Body.String())
	}
}

func TestWithoutAHostEverythingElseIs404(t *testing.T) {
	gw := NewGatewayHandler()

	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, httptest.NewRequest("GET", "/anything", nil))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
