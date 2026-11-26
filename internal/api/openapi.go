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
}

// openAPIEndpoints enumerates the public surface (internal control
// plane, stream-push test hook and the media stream stay out).
var openAPIEndpoints = []oasEndpoint{
	{"POST", "/v1/call/start", "Billing pre-lock for an INVITE (internal token)", "signaling", true},
	{"POST", "/v1/call/answer", "Mark the call answered", "signaling", true},
	{"POST", "/v1/call/end", "Settle the call and emit the CDR", "signaling", true},
	{"GET", "/v1/stats", "Runtime statistics", "ops", false},
	{"POST", "/v1/admin/accounts", "Create an account", "accounts", true},
	{"GET", "/v1/admin/accounts", "List accounts", "accounts", false},
	{"POST", "/v1/admin/accounts/{id}/topup", "Adjust a balance", "accounts", true},
	{"GET", "/v1/admin/rates", "List rates", "rates", false},
	{"PUT", "/v1/admin/rates", "Upsert a rate", "rates", true},
	{"GET", "/v1/admin/risk-rules", "List risk rules", "risk", false},
	{"PUT", "/v1/admin/risk-rules", "Replace the rule set", "risk", true},
	{"GET", "/v1/admin/gateways", "List gateways", "routing", false},
	{"PUT", "/v1/admin/gateways", "Replace the gateway set (LCR-ordered)", "routing", true},
	{"DELETE", "/v1/admin/gateways/{id}", "Remove a gateway", "routing", false},
	{"GET", "/v1/admin/registrations", "SIP registration mirror", "signaling", false},
	{"GET", "/v1/admin/cdrs", "Query CDRs (cursor pagination)", "cdr", false},
	{"GET", "/v1/admin/recording", "Recording toggle state", "recording", false},
	{"POST", "/v1/admin/recording", "Toggle global recording", "recording", true},
	{"GET", "/v1/admin/blocklist-entries", "List blocklist entries", "policy", false},
	{"POST", "/v1/admin/blocklist-entries", "Add a blocklist entry", "policy", true},
	{"DELETE", "/v1/admin/blocklist-entries/{id}", "Remove an entry ({scope}:{pattern})", "policy", false},
	{"GET", "/v1/admin/blocklist-sources", "Per-source entry counts", "policy", false},
	{"GET", "/v1/admin/ipallow/rules", "List IP allowlist rules", "policy", false},
	{"POST", "/v1/admin/ipallow/rules", "Replace IP allowlist rules (emptying needs confirm_empty:true)", "policy", true},
	{"DELETE", "/v1/admin/ipallow/rules/{id}", "Remove a rule (last-rule removal 409s; POST empty+confirm_empty to empty)", "policy", false},
	{"GET", "/v1/admin/packages", "List packages", "packages", false},
	{"PUT", "/v1/admin/packages", "Define/update a package", "packages", true},
	{"GET", "/v1/admin/packages/usage", "Package usage for an account", "packages", false},
	{"POST", "/v1/admin/reports/rollup", "Run the daily rollup", "reports", true},
	{"GET", "/v1/admin/reports", "Daily report rows", "reports", false},
	{"GET", "/v1/admin/reports/views", "Report views", "reports", false},
	{"POST", "/v1/admin/keys", "Create an API key", "auth", true},
	{"GET", "/v1/admin/keys", "List API keys", "auth", false},
	{"DELETE", "/v1/admin/keys/{prefix}", "Revoke an API key", "auth", false},
	{"POST", "/v1/admin/axb/bindings", "Bind an AXB privacy pair", "privacy", true},
	{"GET", "/v1/admin/axb/bindings", "List bindings", "privacy", false},
	{"DELETE", "/v1/admin/axb/bindings/{x}", "Revoke a binding", "privacy", false},
	{"GET", "/v1/admin/axb/stats", "AXB binding count", "privacy", false},
	{"POST", "/v1/admin/webhooks", "Register a webhook", "webhooks", true},
	{"GET", "/v1/admin/webhooks", "List webhooks", "webhooks", false},
	{"DELETE", "/v1/admin/webhooks/{id}", "Delete a webhook", "webhooks", false},
	{"GET", "/v1/tenant/me", "Tenant identity, root account state, limits", "tenant", false},
	{"GET", "/v1/tenant/accounts", "Child accounts of the tenant", "tenant", false},
	{"GET", "/v1/tenant/cdrs", "CDRs scoped to the tenant (cursor pagination)", "tenant", false},
	{"GET", "/v1/tenant/stats", "Tenant live calls and today's spend", "tenant", false},
	{"GET", "/v1/tenant/packages/usage", "Package usage scoped to the tenant", "tenant", false},
	{"POST", "/v1/tenant/webhooks", "Register the tenant's webhook (https+public, contract events)", "tenant", true},
	{"GET", "/v1/tenant/webhooks", "List the tenant's webhooks", "tenant", false},
	{"GET", "/v1/tenant/cdr-stream", "WebSocket: live CDR contract envelopes for the tenant", "tenant", false},
	{"GET", "/v1/recording/{call_id}", "List recording tracks of a call", "recording", false},
	{"GET", "/v1/recording/{call_id}/{track}", "Stream a recording track", "recording", false},
	{"GET", "/healthz", "Liveness probe", "ops", false},
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
			"description": "Event-driven soft exchange + billing + routing + risk control. Conventions: docs/api-conventions.md. Auth: Bearer API key (admin/tenant), X-GVOS-Internal (signaling).",
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
