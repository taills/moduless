package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/ty-lab/go-web-module/core/tunnel"
)

// DiagnosticsReport is one row of the developer diagnostics dashboard.
type DiagnosticsReport struct {
	ExtensionKey string `json:"extension_key"`
	LastPingTime string `json:"last_ping_time"`
	SecondsAgo   int64  `json:"seconds_since_ping"`
	Online       bool   `json:"online"`
}

// GetDiagnostics serves GET /api/system/diagnostics with live tunnel telemetry
// (connection health and last-heartbeat age) read from the tunnel manager.
func GetDiagnostics(mgr *tunnel.TunnelManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		reports := make([]DiagnosticsReport, 0)
		for key, lastPing := range mgr.ListTunnels() {
			ago := int64(now.Sub(lastPing).Seconds())
			reports = append(reports, DiagnosticsReport{
				ExtensionKey: key,
				LastPingTime: lastPing.Format(time.RFC3339),
				SecondsAgo:   ago,
				Online:       ago < 30,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reports)
	}
}
