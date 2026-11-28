// Outbound gateway directory and SIP registration mirror.
package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"

	"github.com/gvos-io/gvos-core/internal/customerdomain/gateway"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

func (s *Server) handleGatewaySet(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPut {
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

	// Full-set operations are read-modify-write; two concurrent
	// writers used to lose updates (PUT vs DELETE races).
	s.gwMu.Lock()
	defer s.gwMu.Unlock()

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
	gws := s.gws.Gateways()
	writeJSON(w, 200, map[string]any{"gateways": gws, "items": gws, "count": s.gws.Count()})
}

// GET /v1/admin/registrations — the network-wide SIP registration
// mirror (fed by gvos.reg.event from the signaling nodes).

func (s *Server) handleRegList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	regs := s.regs.Entries()
	// Stable order (Entries is a map walk): newest activity first.
	sort.Slice(regs, func(i, j int) bool { return regs[i].LastReg > regs[j].LastReg })
	limit, offset := listPage(r, len(regs))
	page := regs[offset : offset+limit]
	next := ""
	if offset+limit < len(regs) {
		next = strconv.Itoa(offset + limit)
	}
	writeJSON(w, 200, map[string]any{
		"registrations": page, "items": page, "count": len(page),
		"total": len(regs), "online": s.regs.Online(), "next_cursor": next,
	})
}

// listPage parses the shared ?limit=&cursor= paging pair (numeric
// offset cursor) for the small in-memory directories (registrations,
// DIDs). Defaults 50, cap 500; cursor past the end clamps to empty.
func listPage(r *http.Request, total int) (limit, offset int) {
	limit = 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	if c := r.URL.Query().Get("cursor"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 0 && n < total {
			offset = n
		} else if err == nil && n >= total {
			offset = total
		}
	}
	if offset+limit > total {
		limit = total - offset
	}
	return limit, offset
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
	s.gwMu.Lock()
	defer s.gwMu.Unlock()
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
	// Persist before flipping memory (same order as the PUT paths) —
	// a delete that skips PutSetting resurrects the gateway on the
	// next restart/replica reload.
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
	s.audit(r, "remove", "gateway", id, map[string]any{"removed": 1}, nil)
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "gateway", "gateways": s.lcrGateways()})
	writeJSON(w, 200, map[string]any{"removed": id})
}

// routeGatewaysLegacy: old verb route during the deprecation window.
func (s *Server) routeGatewaysLegacy(w http.ResponseWriter, r *http.Request) {
	s.routeGateways(w, r)
}

// GET /v1/admin/gateways/{id}/breaker — the breaker observation face
// (state, counters, cooldown deadline; healthy defaults when unseen).
func (s *Server) handleGatewayBreaker(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	found := false
	for _, g := range s.gws.Gateways() {
		if g.ID == id {
			found = true
			break
		}
	}
	if !found {
		httpErrorC(w, 404, "NOT_FOUND", "no gateway "+id)
		return
	}
	writeJSON(w, 200, map[string]any{"gateway_id": id, "breaker": s.gwBreaker.View(id)})
}

// POST /v1/admin/gateways/{id}/unlock — manual breaker reset (the
// original M12 ask; meaningful only now that the breaker exists).
func (s *Server) handleGatewayUnlock(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	id := r.PathValue("id")
	from, to, changed := s.gwBreaker.Unlock(id)
	if !changed {
		httpErrorC(w, 404, "NOT_FOUND", "gateway not tripped (nothing to unlock)")
		return
	}
	s.audit(r, "unlock", "gateway", id, map[string]any{"state": string(from)}, map[string]any{"state": string(to)})
	// The route set changed: broadcast (half-open probes resume).
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "gateway", "gateways": s.lcrGateways(),
	})
	writeJSON(w, 200, map[string]any{"gateway_id": id, "state": string(to)})
}
