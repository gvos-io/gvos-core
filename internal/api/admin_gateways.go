// Outbound gateway directory and SIP registration mirror.
package api

import (
	"encoding/json"
	"net/http"

	"github.com/gvos-io/gvos-core/internal/customerdomain/gateway"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

func (s *Server) handleGatewaySet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || r.Method != http.MethodPut {
		httpError(w, 405, "PUT required")
		return
	}
	var req struct {
		Gateways []gateway.Gateway `json:"gateways"`
		RemoveID string            `json:"remove_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, err.Error())
		return
	}

	current := s.gws.Gateways()

	if req.RemoveID != "" {
		kept := make([]gateway.Gateway, 0, len(current))
		removed := false
		for _, g := range current {
			if g.ID == req.RemoveID {
				removed = true
				continue
			}
			kept = append(kept, g)
		}
		if !removed {
			httpError(w, 404, "gateway not found: "+req.RemoveID)
			return
		}
		if s.pg != nil {
			if err := s.pg.PutSetting(r.Context(), "gateways", kept); err != nil {
				httpError(w, 500, err.Error())
				return
			}
		}
		if _, err := s.gws.Reload(kept); err != nil {
			httpError(w, 500, err.Error())
			return
		}
		s.audit(r, "remove", "gateway", req.RemoveID, current, kept)
		s.emit(events.SubjectCfgReload, map[string]any{"entity": "gateway", "gateways": s.lcrGateways()})
		writeJSON(w, 200, map[string]any{"removed": req.RemoveID, "gateways": len(kept)})
		return
	}

	// Full-set replace: validate every entry before touching state.
	for _, g := range req.Gateways {
		if err := gateway.Validate(g); err != nil {
			httpError(w, 400, err.Error())
			return
		}
	}
	if s.pg != nil {
		if err := s.pg.PutSetting(r.Context(), "gateways", req.Gateways); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	if _, err := s.gws.Reload(req.Gateways); err != nil {
		httpError(w, 500, err.Error())
		return
	}
	s.audit(r, "replace", "gateway", "", current, req.Gateways)
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "gateway", "gateways": s.lcrGateways()})
	writeJSON(w, 200, map[string]any{"gateways": len(req.Gateways)})
}

// GET /v1/admin/gateways

func (s *Server) handleGatewayList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"gateways": s.gws.Gateways(), "count": s.gws.Count()})
}

// GET /v1/admin/registrations — the network-wide SIP registration
// mirror (fed by gvos.reg.event from the signaling nodes).

func (s *Server) handleRegList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{
		"registrations": s.regs.Entries(), "count": s.regs.Count(), "online": s.regs.Online(),
	})
}

// POST /v1/admin/stream-push {"call_id","direction":"uplink|downlink","audio":<base64 or raw>}
// Test hook: pushes telephony-side audio into a live call's media
// stream (drives ASR bridges without a real caller). Audio is base64
// to stay JSON-safe.


// handleGatewayDelete serves DELETE /v1/admin/gateways/{id}.
func (s *Server) handleGatewayDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	gws := s.gws.Gateways()
	kept := gws[:0:0]
	found := false
	for _, gw := range gws {
		if gw.ID == id {
			found = true
			continue
		}
		kept = append(kept, gw)
	}
	if !found {
		httpError(w, 404, "gateway not found: "+id)
		return
	}
	s.gws.Reload(kept)
	s.audit(r, "remove", "gateway", id, map[string]any{"removed": 1}, nil)
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "gateway", "gateways": s.lcrGateways()})
	writeJSON(w, 200, map[string]any{"removed": id})
}

// routeGatewaysLegacy: old verb route during the deprecation window.
func (s *Server) routeGatewaysLegacy(w http.ResponseWriter, r *http.Request) {
	s.routeGateways(w, r)
}
