// Edge policy administration: blocklist, risk rules, IP allowlist.
package api

import (
	"strings"
	"encoding/json"
	"net/http"

	"github.com/lixuanqun/gvos-core/internal/customerdomain/blocklist"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/ipallow"
	"github.com/lixuanqun/gvos-core/internal/customerdomain/riskctrl"
	"github.com/lixuanqun/gvos-core/internal/shared/events"
)

func (s *Server) emitListOp(op string, e blocklist.Entry) {
	s.emit(events.SubjectCfgReload, map[string]any{
		"entity": "blocklist", "op": op,
		"entry": map[string]any{
			"tenant_id": e.TenantID, "kind": e.Kind, "scope": e.Scope,
			"pattern": e.Pattern, "is_regex": e.IsRegex,
		},
	})
}

// POST /v1/admin/record {"enabled":bool} - global recording toggle.
// Kamailio reads the start-event answer's record flag and engages
// rtpengine recording per call.

func (s *Server) handleBlocklist(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Kind    string `json:"kind"`  // black | white
			Scope   string `json:"scope"` // src | dest
			Pattern string `json:"pattern"`
			IsRegex bool   `json:"is_regex"`
			Remove  bool   `json:"remove"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
			(req.Kind != "black" && req.Kind != "white") ||
			(req.Scope != "src" && req.Scope != "dest") || req.Pattern == "" {
			httpError(w, 400, "kind(black|white), scope(src|dest), pattern required")
			return
		}
		if req.Remove {
			// Write-through (PG mode) before the memory remove: the
			// DB row is what restarts and new replicas load back.
			if s.pg != nil {
				if _, err := s.pg.RemoveNumberList(r.Context(), "", req.Kind, req.Scope, req.Pattern); err != nil {
					httpError(w, 500, err.Error())
					return
				}
			}
			n := s.blocklist.Remove("", req.Kind, req.Scope, req.Pattern)
			s.audit(r, "remove", "blocklist", req.Pattern, map[string]any{"removed": n}, nil)
			s.emitListOp("remove", blocklist.Entry{Kind: req.Kind, Scope: req.Scope, Pattern: req.Pattern, IsRegex: req.IsRegex})
			writeJSON(w, 200, map[string]any{"removed": n})
			return
		}
		if s.pg != nil {
			if err := s.pg.UpsertNumberList(r.Context(), blocklist.Entry{Kind: req.Kind, Scope: req.Scope, Pattern: req.Pattern, IsRegex: req.IsRegex}); err != nil {
				httpError(w, 500, err.Error())
				return
			}
		}
		s.blocklist.Add(blocklist.Entry{
			Kind: req.Kind, Scope: req.Scope, Pattern: req.Pattern, IsRegex: req.IsRegex,
		})
		s.audit(r, "create", "blocklist", req.Pattern, nil, req)
		s.emitListOp("add", blocklist.Entry{Kind: req.Kind, Scope: req.Scope, Pattern: req.Pattern, IsRegex: req.IsRegex})
		writeJSON(w, 201, map[string]any{"kind": req.Kind, "scope": req.Scope, "pattern": req.Pattern})
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"rules": s.blocklist.Count()})
	default:
		httpError(w, 405, "POST or GET")
	}
}

// POST /v1/admin/risk - set/replace the fraud rule set (audited).
// Body: {"rules":[{"id","kind","action","limit","cost_limit","prefixes","account"}]}

func (s *Server) handleRiskRules(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) || !requirePOST(w, r) {
		return
	}
	var req struct {
		Rules []riskctrl.Rule `json:"rules"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, "body required")
		return
	}
	// Validate kinds before applying.
	for _, ru := range req.Rules {
		switch ru.Kind {
		case "freq", "cost_day", "dest_prefix":
		default:
			httpError(w, 400, "rule "+ru.ID+": kind must be freq|cost_day|dest_prefix")
			return
		}
		switch ru.Action {
		case "block", "alert":
		default:
			httpError(w, 400, "rule "+ru.ID+": action must be block|alert")
			return
		}
	}
	before := s.risk.Rules()
	// Persist the full rule set (PG mode): restarts and new replicas
	// restore the same fraud posture instead of starting bare.
	if s.pg != nil {
		if err := s.pg.PutSetting(r.Context(), "risk_rules", req.Rules); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	s.risk.SetRules(req.Rules)
	// Audit the complete rules (before had counts only — a compliance
	// gap: the diff must show what actually changed).
	s.audit(r, "replace", "risk_rules", "", before, req.Rules)
	s.emit(events.SubjectCfgReload, map[string]any{"entity": "risk", "rules": req.Rules})
	writeJSON(w, 200, map[string]any{"rules": len(req.Rules)})
}

// POST /v1/admin/ipallow {"rules":[{"id","cidr","action","note"}]}
// replaces the source-IP allowlist (full-set semantics, same as risk
// rules); {"remove_id":"x"} removes one rule instead. GET lists the
// current set. Empty set = default-deny (no SIP source is admitted).

func (s *Server) handleIPAllow(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Rules    []ipallow.Rule `json:"rules"`
			RemoveID string         `json:"remove_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			httpError(w, 400, "body required")
			return
		}
		if req.RemoveID != "" {
			current := s.ipallow.Rules()
			kept := current[:0:0]
			removed := false
			for _, ru := range current {
				if ru.ID == req.RemoveID {
					removed = true
					continue
				}
				kept = append(kept, ru)
			}
			if !removed {
				httpError(w, 404, "rule not found: "+req.RemoveID)
				return
			}
			before := current
			if s.pg != nil {
				if err := s.pg.PutSetting(r.Context(), "ip_allowlist", kept); err != nil {
					httpError(w, 500, err.Error())
					return
				}
			}
			s.ipallow.Reload(kept)
			s.audit(r, "remove", "ip_allowlist", req.RemoveID, before, kept)
			s.emit(events.SubjectCfgReload, ipAllowPayload(kept))
			writeJSON(w, 200, map[string]any{"removed": req.RemoveID, "rules": len(kept)})
			return
		}
		// Full-set replace: validate every rule before touching state.
		for _, ru := range req.Rules {
			if err := ipallow.Validate(ru); err != nil {
				httpError(w, 400, err.Error())
				return
			}
		}
		before := s.ipallow.Rules()
		if s.pg != nil {
			if err := s.pg.PutSetting(r.Context(), "ip_allowlist", req.Rules); err != nil {
				httpError(w, 500, err.Error())
				return
			}
		}
		s.ipallow.Reload(req.Rules)
		s.audit(r, "replace", "ip_allowlist", "", before, req.Rules)
		s.emit(events.SubjectCfgReload, ipAllowPayload(req.Rules))
		writeJSON(w, 200, map[string]any{"rules": len(req.Rules)})
	case http.MethodGet:
		writeJSON(w, 200, map[string]any{"rules": s.ipallow.Rules(), "count": s.ipallow.Count()})
	default:
		httpError(w, 405, "POST or GET")
	}
}

// POST /v1/admin/gateways — full-set replace or single removal.
//   {"gateways": [{id,name,sip_addr,protocol,priority,weight,enabled}]}
//   {"remove_id": "<gateway id>"}
// The set is API-authoritative: PG settings persist it for restart,
// the NATS broadcast (entity "gateway", full array payload) drives
// peer replicas and the Kamailio htable mirror. Full-array payloads
// make replay idempotent.

func ipAllowPayload(rules []ipallow.Rule) map[string]any {
	var allows, denies string
	for _, ru := range rules {
		if ru.Action == ipallow.Deny {
			denies += ru.CIDR + ","
		} else {
			allows += ru.CIDR + ","
		}
	}
	return map[string]any{
		"entity": "ip_allowlist", "rules": rules,
		"allow_cidrs": allows, "deny_cidrs": denies,
	}
}


// --- Phase A resource-route wrappers over the legacy verb handlers ---

// handleBlocklistList serves GET /v1/admin/blocklist-entries.
func (s *Server) handleBlocklistList(w http.ResponseWriter, r *http.Request) {
	s.handleBlocklist(w, r) // legacy handler answers GET with the list
}

// handleBlocklistDelete serves DELETE /v1/admin/blocklist-entries/{id}
// where {id} is "<scope>:<pattern>" (e.g. dest:86139). Path parameter
// replaces the legacy {"remove":true} body flag.
func (s *Server) handleBlocklistDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	scope, pattern := "dest", r.PathValue("id")
	if parts := strings.SplitN(pattern, ":", 2); len(parts) == 2 {
		scope, pattern = parts[0], parts[1]
	}
	if s.pg != nil {
		if _, err := s.pg.RemoveNumberList(r.Context(), "", "black", scope, pattern); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	n := s.blocklist.Remove("", "black", scope, pattern)
	s.audit(r, "remove", "blocklist", pattern, map[string]any{"removed": n}, nil)
	s.emitListOp("remove", blocklist.Entry{Kind: "black", Scope: scope, Pattern: pattern})
	writeJSON(w, 200, map[string]any{"removed": n})
}

// handleBlocklistLegacy keeps the old route alive during the
// deprecation window (307 to the resource form).
func (s *Server) handleBlocklistLegacy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleBlocklist(w, r) // body semantics too rich to redirect
		return
	}
	http.Redirect(w, r, "/v1/admin/blocklist-entries", http.StatusTemporaryRedirect)
}

// handleIPAllowList serves GET /v1/admin/ipallow/rules.
func (s *Server) handleIPAllowList(w http.ResponseWriter, r *http.Request) {
	s.handleIPAllow(w, r) // legacy GET branch
}

// handleIPAllowDelete serves DELETE /v1/admin/ipallow/rules/{id}:
// path parameter replaces the legacy {"remove_id"} body field.
func (s *Server) handleIPAllowDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	current := s.ipallow.Rules()
	kept := current[:0:0]
	removed := false
	for _, ru := range current {
		if ru.ID == id {
			removed = true
			continue
		}
		kept = append(kept, ru)
	}
	if !removed {
		httpError(w, 404, "rule not found: "+id)
		return
	}
	s.ipallow.Reload(kept)
	if s.pg != nil {
		if err := s.pg.PutSetting(r.Context(), "ip_allowlist", ipAllowPayload(kept)); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	s.audit(r, "remove", "ipallow", id, map[string]any{"removed": 1}, nil)
	writeJSON(w, 200, map[string]any{"removed": id})
}

// handleIPAllowLegacy deprecates POST /v1/admin/ipallow (body-flag
// semantics) during the migration window.
func (s *Server) handleIPAllowLegacy(w http.ResponseWriter, r *http.Request) {
	s.handleIPAllow(w, r)
}


// handleBlocklistSources serves GET /v1/admin/blocklist-sources:
// entry counts per source (api / static / feed tags).
func (s *Server) handleBlocklistSources(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"sources": blocklist.SourceStats(s.blocklist)})
}
