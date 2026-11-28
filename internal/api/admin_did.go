// DID inbound administration: register/list/remove inbound numbers
// (callee-billed). Every mutation emits a cfg.reload edge snapshot so
// the Kamailio did htable converges without restarts.
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync/atomic"

	"github.com/gvos-io/gvos-core/internal/customerdomain/did"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

var didSerial int64

// didPayload is the edge snapshot: the full registry, pre-shaped for
// the Kamailio htable apply route (jansson path notation cannot index
// arrays dynamically, so each entry also ships as joined strings).
func didPayload(list []did.DID) map[string]any {
	return map[string]any{"entity": "did", "dids": list, "count": len(list)}
}

// handleDIDPut serves POST /v1/admin/dids
// {"did_number","account_id","destination_type","destination","enabled"}:
// registers or replaces one inbound number. `enabled` is optional and
// defaults to true — a DID registered without it used to land disabled
// (the zero value) and Resolve silently never matched it.
func (s *Server) handleDIDPut(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	var req struct {
		did.DID
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpErrorC(w, 400, "BAD_REQUEST", "body required")
		return
	}
	d := req.DID
	if req.Enabled != nil {
		d.Enabled = *req.Enabled
	} else {
		d.Enabled = true
	}
	if d.ID == "" {
		d.ID = fmt.Sprintf("did-%d", atomic.AddInt64(&didSerial, 1))
	}
	if err := s.dids.Upsert(d); err != nil {
		httpErrorC(w, 400, "BAD_REQUEST", err.Error())
		return
	}
	if s.pg != nil {
		if err := s.pg.PutSetting(r.Context(), "dids", s.dids.List()); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	s.audit(r, "upsert", "did", d.Number, nil, d)
	s.emit(events.SubjectCfgReload, didPayload(s.dids.List()))
	writeJSON(w, 200, d)
}

// handleDIDList serves GET /v1/admin/dids.
func (s *Server) handleDIDList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	list := s.dids.List()
	// Stable order (Store.List is a map walk): by number.
	sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	limit, offset := listPage(r, len(list))
	page := list[offset : offset+limit]
	next := ""
	if offset+limit < len(list) {
		next = strconv.Itoa(offset + limit)
	}
	writeJSON(w, 200, map[string]any{"dids": page, "items": page, "count": len(page), "total": len(list), "next_cursor": next})
}

// handleDIDDelete serves DELETE /v1/admin/dids/{number}: immediate;
// in-flight calls settle normally.
func (s *Server) handleDIDDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	number := r.PathValue("number")
	if !s.dids.Delete(number) {
		httpErrorC(w, 404, "NOT_FOUND", "no DID registered for "+number)
		return
	}
	if s.pg != nil {
		if err := s.pg.PutSetting(r.Context(), "dids", s.dids.List()); err != nil {
			httpError(w, 500, err.Error())
			return
		}
	}
	s.audit(r, "delete", "did", number, nil, nil)
	s.emit(events.SubjectCfgReload, didPayload(s.dids.List()))
	writeJSON(w, 200, map[string]any{"deleted": number})
}

// handleDIDSnapshot serves GET /v1/internal/did-snapshot: the edge
// poller's reconciliation fallback (mirrors gateway-snapshot).
func (s *Server) handleDIDSnapshot(w http.ResponseWriter, r *http.Request) {
	if !requireInternal(w, r) {
		return
	}
	list := s.dids.List()
	writeJSON(w, 200, didPayload(list))
}
