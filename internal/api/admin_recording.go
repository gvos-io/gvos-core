// Global recording opt-in toggle.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/fastvoip/gvos-core/internal/shared/events"
)

func (s *Server) handleRecordToggle(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "body required")
		return
	}
	before := s.recordEnabled
	s.recordEnabled = req.Enabled
	// Persist (PG mode) so restarts and new replicas load the toggle;
	// broadcast so existing peers follow instantly.
	if s.pg != nil {
		if err := s.pg.PutSetting(r.Context(), "recording", map[string]any{"enabled": req.Enabled}); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	s.audit(r, "toggle", "recording", "", map[string]any{"enabled": before}, map[string]any{"enabled": req.Enabled})
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "recording", "enabled": req.Enabled})
	writeJSON(w, 200, map[string]any{"enabled": s.recordEnabled})
}

// POST /v1/admin/blocklist {"kind","scope","pattern","is_regex"} -
// add a screening rule; DELETE semantics via {"remove":true}.
