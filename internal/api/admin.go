// Administrative API: auth guard, audit seam, and shared helpers.
package api

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"math/rand"
	"net/http"
	"time"

	"github.com/lixuanqun/gvos-core/internal/shared/events"
)

func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+adminToken {
		httpError(w, 401, "admin token required")
		return false
	}
	return true
}

// POST /v1/admin/accounts {"name","username","parent_id","rate_group","postpaid","credit"}

func (s *Server) audit(r *http.Request, action, entity, entityID string, before, after any) {
	s.emit(events.SubjectAdminChanged, map[string]any{
		"action": action, "entity": entity, "entity_id": entityID,
		"before": before, "after": after,
	})
	if s.asyncAudit || s.pg == nil {
		return
	}
	ctx, cancel := contextWithTimeout(r.Context())
	defer cancel()
	if err := s.pg.AppendAudit(ctx, demoTenant, "admin", action, entity, entityID, before, after); err != nil {
		// Audit failure must never block the business answer, but it
		// must be loud: append-only audit is a compliance feature.
		logPrintf("AUDIT persist failed (%s %s %s): %v", action, entity, entityID, err)
	}
}

func contextWithTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return contextWithTimeoutDuration(parent, 5*time.Second)
}

// emitListOp broadcasts one screening-rule change as an operation
// payload: peers apply it directly to their in-memory list instead of
// reloading from PG (which would revert memory-only state). The DB
// write-through is the durability path; this is the propagation path.

func genSIPPassword(n int) string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	b := make([]byte, n)
	rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// computeHA1: MD5(username:realm:password) for Digest auth.

func computeHA1(user, realm, pass string) string {
	sum := md5.Sum([]byte(user + ":" + realm + ":" + pass))
	return hex.EncodeToString(sum[:])
}

// startReportWorker runs the T+1 rollup: every hour, aggregates
// yesterday (and today-so-far at each pass, so intra-day numbers
// stay fresh). Manual trigger: POST /v1/admin/reports {"day":"..."}.

func requireGET(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		httpError(w, http.StatusMethodNotAllowed, "GET required")
		return false
	}
	return true
}

// ipAllowPayload builds the ip_allowlist broadcast: the rule array for
// audit/replicas plus the pre-joined CIDR lists the Kamailio edge
// consumes directly (its jansson path notation cannot index arrays
// with dynamic variables).

const demoTenant = "00000000-0000-0000-0000-000000000001"

// adminToken is the Phase A interim admin credential (env override in
// Phase B replaces this constant with API keys).
const adminToken = "dev-admin-token"
