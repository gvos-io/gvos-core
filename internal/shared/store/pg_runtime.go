// Runtime-state persistence (migration 014): API keys, the
// idempotency registry and AXB bindings. The in-memory stores remain
// the working set and hot path; PG is the durable copy loaded at boot
// and written through on change, so restarts and replica reloads
// converge instead of losing keys/bindings/idempotency state.
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/customerdomain/axb"
	"github.com/gvos-io/gvos-core/internal/shared/acl"
)

// --- API keys -------------------------------------------------------

// LoadAPIKeys returns every stored key (revoked included — Lookup
// enforces the revocation; loading it keeps replicas converged).
func (p *PG) LoadAPIKeys(ctx context.Context) ([]acl.Key, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT id::text, tenant_id::text, role, prefix, key_hash,
               created_by, revoked_at, last_used
        FROM api_keys`)
	if err != nil {
		return nil, fmt.Errorf("load api keys: %w", err)
	}
	defer rows.Close()
	var out []acl.Key
	for rows.Next() {
		var k acl.Key
		var role string
		if err := rows.Scan(&k.ID, &k.TenantID, &role, &k.Prefix, &k.Hash, &k.CreatedBy, &k.RevokedAt, &k.LastUsed); err != nil {
			return nil, err
		}
		k.Role = acl.Role(role)
		out = append(out, k)
	}
	return out, rows.Err()
}

// CreateAPIKey persists one generated key (hash only at rest).
// ON CONFLICT keeps the bootstrap mint idempotent across restarts
// (same GVOS_ADMIN_TOKEN → same hash → no-op).
func (p *PG) CreateAPIKey(ctx context.Context, k acl.Key) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO api_keys (tenant_id, role, key_hash, prefix, created_by)
        VALUES ($1, $2, $3, $4, $5)
        ON CONFLICT (key_hash) DO NOTHING`,
		k.TenantID, string(k.Role), k.Hash, k.Prefix, k.CreatedBy)
	if err != nil {
		return fmt.Errorf("create api key: %w", err)
	}
	return nil
}

// RevokeAPIKeyByPrefix marks the key revoked; returns rows affected.
func (p *PG) RevokeAPIKeyByPrefix(ctx context.Context, prefix string) (int, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE api_keys SET revoked_at = now() WHERE prefix = $1 AND revoked_at IS NULL`, prefix)
	if err != nil {
		return 0, fmt.Errorf("revoke api key: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// --- Idempotency ----------------------------------------------------

// ReserveIdemKey claims the key for this request: true = first caller,
// proceed; false = someone else holds/completed it (api replays via
// LookupIdemKey). Rows double as the response store: status -1 marks
// an in-flight reservation until CompleteIdemKey lands the response.
func (p *PG) ReserveIdemKey(ctx context.Context, key string) (bool, error) {
	tag, err := p.pool.Exec(ctx, `
        INSERT INTO idempotency_keys (key, status) VALUES ($1, -1)
        ON CONFLICT (key) DO NOTHING`, key)
	if err != nil {
		return false, fmt.Errorf("reserve idempotency key: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CompleteIdemKey stores the first response for replay.
func (p *PG) CompleteIdemKey(ctx context.Context, key string, status int, body, contentType string) error {
	_, err := p.pool.Exec(ctx, `
        UPDATE idempotency_keys SET status = $2, body = $3, content_type = $4
        WHERE key = $1`, key, status, body, contentType)
	if err != nil {
		return fmt.Errorf("complete idempotency key: %w", err)
	}
	return nil
}

// LookupIdemKey returns the stored first response. done=false means
// the reservation exists but the response is still in flight (or the
// row was lost — callers treat it as free after the wait budget).
func (p *PG) LookupIdemKey(ctx context.Context, key string) (status int, body, contentType string, done bool, err error) {
	err = p.pool.QueryRow(ctx, `
        SELECT status, body, content_type FROM idempotency_keys WHERE key = $1`, key).
		Scan(&status, &body, &contentType)
	if err != nil {
		return 0, "", "", false, err // pgx.ErrNoRows → not reserved
	}
	return status, body, contentType, status >= 0, nil
}

// PurgeIdemKeys deletes rows past the replay TTL (hourly ticker, same
// cadence as the CDR rollup pass).
func (p *PG) PurgeIdemKeys(ctx context.Context) (int64, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - interval '24 hours'`)
	if err != nil {
		return 0, fmt.Errorf("purge idempotency keys: %w", err)
	}
	return tag.RowsAffected(), nil
}

// TestResetRuntimeState wipes the migration-014 runtime state (API
// keys, idempotency rows, AXB bindings) and the gateways settings
// key. For throwaway-schema integration tests: minted keys would
// otherwise retire the legacy admin token for every later boot in
// the same database.
func (p *PG) TestResetRuntimeState(ctx context.Context) error {
	for _, stmt := range []string{
		`DELETE FROM api_keys`,
		`DELETE FROM idempotency_keys`,
		`DELETE FROM axb_bindings`,
		`DELETE FROM cdr`,
		`DELETE FROM settings WHERE key = 'gateways'`,
		// integration-test fixtures share the throwaway schema; the
		// ledger rows go first (FK balance_ledger→accounts).
		`DELETE FROM balance_ledger WHERE account_id IN (SELECT id FROM accounts WHERE username LIKE 'p1pg-%' OR username LIKE 'restart%' OR username LIKE 'idem-%')`,
		`DELETE FROM accounts WHERE username LIKE 'p1pg-%' OR username LIKE 'restart%' OR username LIKE 'idem-%'`,
	} {
		if _, err := p.pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("reset runtime state: %w", err)
		}
	}
	return nil
}

// --- AXB bindings ---------------------------------------------------

// UpsertAXBBinding writes one binding (ref keeps the process-local
// axb-N id stable across restarts).
func (p *PG) UpsertAXBBinding(ctx context.Context, b axb.Binding) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO axb_bindings (x_number, a_number, b_number, active, expire_at, ref)
        VALUES ($1, $2, $3, true, $4, $5)
        ON CONFLICT (x_number) DO UPDATE SET
            a_number = EXCLUDED.a_number, b_number = EXCLUDED.b_number,
            active = true, expire_at = EXCLUDED.expire_at, ref = EXCLUDED.ref`,
		b.XNumber, b.ANumber, b.BNumber, b.ExpireAt, b.ID)
	if err != nil {
		return fmt.Errorf("upsert axb binding: %w", err)
	}
	return nil
}

// DeactivateAXBByX marks the binding inactive (revoke).
func (p *PG) DeactivateAXBByX(ctx context.Context, x string) error {
	_, err := p.pool.Exec(ctx, `UPDATE axb_bindings SET active = false WHERE x_number = $1`, x)
	if err != nil {
		return fmt.Errorf("deactivate axb binding: %w", err)
	}
	return nil
}

// LoadAXBBindings returns live bindings for boot restore. Rows
// without ref (pre-014 leftovers) are skipped — Restore cannot map
// them into the by-id index and they expire naturally.
func (p *PG) LoadAXBBindings(ctx context.Context) ([]axb.Binding, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT x_number, a_number, b_number, expire_at, ref
        FROM axb_bindings
        WHERE active AND expire_at > now() AND ref IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("load axb bindings: %w", err)
	}
	defer rows.Close()
	var out []axb.Binding
	for rows.Next() {
		var b axb.Binding
		if err := rows.Scan(&b.XNumber, &b.ANumber, &b.BNumber, &b.ExpireAt, &b.ID); err != nil {
			return nil, err
		}
		b.Active = true
		out = append(out, b)
	}
	return out, rows.Err()
}

// PurgeAXBBindings removes bindings dead for over a day (hourly
// ticker alongside the idempotency purge).
func (p *PG) PurgeAXBBindings(ctx context.Context) (int64, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM axb_bindings WHERE expire_at < now() - interval '24 hours'`)
	if err != nil {
		return 0, fmt.Errorf("purge axb bindings: %w", err)
	}
	return tag.RowsAffected(), nil
}

// deliveryRetentionDays: how long webhook delivery-log rows live.
// Default 7 days; the table used to grow without bound.
func deliveryRetentionDays() int {
	if v := os.Getenv("GVOS_DELIVERY_TTL_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 7
}

// PurgeWebhookDeliveries trims the delivery log to the retention
// window.
func (p *PG) PurgeWebhookDeliveries(ctx context.Context, days int) (int64, error) {
	tag, err := p.pool.Exec(ctx, `DELETE FROM webhook_deliveries WHERE created_at < now() - make_interval(days => $1)`, days)
	if err != nil {
		return 0, fmt.Errorf("purge webhook deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// StartRuntimePurge runs the hourly dead-row sweep (idempotency +
// AXB + webhook delivery log). Blocks; run in a goroutine. Immediate
// first pass clears what accumulated while the process was down.
func (p *PG) StartRuntimePurge(ctx context.Context) {
	run := func() {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if n, err := p.PurgeIdemKeys(cctx); err != nil {
			log.Printf("idempotency purge failed: %v\n", err)
		} else if n > 0 {
			log.Printf("purged %d idempotency rows\n", n)
		}
		if n, err := p.PurgeAXBBindings(cctx); err != nil {
			log.Printf("axb purge failed: %v\n", err)
		} else if n > 0 {
			log.Printf("purged %d axb bindings\n", n)
		}
		if n, err := p.PurgeWebhookDeliveries(cctx, deliveryRetentionDays()); err != nil {
			log.Printf("delivery purge failed: %v\n", err)
		} else if n > 0 {
			log.Printf("purged %d webhook deliveries\n", n)
		}
	}
	run()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// CountTenants returns the tenant row count (stats counts block).
func (p *PG) CountTenants(ctx context.Context) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM tenants`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count tenants: %w", err)
	}
	return n, nil
}

// --- CDR history ----------------------------------------------------

// CDRQuery filters the CDR history read (admin/tenant lists).
type CDRQuery struct {
	Cursor    string    // opaque base64 keyset marker (start_time|call_id)
	Limit     int       // clamped 1..500 by the caller
	AccountID string    // exact account id (admin face)
	Accounts  []string  // tenant scope: only these accounts
	Dest      string    // exact destination
	CallID    string    // exact call id
	From, To  time.Time // zero = unbounded (To is exclusive)
}

// QueryCDRs reads CDR history keyset-paginated on
// (start_time, call_id) DESC — the stable order the memory ring
// cannot guarantee across its bounded eviction. next_cursor is empty
// on the last page. Rows map back into callcore.CDR so both modes
// emit the same JSON shape; fields without a table column (est_cost,
// free_secs) zero on restored history.
func (p *PG) QueryCDRs(ctx context.Context, q CDRQuery) ([]callcore.CDR, string, error) {
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 50
	}
	args := []any{}
	conds := []string{}
	var out []callcore.CDR
	next := ""
	bind := func(vals ...any) string {
		idx := make([]string, len(vals))
		for i, v := range vals {
			args = append(args, v)
			idx[i] = fmt.Sprintf("$%d", len(args))
		}
		return strings.Join(idx, ", ")
	}
	if q.Cursor != "" {
		st, cid, err := decodeCDRCursor(q.Cursor)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, fmt.Sprintf("(start_time, call_id) < (%s::timestamptz, %s)", bind(st), bind(cid)))
	}
	if q.AccountID != "" {
		conds = append(conds, fmt.Sprintf("account_id::text = %s", bind(q.AccountID)))
	}
	if len(q.Accounts) > 0 {
		conds = append(conds, fmt.Sprintf("account_id::text = ANY(%s)", bind(q.Accounts)))
	}
	if q.Dest != "" {
		conds = append(conds, fmt.Sprintf("dest = %s", bind(q.Dest)))
	}
	if q.CallID != "" {
		conds = append(conds, fmt.Sprintf("call_id = %s", bind(q.CallID)))
	}
	if !q.From.IsZero() {
		conds = append(conds, fmt.Sprintf("start_time >= %s", bind(q.From)))
	}
	if !q.To.IsZero() {
		conds = append(conds, fmt.Sprintf("start_time < %s", bind(q.To)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := p.pool.Query(ctx, `
        SELECT call_id, account_id::text, dest, coalesce(gateway_id::text, ''),
               start_time, answer_time, duration, billed_secs,
               coalesce(price::text, '0'), coalesce(cost::text, '0'),
               coalesce(dest_prefix, ''), coalesce(release, ''),
               coalesce(pdd_ms, 0), coalesce(sip_code, 0), recorded,
               coalesce(mos, 0), coalesce(jitter_ms, 0), coalesce(packet_loss, 0),
               coalesce(direction, 'out')
        FROM cdr`+where+`
        ORDER BY start_time DESC, call_id DESC`+fmt.Sprintf(" LIMIT $%d", len(args)+1), append(args, q.Limit)...)
	if err != nil {
		return nil, "", fmt.Errorf("query cdrs: %w", err)
	}
	defer rows.Close()
	out = make([]callcore.CDR, 0, q.Limit)
	for rows.Next() {
		var (
			c           callcore.CDR
			price, cost string
			answer      *time.Time
		)
		if err := rows.Scan(&c.CallID, &c.AccountID, &c.Dest, &c.GatewayID,
			&c.StartTime, &answer, &c.Duration, &c.BilledSecs,
			&price, &cost, &c.RatePrefix, &c.Release,
			&c.PDDMs, &c.SIPCode, &c.Recorded,
			&c.Mos, &c.JitterMs, &c.PacketLoss, &c.Direction); err != nil {
			return nil, "", err
		}
		if answer != nil {
			c.AnswerTime = *answer
		}
		if c.Price, err = money.FromDecimal(price); err != nil {
			return nil, "", fmt.Errorf("cdr %s price %q: %w", c.CallID, price, err)
		}
		if c.Cost, err = money.FromDecimal(cost); err != nil {
			return nil, "", fmt.Errorf("cdr %s cost %q: %w", c.CallID, cost, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if len(out) == q.Limit {
		last := out[len(out)-1]
		next = encodeCDRCursor(last.StartTime, last.CallID)
	}
	return out, next, nil
}

// encodeCDRCursor/decodeCDRCursor: the keyset marker rides as
// url-safe base64 of "RFC3339Nano|call_id".
func encodeCDRCursor(st time.Time, callID string) string {
	return base64.URLEncoding.EncodeToString([]byte(st.Format(time.RFC3339Nano) + "|" + callID))
}

func decodeCDRCursor(cur string) (time.Time, string, error) {
	raw, err := base64.URLEncoding.DecodeString(cur)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("bad cursor: %w", err)
	}
	i := strings.IndexByte(string(raw), '|')
	if i <= 0 {
		return time.Time{}, "", fmt.Errorf("bad cursor payload")
	}
	st, err := time.Parse(time.RFC3339Nano, string(raw[:i]))
	if err != nil {
		return time.Time{}, "", fmt.Errorf("bad cursor time: %w", err)
	}
	return st, string(raw[i+1:]), nil
}

// --- Audit / ledger reads -------------------------------------------

// AuditRow is one change record (who changed what, when, old/new).
type AuditRow struct {
	ID        int64           `json:"id"`
	TenantID  string          `json:"tenant_id"`
	Operator  string          `json:"operator"`
	Action    string          `json:"action"`
	Entity    string          `json:"entity"`
	EntityID  string          `json:"entity_id"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
	CreatedAt time.Time       `json:"created_at"`
}

// QueryAudit reads the audit trail newest-first, keyset-paginated on
// id. All filters optional. This is the read half that A7's
// "written but never readable" gap left out.
func (p *PG) QueryAudit(ctx context.Context, entity, entityID, tenantID string, limit int, cursor string) ([]AuditRow, string, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args := []any{}
	conds := []string{}
	bind := func(vals ...any) string {
		idx := make([]string, len(vals))
		for i, v := range vals {
			args = append(args, v)
			idx[i] = fmt.Sprintf("$%d", len(args))
		}
		return strings.Join(idx, ", ")
	}
	if entity != "" {
		conds = append(conds, fmt.Sprintf("entity = %s", bind(entity)))
	}
	if entityID != "" {
		conds = append(conds, fmt.Sprintf("entity_id = %s", bind(entityID)))
	}
	if tenantID != "" {
		conds = append(conds, fmt.Sprintf("tenant_id::text = %s", bind(tenantID)))
	}
	if cursor != "" {
		id, err := decodeIDCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, fmt.Sprintf("id < %s", bind(id)))
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := p.pool.Query(ctx, `
        SELECT id, coalesce(tenant_id::text, ''), operator, action, entity,
               coalesce(entity_id, ''), before, after, created_at
        FROM audit_log`+where+`
        ORDER BY id DESC`+fmt.Sprintf(" LIMIT $%d", len(args)+1), append(args, limit)...)
	if err != nil {
		return nil, "", fmt.Errorf("query audit: %w", err)
	}
	defer rows.Close()
	out := make([]AuditRow, 0, limit)
	for rows.Next() {
		var r AuditRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.Operator, &r.Action, &r.Entity,
			&r.EntityID, &r.Before, &r.After, &r.CreatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) == limit {
		next = encodeIDCursor(out[len(out)-1].ID)
	}
	return out, next, nil
}

// LedgerRow is one balance movement (delta, post-movement balance,
// why). The reconciliation face for the write path that has existed
// since migration 001 with no reader.
type LedgerRow struct {
	ID        int64     `json:"id"`
	AccountID string    `json:"account_id"`
	Delta     string    `json:"delta"`
	Balance   string    `json:"balance"`
	Reason    string    `json:"reason"`
	CallID    string    `json:"call_id"`
	Operator  string    `json:"operator"`
	CreatedAt time.Time `json:"created_at"`
}

// QueryLedger reads one account's balance movements newest-first,
// keyset-paginated on id.
func (p *PG) QueryLedger(ctx context.Context, accountID string, limit int, cursor string) ([]LedgerRow, string, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args := []any{}
	conds := []string{}
	bind := func(vals ...any) string {
		idx := make([]string, len(vals))
		for i, v := range vals {
			args = append(args, v)
			idx[i] = fmt.Sprintf("$%d", len(args))
		}
		return strings.Join(idx, ", ")
	}
	conds = append(conds, fmt.Sprintf("account_id::text = %s", bind(accountID)))
	if cursor != "" {
		id, err := decodeIDCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		conds = append(conds, fmt.Sprintf("id < %s", bind(id)))
	}
	rows, err := p.pool.Query(ctx, `
        SELECT id, account_id::text, delta::text, balance::text,
               reason, coalesce(call_id, ''), coalesce(operator, ''), created_at
        FROM balance_ledger WHERE `+strings.Join(conds, " AND ")+`
        ORDER BY id DESC`+fmt.Sprintf(" LIMIT $%d", len(args)+1), append(args, limit)...)
	if err != nil {
		return nil, "", fmt.Errorf("query ledger: %w", err)
	}
	defer rows.Close()
	out := make([]LedgerRow, 0, limit)
	for rows.Next() {
		var r LedgerRow
		if err := rows.Scan(&r.ID, &r.AccountID, &r.Delta, &r.Balance,
			&r.Reason, &r.CallID, &r.Operator, &r.CreatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) == limit {
		next = encodeIDCursor(out[len(out)-1].ID)
	}
	return out, next, nil
}

// encodeIDCursor/decodeIDCursor: opaque url-safe base64 of the keyset
// id (numeric PKs: audit_log, balance_ledger).
func encodeIDCursor(id int64) string {
	return base64.URLEncoding.EncodeToString([]byte(strconv.FormatInt(id, 10)))
}

func decodeIDCursor(cur string) (int64, error) {
	raw, err := base64.URLEncoding.DecodeString(cur)
	if err != nil {
		return 0, fmt.Errorf("bad cursor: %w", err)
	}
	id, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad cursor payload: %w", err)
	}
	return id, nil
}

// --- Account lifecycle (PATCH / soft-delete) ------------------------

// UpdateAccountPatch applies the optional-field changes of
// PATCH /v1/admin/accounts/{id}. Credit is deliberately absent —
// balance moves only through topup/settlement so the ledger stays
// the single money story.
func (p *PG) UpdateAccountPatch(ctx context.Context, id string, name, rateGroup *string, postpaid *bool, maxDebt *money.Amount, maxConcurrent, maxCPS *int, lowBalance *money.Amount) error {
	sets, args := []string{}, []any{}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if name != nil {
		sets = append(sets, "name = "+bind(*name))
	}
	if rateGroup != nil {
		sets = append(sets, "rate_group = "+bind(*rateGroup))
	}
	if postpaid != nil {
		sets = append(sets, "postpaid = "+bind(*postpaid))
	}
	if maxDebt != nil {
		sets = append(sets, "max_debt = "+bind(maxDebt.String()))
	}
	if maxConcurrent != nil {
		sets = append(sets, "max_concurrent = "+bind(*maxConcurrent))
	}
	if maxCPS != nil {
		sets = append(sets, "max_cps = "+bind(*maxCPS))
	}
	if lowBalance != nil {
		sets = append(sets, "low_balance_threshold = "+bind(lowBalance.String()))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, id)
	_, err := p.pool.Exec(ctx, `UPDATE accounts SET `+strings.Join(sets, ", ")+` WHERE id = `+fmt.Sprintf("$%d", len(args))+` AND deleted_at IS NULL`, args...)
	if err != nil {
		return fmt.Errorf("patch account: %w", err)
	}
	return nil
}

// CountAccountChildren reports live child accounts (delete guard).
func (p *PG) CountAccountChildren(ctx context.Context, id string) (int, error) {
	var n int
	if err := p.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE parent_id::text = $1 AND deleted_at IS NULL`, id).Scan(&n); err != nil {
		return 0, fmt.Errorf("count account children: %w", err)
	}
	return n, nil
}

// SoftDeleteAccount marks the row deleted; returns rows affected.
func (p *PG) SoftDeleteAccount(ctx context.Context, id string) (int, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE accounts SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return 0, fmt.Errorf("delete account: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// EndRateRows retires every live row of a prefix (rates keep history
// via ended_at; LoadRateRows only reads live ones). Returns the rows
// affected — 0 means the prefix had no live rate.
func (p *PG) EndRateRows(ctx context.Context, prefix string) (int, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE rates SET ended_at = now() WHERE prefix = $1 AND ended_at IS NULL`, prefix)
	if err != nil {
		return 0, fmt.Errorf("end rates: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// TenantDaySpend sums one day's rolled-up call count and cost for an
// account set (usage_daily is filled by the hourly rollup pass).
func (p *PG) TenantDaySpend(ctx context.Context, day string, accounts []string) (int, money.Amount, error) {
	if len(accounts) == 0 {
		return 0, 0, nil
	}
	var (
		n    int
		cost string
	)
	err := p.pool.QueryRow(ctx, `
        SELECT coalesce(sum(calls), 0), coalesce(sum(cost)::text, '0')
        FROM usage_daily
        WHERE day = $1::date AND account_id::text = ANY($2)`, day, accounts).Scan(&n, &cost)
	if err != nil {
		return 0, 0, fmt.Errorf("tenant day spend: %w", err)
	}
	amt, err := money.FromDecimal(cost)
	if err != nil {
		return 0, 0, err
	}
	return n, amt, nil
}

// TransferCredit moves credit between two accounts in one transaction
// (tenant→child allocation): balance check, both updates and both
// ledger rows (transfer-out / transfer-in) are atomic. The tenant's
// balance must cover the delta — a reseller cannot conjure credit.
func (p *PG) TransferCredit(ctx context.Context, fromID, toID string, delta money.Amount, operator string) (fromBal, toBal money.Amount, err error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("transfer begin: %w", err)
	}
	defer tx.Rollback(ctx) // safe no-op after commit

	var fromBalStr string
	if err := tx.QueryRow(ctx, `
        UPDATE accounts SET credit = credit - $1::numeric
        WHERE id = $2::uuid AND deleted_at IS NULL AND credit >= $1::numeric
        RETURNING credit::text`, delta.String(), fromID).Scan(&fromBalStr); err != nil {
		return 0, 0, fmt.Errorf("transfer from %s: %w", fromID, err)
	}
	var toBalStr string
	if err := tx.QueryRow(ctx, `
        UPDATE accounts SET credit = credit + $1::numeric
        WHERE id = $2::uuid AND deleted_at IS NULL
        RETURNING credit::text`, delta.String(), toID).Scan(&toBalStr); err != nil {
		return 0, 0, fmt.Errorf("transfer to %s: %w", toID, err)
	}
	for _, lr := range []struct {
		account string
		delta   string
		reason  string
	}{{fromID, "-" + delta.String(), "transfer-out"}, {toID, delta.String(), "transfer-in"}} {
		if _, err := tx.Exec(ctx, `
            INSERT INTO balance_ledger (account_id, delta, balance, reason, operator)
            VALUES ($1::uuid, $2::numeric, $3::numeric, $4, $5)`,
			lr.account, lr.delta, lr.account == fromID, lr.reason, operator); err != nil {
			return 0, 0, fmt.Errorf("transfer ledger: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("transfer commit: %w", err)
	}
	if fromBal, err = money.FromDecimal(fromBalStr); err != nil {
		return 0, 0, err
	}
	if toBal, err = money.FromDecimal(toBalStr); err != nil {
		return 0, 0, err
	}
	return fromBal, toBal, nil
}
