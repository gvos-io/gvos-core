// Administrative API: auth guard, audit seam, and shared helpers.
package api

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/shared/acl"
	"github.com/gvos-io/gvos-core/internal/shared/events"
)

// identity is the authenticated caller bound into the request
// context (Phase B: the API-key face). TenantID is the audit
// dimension; Prefix is the display handle recorded in audit rows.
type identity struct {
	TenantID string
	Role     acl.Role
	Prefix   string
}

type ctxKey int

const identityKey ctxKey = 1

// whoAmI returns the request identity, or the legacy fallback.
func whoAmI(r *http.Request) identity {
	if id, ok := r.Context().Value(identityKey).(identity); ok {
		return id
	}
	return identity{TenantID: demoTenant, Role: acl.RoleAdmin, Prefix: "legacy"}
}

// requireAdmin enforces the admin-plane credential: an API key
// (Bearer gvos_...) resolved through the acl registry, falling back
// to the Phase A constant token only in the no-key demo mode (a
// warning logs once). Fail-closed: no registry and no fallback
// configured means 401, never open.
func (s *Server) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	h := r.Header.Get("Authorization")
	const bearer = "Bearer "
	if !strings.HasPrefix(h, bearer) {
		httpErrorC(w, 401, "UNAUTHORIZED", "admin token required")
		return false
	}
	presented := strings.TrimPrefix(h, bearer)
	if strings.HasPrefix(presented, "gvos_") {
		if s.acl != nil {
			if k, ok := s.acl.Lookup(presented); ok {
				if k.Role != acl.RoleAdmin && k.Role != acl.RoleOperator {
					httpErrorC(w, 403, "FORBIDDEN", "key role "+string(k.Role)+" cannot use the admin plane")
					return false
				}
				ctx := context.WithValue(r.Context(), identityKey, identity{
					TenantID: k.TenantID, Role: k.Role, Prefix: k.Prefix,
				})
				*r = *r.Clone(ctx)
				return true
			}
		}
		httpErrorC(w, 401, "UNAUTHORIZED", "unknown or revoked API key")
		return false
	}
	// Legacy fallback: constant token only while no key is registered.
	if s.acl == nil || s.acl.Count() == 0 {
		if presented == adminToken {
			log.Printf("WARN admin auth via legacy constant token (no API keys registered; Phase B deprecation)")
			ctx := context.WithValue(r.Context(), identityKey, identity{
				TenantID: demoTenant, Role: acl.RoleAdmin, Prefix: "legacy",
			})
			*r = *r.Clone(ctx)
			return true
		}
	}
	httpErrorC(w, 401, "UNAUTHORIZED", "admin token required")
	return false
}

// POST /v1/admin/accounts {"name","username","parent_id","rate_group","postpaid","credit"}

func (s *Server) audit(r *http.Request, action, entity, entityID string, before, after any) {
	id := whoAmI(r)
	s.emit(events.SubjectAdminChanged, map[string]any{
		"action": action, "entity": entity, "entity_id": entityID,
		"operator": id.Prefix, "tenant_id": id.TenantID,
		"before": before, "after": after,
	})
	if s.asyncAudit || s.pg == nil {
		return
	}
	ctx, cancel := contextWithTimeout(r.Context())
	defer cancel()
	if err := s.pg.AppendAudit(ctx, whoAmI(r).TenantID, whoAmI(r).Prefix, action, entity, entityID, before, after); err != nil {
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
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely; fall back to time-derived bytes rather than
		// panic mid-create (still better than math/rand).
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(now >> (i % 8))
		}
	}
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
