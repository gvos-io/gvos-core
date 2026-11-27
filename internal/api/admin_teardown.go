// Admin forced teardown (FR-TEAR-001): ops can terminate a live call
// without waiting for balance exhaustion or edge timeout.
package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gvos-io/gvos-core/internal/shared/events"
)

// handleCallTeardown serves POST /v1/admin/calls/{call_id}/teardown
// {"reason"?: string}. Publishes gvos.call.teardown for the executor;
// when the event bus is unavailable, dispatches inline.
func (s *Server) handleCallTeardown(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	callID := strings.TrimSpace(r.PathValue("call_id"))
	if callID == "" {
		httpErrorC(w, 400, "BAD_REQUEST", "call_id required")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "admin-teardown"
	}

	s.audit(r, "teardown", "call", callID, nil, map[string]string{"reason": reason})
	s.emit(events.SubjectCallTeardown, map[string]string{"call_id": callID, "reason": reason})
	if s.events == nil {
		dispatchTeardown(callID, reason)
	}
	writeJSON(w, 202, map[string]any{
		"call_id": callID,
		"reason":  reason,
		"status":  "accepted",
	})
}
