// openapi.go: registry-driven OpenAPI 3.1 generation (api-roadmap
// Phase E, spec half — SDK generation deferred per direction). The
// endpoint table below is the single source: every public route gets
// a path entry with method, summary, tag and the shared error
// envelope; schemas stay lightweight (typed params + free-form
// bodies) until a contract round tightens them. Served at
// GET /openapi.json.
package api

import (
	"encoding/json"
	"net/http"
)

// oasEndpoint is one public API entry.
type oasEndpoint struct {
	Method  string
	Path    string
	Summary string
	Tag     string
	Body    bool // accepts a JSON body
	Params  []oasParam
}

// oasParam is one query parameter (the only parameter kind this API
// uses; path params are noted in the summaries).
type oasParam struct {
	Name, Desc string
}

// openAPIEndpoints enumerates the public surface (internal control
// plane, stream-push test hook and the media stream stay out).
var openAPIEndpoints = []oasEndpoint{
	{"POST", "/v1/calls", "Originate a platform-dialed call (click-to-call / AI outbound): 202 async, lifecycle via GET + call.state webhook; Idempotency-Key supported", "calls", true, nil},
	{"GET", "/v1/calls", "List origination calls (state/account filters, cursor paging)", "calls", false, []oasParam{pLimit(), pCursor(), {"state", "pending|dialing|answered|ended|cancelled|failed"}, {"account_id", "exact account id (tenant keys are scoped to their subtree)"}}},
	{"GET", "/v1/calls/{id}", "Read one origination call", "calls", false, nil},
	{"DELETE", "/v1/calls/{id}", "Cancel (pending/dialing) or tear down (answered)", "calls", false, nil},
	{"POST", "/v1/call/start", "Billing pre-lock for an INVITE (internal token)", "signaling", true, nil},
	{"POST", "/v1/call/answer", "Mark the call answered", "signaling", true, nil},
	{"POST", "/v1/call/end", "Settle the call and emit the CDR", "signaling", true, nil},
	{"GET", "/v1/stats", "Runtime statistics", "ops", false, nil},
	{"POST", "/v1/admin/accounts", "Create an account", "accounts", true, nil},
	{"GET", "/v1/admin/accounts", "List accounts (q/parent_id/roots filters)", "accounts", false, nil},
	{"GET", "/v1/admin/accounts/{id}", "Read one account (id or username)", "accounts", false, nil},
	{"PATCH", "/v1/admin/accounts/{id}", "Update management knobs (name/rate_group/postpaid/max_debt/max_concurrent/max_cps/low_balance; credit only moves via topup/settlement)", "accounts", true, nil},
	{"DELETE", "/v1/admin/accounts/{id}", "Soft-delete an account (409 HAS_CHILDREN while live children exist)", "accounts", false, nil},
	{"POST", "/v1/admin/accounts/{id}/topup", "Adjust a balance", "accounts", true, nil},
	{"GET", "/v1/admin/accounts/{id}/ledger", "Balance movements of one account (topup/settle/transfer/refund)", "accounts", false, pPaging()},
	{"GET", "/v1/admin/rates", "List rates (optional ?group=; rows carry group)", "rates", false, []oasParam{{"group", "rate group name"}}},
	{"GET", "/v1/admin/rate-groups", "List rate groups with live-rate counts", "rates", false, nil},
	{"POST", "/v1/admin/rate-groups", "Define a rate group (PG mode; idempotent)", "rates", true, nil},
	{"DELETE", "/v1/admin/rate-groups/{name}", "Delete an empty, unreferenced group (409 otherwise; demo is protected)", "rates", false, nil},
	{"POST", "/v1/admin/rates/quote", "List-price estimate for a destination (account rate_group when account_id given; package allowance applies only at settlement)", "rates", true, nil},
	{"PUT", "/v1/admin/rates", "Upsert a rate", "rates", true, nil},
	{"DELETE", "/v1/admin/rates/{prefix}", "Retire a rate (PG keeps history via ended_at)", "rates", false, nil},
	{"GET", "/v1/admin/risk-rules", "List risk rules", "risk", false, nil},
	{"PUT", "/v1/admin/risk-rules", "Replace the rule set", "risk", true, nil},
	{"GET", "/v1/admin/gateways", "List gateways", "routing", false, nil},
	{"PUT", "/v1/admin/gateways", "Replace the gateway set (LCR-ordered)", "routing", true, nil},
	{"DELETE", "/v1/admin/gateways/{id}", "Remove a gateway", "routing", false, nil},
	{"GET", "/v1/admin/registrations", "SIP registration mirror (newest activity first)", "signaling", false, pPaging()},
	{"GET", "/v1/admin/cdrs", "Query CDRs (PG history authority in PG mode; memory ring otherwise)", "cdr", false, pCDRFilters()},
	{"GET", "/v1/admin/recording", "Recording toggle state", "recording", false, nil},
	{"POST", "/v1/admin/recording", "Toggle global recording", "recording", true, nil},
	{"GET", "/v1/admin/blocklist-entries", "List blocklist entries (real entries + count)", "policy", false, nil},
	{"POST", "/v1/admin/blocklist-entries", "Add a blocklist entry", "policy", true, nil},
	{"DELETE", "/v1/admin/blocklist-entries/{id}", "Remove an entry ({scope}:{pattern})", "policy", false, nil},
	{"GET", "/v1/admin/blocklist-sources", "Per-source entry counts", "policy", false, nil},
	{"GET", "/v1/admin/ipallow/rules", "List IP allowlist rules", "policy", false, nil},
	{"POST", "/v1/admin/ipallow/rules", "Replace IP allowlist rules (emptying needs confirm_empty:true)", "policy", true, nil},
	{"DELETE", "/v1/admin/ipallow/rules/{id}", "Remove a rule (last-rule removal 409s; POST empty+confirm_empty to empty)", "policy", false, nil},
	{"GET", "/v1/admin/packages", "List packages", "packages", false, nil},
	{"PUT", "/v1/admin/packages", "Define/update a package", "packages", true, nil},
	{"GET", "/v1/admin/packages/usage", "Package usage for an account", "packages", false, nil},
	{"GET", "/v1/admin/audit", "Audit trail: who changed what, when (newest first)", "audit", false, pAuditFilters()},
	{"POST", "/v1/admin/reports/rollup", "Run the daily rollup", "reports", true, nil},
	{"GET", "/v1/admin/reports", "Daily report rows", "reports", false, nil},
	{"GET", "/v1/admin/reports/views", "Report views", "reports", false, nil},
	{"POST", "/v1/admin/keys", "Create an API key", "auth", true, nil},
	{"GET", "/v1/admin/keys", "List API keys", "auth", false, nil},
	{"DELETE", "/v1/admin/keys/{prefix}", "Revoke an API key", "auth", false, nil},
	{"POST", "/v1/admin/axb/bindings", "Bind an AXB privacy pair", "privacy", true, nil},
	{"GET", "/v1/admin/axb/bindings", "List bindings", "privacy", false, nil},
	{"DELETE", "/v1/admin/axb/bindings/{x}", "Revoke a binding", "privacy", false, nil},
	{"GET", "/v1/admin/axb/stats", "AXB binding count", "privacy", false, nil},
	{"POST", "/v1/admin/webhooks", "Register a webhook", "webhooks", true, nil},
	{"GET", "/v1/admin/webhooks", "List webhooks", "webhooks", false, nil},
	{"PATCH", "/v1/admin/webhooks/{id}", "Update url/events/active (the secret is immutable; delete+recreate to rotate)", "webhooks", true, nil},
	{"DELETE", "/v1/admin/webhooks/{id}", "Delete a webhook", "webhooks", false, nil},
	{"GET", "/v1/admin/webhooks/{id}/deliveries", "List webhook delivery attempts (keyset cursor on id)", "webhooks", false, pPaging()},
	{"POST", "/v1/admin/webhooks/deliveries/{id}/replay", "Replay a webhook delivery", "webhooks", false, nil},
	{"POST", "/v1/admin/calls/{call_id}/teardown", "Force-teardown a live call", "signaling", true, nil},
	{"POST", "/v1/admin/dids", "Register/replace an inbound DID", "routing", true, nil},
	{"GET", "/v1/admin/dids", "List inbound DIDs (number order)", "routing", false, pPaging()},
	{"DELETE", "/v1/admin/dids/{number}", "Remove an inbound DID", "routing", false, nil},
	{"GET", "/v1/tenant/me", "Tenant identity, root account state, limits", "tenant", false, nil},
	{"GET", "/v1/tenant/accounts", "Child accounts of the tenant", "tenant", false, nil},
	{"POST", "/v1/tenant/accounts", "Provision a child under the tenant (initial_credit is allocated from the tenant's own balance — transfer semantics; agent role read-only)", "tenant", true, nil},
	{"POST", "/v1/tenant/accounts/{id}/topup", "Allocate credit downward (tenant → child, journaled transfer-out/in; positive delta only)", "tenant", true, nil},
	{"GET", "/v1/tenant/cdrs", "CDRs scoped to the tenant (PG history authority in PG mode)", "tenant", false, pCDRFilters()},
	{"GET", "/v1/tenant/stats", "Tenant live calls and today's spend", "tenant", false, nil},
	{"GET", "/v1/tenant/packages/usage", "Package usage scoped to the tenant", "tenant", false, nil},
	{"POST", "/v1/tenant/webhooks", "Register the tenant's webhook (https+public, contract events)", "tenant", true, nil},
	{"GET", "/v1/tenant/webhooks", "List the tenant's webhooks", "tenant", false, nil},
	{"DELETE", "/v1/tenant/webhooks/{id}", "Delete the tenant's own webhook", "tenant", false, nil},
	{"GET", "/v1/tenant/cdr-stream", "WebSocket: live CDR contract envelopes for the tenant", "tenant", false, nil},
	{"GET", "/v1/recording/{call_id}", "List recording tracks of a call", "recording", false, nil},
	{"GET", "/v1/recording/{call_id}/{track}", "Stream a recording track", "recording", false, nil},
	{"GET", "/healthz", "Liveness probe", "ops", false, nil},
}

// webhookEventEnum is the closed subscribable-event set
// (webhook.ContractSubjects is the runtime authority; the contract
// doc api-conventions §Webhook governs evolution).
var webhookEventEnum = []string{
	"cdr.completed", "call.teardown", "account.low_balance", "risk.blocked", "package.exhausted",
}

// Shared query-parameter vocabulary (api-conventions §4 paging and
// the CDR/audit server-side filters). Single definitions so the spec
// and the handlers cannot drift apart silently.
func pLimit() oasParam  { return oasParam{"limit", "page size, default 50, max 500"} }
func pCursor() oasParam { return oasParam{"cursor", "opaque keyset marker; empty = first page"} }

func pPaging() []oasParam { return []oasParam{pLimit(), pCursor()} }

func pCDRFilters() []oasParam {
	return []oasParam{
		pLimit(), pCursor(),
		{"account_id", "exact account id"},
		{"dest", "exact destination"},
		{"call_id", "exact call id"},
		{"from", "start of window: RFC3339 or YYYY-MM-DD (inclusive)"},
		{"to", "end of window: RFC3339 or YYYY-MM-DD (exclusive)"},
	}
}

func pAuditFilters() []oasParam {
	return []oasParam{
		pLimit(), pCursor(),
		{"entity", "entity name filter (account|rate|gateway|…)"},
		{"entity_id", "exact entity id"},
		{"tenant_id", "tenant uuid"},
	}
}

// errEnvelopeSchema is the shared error body (api-conventions §3).
func errEnvelopeSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"error": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code":    map[string]any{"type": "string", "description": "stable SCREAMING_SNAKE code (api-conventions §3)"},
					"message": map[string]any{"type": "string"},
				},
				"required": []string{"code", "message"},
			},
		},
	}
}

// buildOpenAPI assembles the 3.1 document.
func buildOpenAPI() map[string]any {
	paths := map[string]any{}
	for _, e := range openAPIEndpoints {
		p, ok := paths[e.Path].(map[string]any)
		if !ok {
			p = map[string]any{}
			paths[e.Path] = p
		}
		op := map[string]any{
			"summary": e.Summary,
			"tags":    []string{e.Tag},
			"responses": map[string]any{
				"default": map[string]any{
					"description": "error envelope on failure",
					"content":     map[string]any{"application/json": map[string]any{"schema": errEnvelopeSchema()}},
				},
			},
		}
		if e.Body {
			op["requestBody"] = map[string]any{
				"content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "object"}}},
			}
		}
		if len(e.Params) > 0 {
			params := make([]map[string]any, 0, len(e.Params))
			for _, prm := range e.Params {
				params = append(params, map[string]any{
					"name": prm.Name, "in": "query", "required": false,
					"description": prm.Desc,
					"schema":      map[string]any{"type": "string"},
				})
			}
			op["parameters"] = params
		}
		p[string([]byte(e.Method))] = op
	}
	for path, methods := range paths {
		pm := methods.(map[string]any)
		for method, opAny := range pm {
			op := opAny.(map[string]any)
			tag := ""
			if tags, ok := op["tags"].([]string); ok && len(tags) > 0 {
				tag = tags[0]
			}
			switch tag {
			case "signaling":
				op["security"] = []map[string]any{{"InternalToken": []string{}}}
			case "ops":
				// healthz / stats: stats may be public; leave open
			default:
				op["security"] = []map[string]any{{"BearerAuth": []string{}}}
			}
			_ = method
		}
		_ = path
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       "gvos API",
			"version":     "1.1.0",
			"description": "Event-driven soft exchange + billing + routing + risk control. Conventions: docs/api-conventions.md. Auth: Bearer API key (admin/tenant), X-GVOS-Internal (signaling). Webhook events (closed enum, v1 envelope): cdr.completed, call.teardown, account.low_balance, risk.blocked, package.exhausted. Paging: cursor + limit (default 50, max 500), responses carry next_cursor (empty = last page); list rows also ride items.",
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"BearerAuth": map[string]any{
					"type": "http", "scheme": "bearer", "bearerFormat": "gvos_api_key",
					"description": "Admin or tenant API key (Authorization: Bearer …)",
				},
				"InternalToken": map[string]any{
					"type": "apiKey", "in": "header", "name": "X-GVOS-Internal",
					"description": "Machine token for Kamailio ↔ gv-core signaling face",
				},
			},
			"schemas": map[string]any{
				"ErrorEnvelope": errEnvelopeSchema(),
				"CallStart": map[string]any{
					"type":     "object",
					"required": []string{"call_id", "from_user", "to_user"},
					"properties": map[string]any{
						"call_id":   map[string]any{"type": "string"},
						"from_user": map[string]any{"type": "string"},
						"to_user":   map[string]any{"type": "string"},
					},
				},
				"AXBBind": map[string]any{
					"type":     "object",
					"required": []string{"a_number", "b_number"},
					"properties": map[string]any{
						"a_number": map[string]any{"type": "string"},
						"b_number": map[string]any{"type": "string"},
						"ttl_secs": map[string]any{"type": "integer"},
					},
				},
			},
		},
		"paths": paths,
	}
}

// handleOpenAPI serves GET /openapi.json (no auth: the spec describes
// the public contract; endpoints themselves stay guarded).
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildOpenAPI())
}
