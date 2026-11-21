// Tenant self-service plane (api-roadmap Phase C): read-only queries
// scoped to the caller's tenant. Every query filters by the API key's
// tenant_id — the account tree and CDR ring carry the tenant linkage
// already, so this plane is exposure discipline, not new data paths.
//
// Auth: role must be tenant|agent (admin callers use the admin plane;
// a 403 with a pointer there keeps the contract crisp). The legacy
// constant token does NOT work here — tenant scoping is
// security-relevant and the legacy identity has no tenant binding.
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/webhook"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

// requireTenant authenticates a tenant-plane request: a gvos_ API key
// resolving to role tenant|agent. Returns the bound identity.
func (s *Server) requireTenant(w http.ResponseWriter, r *http.Request) (identity, bool) {
	h := r.Header.Get("Authorization")
	const bearer = "Bearer "
	if !strings.HasPrefix(h, bearer) || s.acl == nil {
		httpErrorC(w, 403, "FORBIDDEN", "tenant plane requires a tenant|agent API key (legacy token has no tenant binding)")
		return identity{}, false
	}
	k, ok := s.acl.Lookup(strings.TrimPrefix(h, bearer))
	if !ok {
		httpErrorC(w, 401, "UNAUTHORIZED", "unknown key")
		return identity{}, false
	}
	if k.Role != acl.RoleTenant && k.Role != acl.RoleAgent {
		httpErrorC(w, 403, "FORBIDDEN", "role "+string(k.Role)+" cannot use the tenant plane")
		return identity{}, false
	}
	id := identity{TenantID: k.TenantID, Role: k.Role, Prefix: k.Prefix}
	return id, true
}

// tenantAccounts returns the tenant root account plus its direct
// children (the demo tree is shallow; deep chains resolve via Chain
// when the tree grows). The returned set is the tenant filter for
// every query on this plane.
func (s *Server) tenantAccounts(tenantID string) map[string]bool {
	set := map[string]bool{tenantID: true}
	if all, err := s.store.List(); err == nil {
		for _, a := range all {
			if a.ParentID == tenantID {
				set[a.ID] = true
			}
		}
	}
	return set
}

// GET /v1/tenant/me — the key's identity, root account state
// (balance/limits), and the tenant's account count.
func (s *Server) handleTenantMe(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	resp := map[string]any{
		"tenant_id": id.TenantID,
		"role":      string(id.Role),
		"key_prefix": id.Prefix,
	}
	if root, err := s.store.Get(id.TenantID); err == nil {
		resp["account"] = root
		resp["currency"] = root.Currency
	} else {
		resp["account"] = nil
	}
	count := 0
	if all, err := s.store.List(); err == nil {
		for _, a := range all {
			if a.ParentID == id.TenantID {
				count++
			}
		}
	}
	resp["child_accounts"] = count
	writeJSON(w, 200, resp)
}

// GET /v1/tenant/accounts — the tenant's child accounts (read-only
// projections: no secret_hash, no internal bookkeeping).
func (s *Server) handleTenantAccounts(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	all, err := s.store.List()
	if err != nil {
		httpError(w, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, a := range all {
		if a.ParentID != id.TenantID {
			continue
		}
		out = append(out, map[string]any{
			"id": a.ID, "username": a.Username, "name": a.Name,
			"credit": a.Credit, "currency": a.Currency,
			"postpaid": a.Postpaid, "concurrent": a.Concurrent, "cps": a.CPS,
		})
	}
	writeJSON(w, 200, map[string]any{"accounts": out, "count": len(out)})
}

// GET /v1/tenant/cdrs — CDRs filtered to the tenant's account set.
// Same cursor pagination as the admin face; rows outside the tenant
// set are invisible (escape test: A-tenant key never sees B-tenant
// rows — asserted in tenant_test.go).
func (s *Server) handleTenantCDRs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	set := s.tenantAccounts(id.TenantID)
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := json.Number(l).Int64(); err == nil && n > 0 && n <= 500 {
			limit = int(n)
		}
	}
	s.cdrsMu.Lock()
	n := len(s.cdrs)
	cursor := 0
	if c := r.URL.Query().Get("cursor"); c != "" {
		if v, err := json.Number(c).Int64(); err == nil && v >= 0 && int(v) <= n {
			cursor = int(v)
		} else {
			s.cdrsMu.Unlock()
			httpErrorC(w, 400, "BAD_CURSOR", "cursor out of range")
			return
		}
	}
	out := make([]*tenantCDRRow, 0, limit)
	for i := n - 1 - cursor; i >= 0 && len(out) < limit; i-- {
		cdr := s.cdrs[i]
		if !set[cdr.AccountID] {
			continue
		}
		out = append(out, &tenantCDRRow{
			CallID: cdr.CallID, AccountID: cdr.AccountID, Dest: cdr.Dest,
			StartTime: cdr.StartTime.Format(time.RFC3339), Duration: cdr.Duration,
			BilledSecs: cdr.BilledSecs, Cost: cdr.Cost.String(),
			Currency: "CNY", Release: cdr.Release, SIPCode: cdr.SIPCode,
		})
	}
	nextCursor := 0
	if n-cursor-len(out) > 0 {
		nextCursor = cursor + len(out)
	}
	s.cdrsMu.Unlock()
	writeJSON(w, 200, map[string]any{"cdrs": out, "count": len(out), "cursor": nextCursor})
}

// tenantCDRRow is the stable projection for the tenant plane (same
// discipline as the webhook contract: internal refactors do not leak).
type tenantCDRRow struct {
	CallID     string `json:"call_id"`
	AccountID  string `json:"account_id"`
	Dest       string `json:"dest"`
	StartTime  string `json:"start_time"`
	Duration   int    `json:"duration"`
	BilledSecs int    `json:"billed_secs"`
	Cost       string `json:"cost"`
	Currency   string `json:"currency"`
	Release    string `json:"release"`
	SIPCode    int    `json:"sip_code"`
}

// GET /v1/tenant/stats — the tenant's slice of the platform: live
// calls, today's spend, children, package shortfall per account.
func (s *Server) handleTenantStats(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	set := s.tenantAccounts(id.TenantID)
	s.cdrsMu.Lock()
	live, spent, zero := 0, 0, money.Amount(0)
	spend := money.Amount(0)
	_ = zero
	today := time.Now().Format("2006-01-02")
	for _, cdr := range s.cdrs {
		if !set[cdr.AccountID] {
			continue
		}
		if cdr.StartTime.Format("2006-01-02") == today {
			spent++
			spend = spend.Add(cdr.Cost)
		}
	}
	// live calls for the tenant: the manager scan is the authority
	live = s.calls.LiveByAccounts(set)
	s.cdrsMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"tenant_id": id.TenantID, "live_calls": live,
		"today_cdrs": spent, "today_spend": spend,
	})
}

// GET /v1/tenant/packages/usage — per-account package usage, scoped.
// No account param = every child account with an assigned package.
func (s *Server) handleTenantPackageUsage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	set := s.tenantAccounts(id.TenantID)
	acc := r.URL.Query().Get("account")
	if acc != "" {
		if !set[acc] {
			httpErrorC(w, 404, "NOT_FOUND", "account not in this tenant")
			return
		}
		free, spend, ok := s.pkg.Usage(acc)
		writeJSON(w, 200, map[string]any{"account": acc, "assigned": ok,
			"free_secs_used": free, "cycle_spend": spend, "min_spend_shortfall": s.pkg.MinSpendShortfall(acc)})
		return
	}
	out := []map[string]any{}
	for a := range set {
		if _, _, ok := s.pkg.Usage(a); ok {
			free, spend, _ := s.pkg.Usage(a)
			out = append(out, map[string]any{"account": a, "assigned": true,
				"free_secs_used": free, "cycle_spend": spend})
		}
	}
	writeJSON(w, 200, map[string]any{"usage": out, "count": len(out)})
}

// POST /v1/tenant/webhooks — the tenant registers its own callback
// endpoint (same validation and store as the admin face, tenant-scoped
// to the key identity; events enum closed to the contract set).
func (s *Server) handleTenantWebhookCreate(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" || len(req.Events) == 0 {
		httpErrorC(w, 400, "BAD_REQUEST", "url and events[] required")
		return
	}
	// Same SSRF gate as the admin face: the deliverer POSTs from
	// inside the cluster.
	if err := webhook.ValidateSubscription(req.URL); err != nil {
		httpErrorC(w, 400, "BAD_REQUEST", "webhook url rejected: "+err.Error())
		return
	}
	allowed := map[string]bool{"gvos.cdr.completed": true, "gvos.call.teardown": true}
	for _, e := range req.Events {
		if !allowed[e] {
			httpErrorC(w, 400, "BAD_REQUEST", "unknown contract event "+e)
			return
		}
	}
	secret := genWebhookSecret()
	var wid string
	if s.whStore != nil {
		var err error
		wid, err = s.whStore.CreateWebhookTenant(r.Context(), req.URL, secret, req.Events, id.TenantID)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
	} else {
		wid = "wh-" + secret[6:14]
	}
	s.refreshWebhookDispatcher()
	s.audit(r, "create", "tenant_webhook", wid, nil, map[string]any{"url": req.URL, "events": req.Events, "tenant_id": id.TenantID})
	writeJSON(w, 201, map[string]any{
		"id": wid, "secret": secret, "url": req.URL, "events": req.Events,
		"note": "store this secret now; it is not retrievable later",
	})
}

// GET /v1/tenant/webhooks — the tenant's own subscriptions only.
// Tenant scoping rides the webhook store's tenant binding (012): rows
// without a matching tenant_id are invisible to this plane.
func (s *Server) handleTenantWebhookList(w http.ResponseWriter, r *http.Request) {
	id, ok := s.requireTenant(w, r)
	if !ok {
		return
	}
	out := []map[string]any{}
	if s.whStore != nil {
		rows, err := s.whStore.ListWebhooks(r.Context())
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
		for _, rw := range rows {
			if rw.TenantID != "" && rw.TenantID != id.TenantID {
				continue
			}
			out = append(out, map[string]any{"id": rw.ID, "url": rw.URL, "events": rw.Events, "active": rw.Active})
		}
	}
	writeJSON(w, 200, map[string]any{"webhooks": out, "count": len(out)})
}


