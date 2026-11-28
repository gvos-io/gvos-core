// Webhook delivery log + replay (FR-WH-001 enhancement).
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/gvos-io/gvos-core/internal/shared/store"
	"github.com/gvos-io/gvos-core/internal/webhook"
)

// pgDeliveryLogger adapts store.PG to webhook.DeliveryLogger without
// importing store types into the webhook package.
type pgDeliveryLogger struct{ pg *store.PG }

func (l pgDeliveryLogger) LogDelivery(ctx context.Context, rec webhook.DeliveryRecord) error {
	return l.pg.InsertWebhookDelivery(ctx, rec.SubID, rec.URL, rec.Subject, rec.Payload, rec.Status, rec.LastError, rec.TenantID, rec.Attempts)
}

// handleWebhookDeliveries serves GET /v1/admin/webhooks/{id}/deliveries.
func (s *Server) handleWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		writeJSON(w, 200, map[string]any{"items": []any{}, "count": 0, "note": "delivery log requires PG mode"})
		return
	}
	subID := r.PathValue("id")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, next, err := s.pg.ListWebhookDeliveries(r.Context(), subID, limit, r.URL.Query().Get("cursor"))
	if err != nil {
		if len(err.Error()) > 9 && err.Error()[:9] == "bad cursor" {
			httpErrorC(w, 400, "BAD_CURSOR", err.Error())
			return
		}
		httpError(w, 500, err.Error())
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, rw := range rows {
		out = append(out, map[string]any{
			"id": rw.ID, "sub_id": rw.SubID, "url": rw.URL, "subject": rw.Subject,
			"status": rw.Status, "attempts": rw.Attempts, "last_error": rw.LastError,
			"tenant_id": rw.TenantID, "created_at": rw.CreatedAt.UTC().Format(timeRFC3339),
			// payload omitted from list (can be large); fetch via replay path
		})
	}
	writeJSON(w, 200, map[string]any{"items": out, "deliveries": out, "count": len(out), "next_cursor": next})
}

const timeRFC3339 = "2006-01-02T15:04:05Z07:00"

// handleWebhookDeliveryReplay serves
// POST /v1/admin/webhooks/deliveries/{id}/replay — re-POSTs the stored
// payload using the subscription's current secret (or returns 404 if
// the subscription is gone).
func (s *Server) handleWebhookDeliveryReplay(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	if s.pg == nil {
		httpErrorC(w, 503, "UNAVAILABLE", "delivery replay requires PG mode")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		httpErrorC(w, 400, "BAD_REQUEST", "delivery id required")
		return
	}
	row, err := s.pg.GetWebhookDelivery(r.Context(), id)
	if err != nil {
		httpErrorC(w, 404, "NOT_FOUND", "delivery not found")
		return
	}
	if row.SubID == "" {
		httpErrorC(w, 409, "CONFLICT", "delivery has no subscription id; cannot sign replay")
		return
	}
	sub, err := s.pg.GetWebhookByID(r.Context(), row.SubID)
	if err != nil || sub == nil || !sub.Active {
		httpErrorC(w, 409, "CONFLICT", "subscription missing or inactive")
		return
	}

	var payload any
	if row.Payload != "" {
		_ = json.Unmarshal([]byte(row.Payload), &payload)
	}
	d := webhook.NewDeliverer()
	attempts := d.Deliver(r.Context(), webhook.Subscription{
		ID: sub.ID, URL: sub.URL, Secret: sub.Secret, Events: sub.Events, Active: sub.Active, Tenant: sub.TenantID,
	}, row.Subject, 0, payload)

	status := "delivered"
	if attempts >= len(webhook.Backoff) || attempts == 0 {
		status = "dead"
	}
	_ = s.pg.InsertWebhookDelivery(r.Context(), sub.ID, sub.URL, row.Subject, row.Payload, status, "replay", sub.TenantID, attempts)
	s.audit(r, "replay", "webhook_delivery", strconv.FormatInt(id, 10), nil, map[string]any{"status": status, "attempts": attempts})
	writeJSON(w, 200, map[string]any{"replayed": id, "status": status, "attempts": attempts})
}
