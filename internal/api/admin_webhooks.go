// Outbound webhook administration (Phase D): register/list/delete
// customer endpoints. The HMAC secret is returned exactly once at
// creation (same discipline as API keys / SIP passwords).
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/gvos-io/gvos-core/internal/shared/store"
	"github.com/gvos-io/gvos-core/internal/webhook"
)

// PG webhook subscription storage face (store.PG implements; memory
// mode keeps subscriptions process-local and non-persistent).
type WebhookStore interface {
	CreateWebhook(ctx context.Context, url, secret string, events []string) (string, error)
	CreateWebhookTenant(ctx context.Context, url, secret string, events []string, tenantID string) (string, error)
	ListWebhooks(ctx context.Context) ([]store.WebhookRow, error)
	DeleteWebhook(ctx context.Context, id string) (int, error)
}

func genWebhookSecret() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return "whsec_" + hex.EncodeToString(b)
}

// handleWebhookCreate serves POST /v1/admin/webhooks {url, events[]}.
func (s *Server) handleWebhookCreate(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
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
	for _, e := range req.Events {
		if !strings.HasPrefix(e, "gvos.") {
			httpErrorC(w, 400, "BAD_REQUEST", "events must be gvos.* subjects")
			return
		}
	}
	// SSRF gate (contract §security): the deliverer POSTs from inside
	// the cluster, so customer URLs must be https + public. Validated
	// at create AND update; delivery-time DNS check is the second
	// layer (webhook.ValidateSubscription).
	if err := webhook.ValidateSubscription(req.URL); err != nil {
		httpErrorC(w, 400, "BAD_REQUEST", "webhook url rejected: "+err.Error())
		return
	}
	// Contract events ride the v1 envelope (contract.go): the enum is
	// closed — cdr.completed + call.teardown.
	allowed := map[string]bool{"gvos.cdr.completed": true, "gvos.call.teardown": true}
	for _, e := range req.Events {
		if !allowed[e] {
			httpErrorC(w, 400, "BAD_REQUEST", "unknown contract event "+e)
			return
		}
	}
	secret := genWebhookSecret()
	var id string
	if s.whStore != nil {
		var err error
		id, err = s.whStore.CreateWebhook(r.Context(), req.URL, secret, req.Events)
		if err != nil {
			httpError(w, 500, err.Error())
			return
		}
	} else {
		id = "wh-" + secret[6:14]
	}
	s.refreshWebhookDispatcher()
	s.audit(r, "create", "webhook", id, nil, map[string]any{"url": req.URL, "events": req.Events})
	writeJSON(w, 201, map[string]any{
		"id": id, "secret": secret, // one-time display
		"url": req.URL, "events": req.Events,
		"note": "store this secret now; it is not retrievable later",
	})
}

// handleWebhookList serves GET /v1/admin/webhooks.
func (s *Server) handleWebhookList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
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
			out = append(out, map[string]any{"id": rw.ID, "url": rw.URL, "events": rw.Events, "active": rw.Active})
		}
	}
	writeJSON(w, 200, map[string]any{"webhooks": out, "count": len(out)})
}

// handleWebhookDelete serves DELETE /v1/admin/webhooks/{id}.
func (s *Server) handleWebhookDelete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.whStore != nil {
		if n, _ := s.whStore.DeleteWebhook(r.Context(), r.PathValue("id")); n == 0 {
			httpErrorC(w, 404, "NOT_FOUND", "no webhook with that id")
			return
		}
	}
	s.refreshWebhookDispatcher()
	s.audit(r, "delete", "webhook", r.PathValue("id"), nil, nil)
	writeJSON(w, 200, map[string]any{"deleted": r.PathValue("id")})
}

// refreshWebhookDispatcher reloads the active subscription set into
// the dispatcher (PG mode) — memory mode keeps whatever the process
// created this boot.
func (s *Server) refreshWebhookDispatcher() {
	if s.whDispatch == nil || s.whStore == nil {
		return
	}
	rows, err := s.whStore.ListWebhooks(context.Background())
	if err != nil {
		log.Printf("webhook dispatcher refresh: %v", err)
		return
	}
	subs := make([]webhook.Subscription, 0, len(rows))
	for _, rw := range rows {
		subs = append(subs, webhook.Subscription{ID: rw.ID, URL: rw.URL, Secret: rw.Secret, Events: rw.Events, Active: rw.Active, Tenant: rw.TenantID})
	}
	s.whDispatch.SetSubscriptions(subs)
}
