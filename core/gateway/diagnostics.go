package gateway

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/taills/moduless/core/tunnel"
)

// DiagnosticsReport is one row of the developer diagnostics dashboard — one per
// connected replica (an extension may have several).
type DiagnosticsReport struct {
	ExtensionKey string `json:"extension_key"`
	InstanceID   string `json:"instance_id"`
	Weight       int    `json:"weight"`
	LastPingTime string `json:"last_ping_time"`
	SecondsAgo   int64  `json:"seconds_since_ping"`
	Online       bool   `json:"online"`
}

// GetDiagnostics serves GET /api/system/diagnostics with live per-replica tunnel
// telemetry (connection health and last-heartbeat age) read from the manager.
func GetDiagnostics(mgr *tunnel.TunnelManager) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		reports := make([]DiagnosticsReport, 0)
		for _, rep := range mgr.ListReplicas() {
			ago := int64(now.Sub(rep.LastPing).Seconds())
			reports = append(reports, DiagnosticsReport{
				ExtensionKey: rep.Key,
				InstanceID:   rep.InstanceID,
				Weight:       rep.Weight,
				LastPingTime: rep.LastPing.Format(time.RFC3339),
				SecondsAgo:   ago,
				Online:       true,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reports)
	}
}
