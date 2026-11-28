// Package store is the PostgreSQL persistence layer for the billing
// control plane. It implements the read paths (accounts, rates) and
// append paths (CDR, balance ledger, audit) against migrations/001_init.sql.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"runtime"
	"strconv"
	"time"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gvos-io/gvos-core/internal/billingdomain/account"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/callcore"
	"github.com/gvos-io/gvos-core/internal/customerdomain/blocklist"
	"github.com/gvos-io/gvos-core/internal/customerdomain/gateway"
	"github.com/gvos-io/gvos-core/internal/shared/metrics"
)

// Write-path counters for the /metrics exposition: the billing loss
// alarm is CDR/ledger write-rate dropping to zero, so these two are
// the most operationally valuable series on the dashboard.
var (
	cdrWritten    = metrics.Default.Counter("gvos_cdr_written_total", "CDR rows persisted to PostgreSQL")
	ledgerWritten = metrics.Default.Counter("gvos_ledger_written_total", "balance_ledger rows persisted to PostgreSQL")
)

// PG wraps the connection pool.
type PG struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*PG, error) {
	// Pool sizing: pgx defaults cap at max(4, NumCPU) connections with
	// MinConns=0; CDR bursts (BYE storms settling CDRW/CDRR workers in
	// parallel — env-tunable counts, see workers_dispatch.go) saturate
	// a small pool long before the CPU does. Scale with CPUs, floor 8,
	// ceiling 32; GVOS_PG_MAX_CONNS overrides for tuned deployments.
	maxConns := runtime.NumCPU() * 4
	if maxConns < 8 {
		maxConns = 8
	}
	if maxConns > 32 {
		maxConns = 32
	}
	if v := os.Getenv("GVOS_PG_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxConns = n
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = int32(maxConns)
	cfg.MinConns = int32(maxConns / 4)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &PG{pool: pool}, nil
}

func (p *PG) Close() { p.pool.Close() }

// LoadAccounts reads the account tree into an in-memory account.Store
// (the live reserve/commit surface). Soft-deleted rows are skipped.
func (p *PG) LoadAccounts(ctx context.Context, dst *account.Store) (int, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT id::text, coalesce(parent_id::text, ''), name, username,
               rate_group, postpaid, credit, max_debt, max_concurrent, max_cps,
               low_balance_threshold
        FROM accounts
        WHERE deleted_at IS NULL
        ORDER BY created_at`)
	if err != nil {
		return 0, fmt.Errorf("query accounts: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
			var (
			id, parent, name, username, group string
			postpaid                          bool
			credit, maxDebt, lowBal           string // NUMERIC scans as string; money.FromDecimal converts
			maxConc, maxCPS                   int
		)
		if err := rows.Scan(&id, &parent, &name, &username, &group, &postpaid, &credit, &maxDebt, &maxConc, &maxCPS, &lowBal); err != nil {
			return n, fmt.Errorf("scan account: %w", err)
		}
		creditAmt, err := money.FromDecimal(credit)
		if err != nil {
			return n, fmt.Errorf("account %s credit %q: %w", id, credit, err)
		}
		debtAmt, err := money.FromDecimal(maxDebt)
		if err != nil {
			return n, fmt.Errorf("account %s max_debt %q: %w", id, maxDebt, err)
		}
		lowBalAmt, err := money.FromDecimal(lowBal)
		if err != nil {
			return n, fmt.Errorf("account %s low_balance %q: %w", id, lowBal, err)
		}
		if err := dst.Put(&account.Account{
			ID: id, ParentID: parent, Name: name, Username: username,
			RateGroup: group, Postpaid: postpaid, Credit: creditAmt, MaxDebt: debtAmt,
			Concurrent: maxConc, CPS: maxCPS, LowBalance: lowBalAmt,
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// LoadRates reads one rate group's rows into a rate.Table. Time-windowed
// rows map to Effective intervals: days as weekday ints, start/end as
// "HH:MM" strings.
func (p *PG) LoadRates(ctx context.Context, group string) (*rate.Table, error) {
	rates, err := p.LoadRateRows(ctx, group)
	if err != nil {
		return nil, err
	}
	return rate.NewTable(rates), nil
}

// LoadRateRows returns the effective rates of a group as a flat slice
// — the same rows LoadRates wraps into a lookup Table. The admin API
// keeps this list so GET /v1/admin/rates reflects what billing uses
// (the Table itself has no enumeration).
func (p *PG) LoadRateRows(ctx context.Context, group string) ([]rate.Rate, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT prefix, price, init_dur, coalesce(init_price, 0),
               step_dur, min_dur, coalesce(days, '{}'::smallint[]),
               to_char(win_start, 'HH24:MI'), to_char(win_end, 'HH24:MI')
        FROM rates r
        JOIN rate_groups g ON g.id = r.group_id
        WHERE g.name = $1 AND r.ended_at IS NULL
        ORDER BY r.effective_at`, group)
	if err != nil {
		return nil, fmt.Errorf("query rates: %w", err)
	}
	defer rows.Close()

	var rates []rate.Rate
	for rows.Next() {
		var (
			prefix           string
			price, initPrice string
			initDur, stepDur int
			minDur           int
			days             []int16
			winStart, winEnd *string
		)
		if err := rows.Scan(&prefix, &price, &initDur, &initPrice, &stepDur, &minDur, &days, &winStart, &winEnd); err != nil {
			return nil, fmt.Errorf("scan rate: %w", err)
		}
		priceAmt, err := money.FromDecimal(price)
		if err != nil {
			return nil, fmt.Errorf("rate %s price %q: %w", prefix, price, err)
		}
		initAmt, err := money.FromDecimal(initPrice)
		if err != nil {
			return nil, fmt.Errorf("rate %s init_price %q: %w", prefix, initPrice, err)
		}
		r := rate.Rate{
			Prefix: prefix, Price: priceAmt,
			InitDur: initDur, InitRate: initAmt,
			StepDur: stepDur, MinDur: minDur,
		}
		if winStart != nil && winEnd != nil && *winStart != "" && *winEnd != "" {
			iv := rate.Interval{Start: *winStart, End: *winEnd}
			for _, d := range days {
				iv.Days = append(iv.Days, int(d))
			}
			r.Effective = []rate.Interval{iv}
		}
		rates = append(rates, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return rates, nil
}

// LoadGateways reads the legacy gateways table as the restart
// fallback (used once, when no "gateways" setting exists — the
// admin API owns the live set from then on and never writes here).
// Priority maps onto the dispatcher-compatible dspriority column;
// missing columns default to 0 = first choice.
func (p *PG) LoadGateways(ctx context.Context) ([]gateway.Gateway, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT id::text, name, sip_addr, coalesce(protocol, 'udp'),
               coalesce(dspriority, 0), coalesce(weight, 100),
               (locked_until IS NULL OR locked_until < now())
        FROM gateways
        WHERE deleted_at IS NULL
        ORDER BY coalesce(dspriority, 0), name`)
	if err != nil {
		return nil, fmt.Errorf("query gateways: %w", err)
	}
	defer rows.Close()

	var gws []gateway.Gateway
	for rows.Next() {
		var g gateway.Gateway
		if err := rows.Scan(&g.ID, &g.Name, &g.SipAddr, &g.Protocol, &g.Priority, &g.Weight, &g.Enabled); err != nil {
			return nil, fmt.Errorf("scan gateway: %w", err)
		}
		gws = append(gws, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return gws, nil
}

// insertCDRSQL is the cdr write shared by InsertCDR and
// InsertCDRWithSettle (ON CONFLICT DO NOTHING makes it idempotent
// under redelivery: uq_cdr_call, migration 008). direction/gateway_id
// (migration 013) default on the column: legacy CDRs and old callers
// that leave them empty persist 'out'/NULL exactly as before.
const insertCDRSQL = `
        INSERT INTO cdr (call_id, tenant_id, account_id, dest, dest_prefix,
                         start_time, answer_time, end_time, duration, billed_secs,
                         price, cost, release, pdd_ms, sip_code, recorded,
                         mos, jitter_ms, packet_loss, direction, gateway_id)
        VALUES ($1, '00000000-0000-0000-0000-000000000001', $2::uuid, $3, $4,
                $5::timestamptz,
                CASE WHEN $17::timestamptz <= '1970-01-01' THEN NULL ELSE $17::timestamptz END,
                $5::timestamptz + ($6::int * interval '1 second'),
                $6::int, $7::int, $8::numeric, $9::numeric, $10, $11::int, $12::int, $13::bool,
                $14::real, $15::real, $16::real,
                CASE WHEN $18 = '' THEN 'out' ELSE $18 END,
                NULLIF($19, '')::uuid)
        ON CONFLICT DO NOTHING`

// cdrArgs binds a CDR to insertCDRSQL. Money crosses as decimal
// strings (ADR-0003): the NUMERIC columns must receive yuan, never the
// scaled int64 (money.Amount.Value() guards the direct form too).
func cdrArgs(c *callcore.CDR) []any {
	return []any{
		c.CallID, c.AccountID, c.Dest, c.RatePrefix,
		c.StartTime, c.Duration, c.BilledSecs, c.Price.String(), c.Cost.String(), c.Release, c.PDDMs, c.SIPCode, c.Recorded,
		c.Mos, c.JitterMs, c.PacketLoss, c.AnswerTime,
		c.Direction, c.GatewayID,
	}
}

// InsertCDR persists one billing record into the monthly-partitioned
// cdr table. Idempotent under redelivery (uq_cdr_call, migration 008):
// returns inserted=false when the row already exists — the caller then
// skips dependent writes (ledger) so at-least-once delivery produces
// exactly-once effects.
func (p *PG) InsertCDR(ctx context.Context, c *callcore.CDR) (bool, error) {
	tag, err := p.pool.Exec(ctx, insertCDRSQL, cdrArgs(c)...)
	if err != nil {
		return false, fmt.Errorf("insert cdr: %w", err)
	}
	if tag.RowsAffected() > 0 {
		cdrWritten.Inc(1)
	}
	return tag.RowsAffected() > 0, nil
}

// InsertCDRWithSettle persists one billing record together with its
// settlement ledger row — atomically and idempotently. With
// deductCredit set (no Redis: the in-memory balance is the authority
// and accounts.credit mirrors it) the call's cost also comes off
// accounts.credit in the same transaction. A redelivery finds the CDR
// row, writes nothing and reports inserted=false: at-least-once
// delivery, exactly-once money. bal is the post-settlement balance
// snapshot for the ledger row.
func (p *PG) InsertCDRWithSettle(ctx context.Context, c *callcore.CDR, bal money.Amount, deductCredit bool) (bool, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("settle begin: %w", err)
	}
	defer tx.Rollback(ctx) // safe no-op after commit

	tag, err := tx.Exec(ctx, insertCDRSQL, cdrArgs(c)...)
	if err != nil {
		return false, fmt.Errorf("insert cdr: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("settle commit (redelivery): %w", err)
		}
		return false, nil
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO balance_ledger (account_id, delta, balance, reason, call_id)
        VALUES ($1::uuid, $2::numeric, $3::numeric, $4, $5)`,
		c.AccountID, (-c.Cost).String(), bal.String(), "call-settle", c.CallID); err != nil {
		return false, fmt.Errorf("insert ledger: %w", err)
	}
	if deductCredit {
		// A prepaid account's credit never goes below zero here: the
		// schema's CHECK forbids it and a violated CHECK would roll the
		// whole settlement (CDR included) back. The remainder of a cost
		// paid out of a credit line is forgiven in the mirror.
		if _, err := tx.Exec(ctx, `
            UPDATE accounts
               SET credit = credit - CASE WHEN postpaid THEN $1::numeric
                                          ELSE LEAST($1::numeric, GREATEST(credit, 0)) END
             WHERE id = $2::uuid`, c.Cost.String(), c.AccountID); err != nil {
			return false, fmt.Errorf("deduct credit: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("settle commit: %w", err)
	}
	cdrWritten.Inc(1)
	ledgerWritten.Inc(1)
	return true, nil
}

// EnsureCDRPartitions makes sure a monthly partition exists for this
// month and the next `ahead` months. The schema ships partitions for a
// fixed window only; the first month without one fails every CDR
// insert (the durable consumer gives up after its redeliveries). Runs
// at boot and on the hourly report pass, so no cron can forget it.
// Idempotent; needs CREATE on the schema (the app role owns it).
func (p *PG) EnsureCDRPartitions(ctx context.Context, ahead int) error {
	if ahead < 0 {
		ahead = 0
	}
	now := time.Now()
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= ahead; i++ {
		from := first.AddDate(0, i, 0)
		to := from.AddDate(0, 1, 0)
		name := fmt.Sprintf("cdr_%04d_%02d", from.Year(), int(from.Month()))
		if _, err := p.pool.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF cdr FOR VALUES FROM ('%s') TO ('%s')`,
			name, from.Format("2006-01-02"), to.Format("2006-01-02"))); err != nil {
			return fmt.Errorf("ensure partition %s: %w", name, err)
		}
	}
	return nil
}

// AppendLedger writes a balance movement row (append-only).
func (p *PG) AppendLedger(ctx context.Context, accountID string, delta, balance money.Amount, reason, callID string) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO balance_ledger (account_id, delta, balance, reason, call_id)
	VALUES ($1::uuid, $2::numeric, $3::numeric, $4, $5)`,
		accountID, delta.String(), balance.String(), reason, callID)
	if err == nil {
		ledgerWritten.Inc(1)
	}
	return err
}

// HasLedgerForCall reports whether a call-settle ledger row already
// exists for callID (CDRW redelivery must not skip a missing ledger
// after a prior InsertCDR succeeded).
func (p *PG) HasLedgerForCall(ctx context.Context, callID string) (bool, error) {
	if callID == "" {
		return false, nil
	}
	var n int
	err := p.pool.QueryRow(ctx, `
        SELECT COUNT(*) FROM balance_ledger
        WHERE call_id = $1 AND reason = 'call-settle'`, callID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("ledger exists: %w", err)
	}
	return n > 0, nil
}

// AppendAudit writes one audit-log row. before/after accept any
// JSON-marshalable value or nil.
func (p *PG) AppendAudit(ctx context.Context, tenantID, operator, action, entity, entityID string, before, after any) error {
	b, err := marshalOrNull(before)
	if err != nil {
		return err
	}
	a, err := marshalOrNull(after)
	if err != nil {
		return err
	}
	_, err = p.pool.Exec(ctx, `
        INSERT INTO audit_log (tenant_id, operator, action, entity, entity_id, before, after)
        VALUES ($1::uuid, $2, $3, $4, $5, $6::jsonb, $7::jsonb)`,
		tenantID, operator, action, entity, entityID, b, a)
	return err
}

// CreateAccountWithHA1 inserts an account row with its SIP Digest
// credential and returns the UUID.
func (p *PG) CreateAccountWithHA1(ctx context.Context, tenantID, parentID, name, username, ha1, rateGroup string, postpaid bool, credit money.Amount) (string, error) {
	var id string
	err := p.pool.QueryRow(ctx, `
        INSERT INTO accounts (tenant_id, parent_id, name, username, secret_hash,
                              ha1, rate_group, postpaid, credit)
        VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9::numeric)
        RETURNING id::text`,
		tenantID, nullIfEmpty(parentID), name, username, "x", ha1, rateGroup, postpaid, credit.String()).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create account: %w", err)
	}
	return id, nil
}

// WriteBalance sets an account's credit to an absolute value (Redis
// snapshot write-back path; no ledger row — the ledger stays
// call/topup-driven).
func (p *PG) WriteBalance(ctx context.Context, accountID string, credit money.Amount) error {
	_, err := p.pool.Exec(ctx, `UPDATE accounts SET credit = $1::numeric WHERE id = $2::uuid`, credit.String(), accountID)
	if err != nil {
		return fmt.Errorf("write balance: %w", err)
	}
	return nil
}

// TopUp adjusts an account balance (delta signed) and returns the new
// balance; it also appends the ledger row so topup and adjustment stay
// in the append-only history.
func (p *PG) TopUp(ctx context.Context, accountID string, delta money.Amount, reason, operator string) (money.Amount, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("topup begin: %w", err)
	}
	defer tx.Rollback(ctx) // safe no-op after commit

	var balance string
	if err := tx.QueryRow(ctx, `
        UPDATE accounts SET credit = credit + $1::numeric
        WHERE id = $2::uuid AND deleted_at IS NULL
        RETURNING credit::text`, delta.String(), accountID).Scan(&balance); err != nil {
		return 0, fmt.Errorf("topup update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO balance_ledger (account_id, delta, balance, reason, operator)
        VALUES ($1::uuid, $2::numeric, $3::numeric, $4, $5)`,
		accountID, delta.String(), balance, reason, operator); err != nil {
		return 0, fmt.Errorf("topup ledger: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("topup commit: %w", err)
	}
	bal, err := money.FromDecimal(balance)
	if err != nil {
		return 0, fmt.Errorf("topup balance %q: %w", balance, err)
	}
	return bal, nil
}

// UpsertRate replaces the current rate for a prefix: superseded rows
// are marked ended_at (the loader reads only live rows), and a fresh
// row carries the new price - so a price change is a version, not a
// mutation (audit trail intact).
func (p *PG) UpsertRate(ctx context.Context, groupID, prefix string, price money.Amount, initDur, stepDur, minDur int) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("upsert rate begin: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
        UPDATE rates SET ended_at = now()
        WHERE group_id = $1::uuid AND prefix = $2 AND ended_at IS NULL`,
		groupID, prefix); err != nil {
		return fmt.Errorf("upsert rate supersede: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO rates (group_id, prefix, price, init_dur, step_dur, min_dur, effective_at)
        VALUES ($1::uuid, $2, $3, $4, $5, $6, now())`,
		groupID, prefix, price.String(), initDur, stepDur, minDur); err != nil {
		return fmt.Errorf("upsert rate insert: %w", err)
	}
	return tx.Commit(ctx)
}

// LoadBlocklist reads number_lists into a blocklist.List.
func (p *PG) LoadBlocklist(ctx context.Context, dst *blocklist.List) (int, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT coalesce(tenant_id::text, ''), kind, scope, pattern, is_regex
        FROM number_lists`)
	if err != nil {
		return 0, fmt.Errorf("query number_lists: %w", err)
	}
	defer rows.Close()

	var entries []blocklist.Entry
	for rows.Next() {
		var e blocklist.Entry
		if err := rows.Scan(&e.TenantID, &e.Kind, &e.Scope, &e.Pattern, &e.IsRegex); err != nil {
			return len(entries), fmt.Errorf("scan list entry: %w", err)
		}
		entries = append(entries, e)
	}
	n := len(entries)
	dst.Reload(entries)
	return n, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UpsertNumberList persists one screening rule (the admin add path).
// The in-memory Add stays the fast path; this is the durability copy
// so restarts and replicas load the same ruleset.
func (p *PG) UpsertNumberList(ctx context.Context, e blocklist.Entry) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO number_lists (tenant_id, kind, scope, pattern, is_regex)
        VALUES ($1::uuid, $2, $3, $4, $5)`,
		nullIfEmpty(e.TenantID), e.Kind, e.Scope, e.Pattern, e.IsRegex)
	if err != nil {
		return fmt.Errorf("insert number_lists: %w", err)
	}
	return nil
}

// RemoveNumberList deletes matching screening rules (admin remove
// path); returns how many rows went away.
func (p *PG) RemoveNumberList(ctx context.Context, tenant, kind, scope, pattern string) (int, error) {
	tag, err := p.pool.Exec(ctx, `
        DELETE FROM number_lists
        WHERE ((tenant_id::text = $1) OR ($1 = '' AND tenant_id IS NULL))
          AND kind = $2 AND scope = $3 AND pattern = $4`,
		tenant, kind, scope, pattern)
	if err != nil {
		return 0, fmt.Errorf("delete number_lists: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// PutSetting stores one global runtime setting (recording toggle, risk
// rules, package snapshot) as JSONB. The broadcast notifies peers
// instantly; this row is what restarts load back.
func (p *PG) PutSetting(ctx context.Context, key string, value any) error {
	v, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal setting %s: %w", key, err)
	}
	_, err = p.pool.Exec(ctx, `
        INSERT INTO settings (key, value, updated_at)
        VALUES ($1, $2::jsonb, now())
        ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		key, string(v))
	if err != nil {
		return fmt.Errorf("upsert setting %s: %w", key, err)
	}
	return nil
}

// GetSetting reads a setting back into dest (json.Unmarshal target).
// Returns os.ErrNotExist-equivalent: (nil, ErrSettingNotFound) when
// the key was never set — callers treat that as "use the default".
var ErrSettingNotFound = errors.New("setting not found")

func (p *PG) GetSetting(ctx context.Context, key string, dest any) error {
	var raw []byte
	err := p.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSettingNotFound
	}
	if err != nil {
		return fmt.Errorf("query setting %s: %w", key, err)
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("unmarshal setting %s: %w", key, err)
	}
	return nil
}

func marshalOrNull(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("marshal audit value: %w", err)
	}
	return string(b), nil
}

// RollupDaily aggregates one day's CDRs into usage_daily (idempotent:
// the row is replaced). This is the T+1 worker's core query.
func (p *PG) RollupDaily(ctx context.Context, day string) (int, error) {
	tag, err := p.pool.Exec(ctx, `
        INSERT INTO usage_daily (day, tenant_id, account_id, calls, answered,
                                 duration, billed_secs, cost)
        SELECT start_time::date,
               tenant_id,
               account_id,
               count(*),
               count(*) FILTER (WHERE answer_time IS NOT NULL OR duration > 0),
               coalesce(sum(duration), 0),
               coalesce(sum(billed_secs), 0),
               coalesce(sum(cost), 0)
        FROM cdr
        WHERE start_time::date = $1::date
        GROUP BY 1, tenant_id, account_id
        ON CONFLICT (day, tenant_id, account_id) DO UPDATE SET
            calls = EXCLUDED.calls,
            answered = EXCLUDED.answered,
            duration = EXCLUDED.duration,
            billed_secs = EXCLUDED.billed_secs,
            cost = EXCLUDED.cost`,
		day)
	if err != nil {
		return 0, fmt.Errorf("rollup: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// DailyReport rows for one account (or all when accountID is empty).
type DailyReportRow struct {
	Day        string  `json:"day"`
	AccountID  string  `json:"account_id"`
	Calls      int     `json:"calls"`
	Answered   int     `json:"answered"`
	Duration   int     `json:"duration"`
	BilledSecs int     `json:"billed_secs"`
	Cost       float64 `json:"cost"`
}

// QueryDaily reads rolled-up rows ordered by day.
func (p *PG) QueryDaily(ctx context.Context, day, accountID string) ([]DailyReportRow, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT day::text, account_id::text, calls, answered, duration, billed_secs, cost
        FROM usage_daily
        WHERE ($1 = '' OR day = $1::date)
          AND ($2 = '' OR account_id::text = $2)
        ORDER BY day DESC, cost DESC
        LIMIT 500`, day, accountID)
	if err != nil {
		return nil, fmt.Errorf("query daily: %w", err)
	}
	defer rows.Close()
	var out []DailyReportRow
	for rows.Next() {
		var r DailyReportRow
		if err := rows.Scan(&r.Day, &r.AccountID, &r.Calls, &r.Answered, &r.Duration, &r.BilledSecs, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GatewayDaily aggregates answer-seizure ratio per gateway for one day
// (the ASR/ACD/PDD report the PRD asks for).
type GatewayReportRow struct {
	Day       string  `json:"day"`
	GatewayID string  `json:"gateway_id"`
	Calls     int     `json:"calls"`
	Answered  int     `json:"answered"`
	ASR       float64 `json:"asr"`
	ACDSecs   float64 `json:"acd_secs"`
	Cost      float64 `json:"cost"`
}

// QueryGatewayDaily reads live CDR aggregates per gateway (day ” = all).
func (p *PG) QueryGatewayDaily(ctx context.Context, day string) ([]GatewayReportRow, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT d.day::text,
               d.gw,
               d.calls,
               d.ans,
               CASE WHEN d.calls = 0 THEN 0 ELSE d.ans::float / d.calls END,
               CASE WHEN d.ans = 0 THEN 0 ELSE d.dur::float / d.ans END,
               d.cost
        FROM (
            SELECT start_time::date AS day,
                   coalesce(gateway_id::text, 'unknown') AS gw,
                   count(*) AS calls,
                   count(*) FILTER (WHERE duration > 0) AS ans,
                   coalesce(sum(duration), 0) AS dur,
                   coalesce(sum(cost), 0) AS cost
            FROM cdr
            WHERE ($1 = '' OR start_time::date = $1::date)
            GROUP BY 1, 2
        ) d
        ORDER BY d.day DESC, d.calls DESC
        LIMIT 200`, day)
	if err != nil {
		return nil, fmt.Errorf("query gateway daily: %w", err)
	}
	defer rows.Close()
	var out []GatewayReportRow
	for rows.Next() {
		var r GatewayReportRow
		if err := rows.Scan(&r.Day, &r.GatewayID, &r.Calls, &r.Answered, &r.ASR, &r.ACDSecs, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Blind-zone analysis (盲区分析) ----

// BlindZoneRow is one blind-zone finding: destinations you are blind
// to - either unrated (no price matched) or unroutable (never answers).
type BlindZoneRow struct {
	Prefix   string  `json:"prefix"`
	Calls    int     `json:"calls"`
	Answered int     `json:"answered"`
	ASR      float64 `json:"asr"`
	Reason   string  `json:"reason"` // unrated | dead-route
}

// QueryBlindZones finds destinations that cost money to dial but
// return nothing: calls whose rate lookup missed (dest_prefix IS NULL)
// and destination prefixes with >= minCalls attempts and zero answers.
func (p *PG) QueryBlindZones(ctx context.Context, minCalls int) ([]BlindZoneRow, error) {
	if minCalls < 1 {
		minCalls = 5
	}
	rows, err := p.pool.Query(ctx, `
        SELECT prefix, calls, ans, 0::float8, reason FROM (
            SELECT substr(dest, 1, 4) AS prefix, count(*) AS calls,
                   count(*) FILTER (WHERE duration > 0) AS ans,
                   'unrated' AS reason
            FROM cdr
            WHERE dest_prefix IS NULL
            GROUP BY 1
            HAVING count(*) >= $1
            UNION ALL
            SELECT substr(dest, 1, 6) AS prefix, count(*) AS calls,
                   count(*) FILTER (WHERE duration > 0) AS ans,
                   'dead-route' AS reason
            FROM cdr
            WHERE dest_prefix IS NOT NULL
            GROUP BY 1
            HAVING count(*) >= $1 AND count(*) FILTER (WHERE duration > 0) = 0
        ) z
        ORDER BY z.calls DESC
        LIMIT 100`, minCalls)
	if err != nil {
		return nil, fmt.Errorf("query blind zones: %w", err)
	}
	defer rows.Close()
	var out []BlindZoneRow
	for rows.Next() {
		var r BlindZoneRow
		if err := rows.Scan(&r.Prefix, &r.Calls, &r.Answered, &r.ASR, &r.Reason); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// CrossRow is one cell of the account x destination-cluster matrix
// (the practical form of VOS's caller/destination cross analysis:
// which accounts dial which destination clusters, and how healthy
// each lane is).
type CrossRow struct {
	AccountID   string  `json:"account_id"`
	DestCluster string  `json:"dest_cluster"` // first 2-3 digits of the destination
	Calls       int     `json:"calls"`
	Answered    int     `json:"answered"`
	ASR         float64 `json:"asr"`
	Cost        float64 `json:"cost"`
}

// QueryCrossAnalysis returns the account x destination matrix.
func (p *PG) QueryCrossAnalysis(ctx context.Context, day string) ([]CrossRow, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT d.acc, d.cluster, d.calls, d.ans,
               CASE WHEN d.calls = 0 THEN 0 ELSE d.ans::float8 / d.calls END,
               d.cost
        FROM (
            SELECT account_id::text AS acc,
                   CASE
                     WHEN dest LIKE '86%'  THEN 'CN(86)'
                     WHEN dest LIKE '1%'   THEN 'US/CA(1)'
                     WHEN dest LIKE '44%'  THEN 'UK(44)'
                     WHEN dest LIKE '91%'  THEN 'IN(91)'
                     ELSE 'other(' || substr(dest, 1, 2) || ')'
                   END AS cluster,
                   count(*) AS calls,
                   count(*) FILTER (WHERE duration > 0) AS ans,
                   coalesce(sum(cost), 0) AS cost
            FROM cdr
            WHERE ($1 = '' OR start_time::date = $1::date)
            GROUP BY 1, 2
        ) d
        ORDER BY d.cost DESC
        LIMIT 200`, day)
	if err != nil {
		return nil, fmt.Errorf("query cross: %w", err)
	}
	defer rows.Close()
	var out []CrossRow
	for rows.Next() {
		var r CrossRow
		if err := rows.Scan(&r.AccountID, &r.DestCluster, &r.Calls, &r.Answered, &r.ASR, &r.Cost); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertRecording indexes one recording artifact (recpipe pipeline;
// migration 009). Idempotent per (call_id, track, format).
func (p *PG) UpsertRecording(ctx context.Context, callID, track, objectKey, locator, format string, sizeBytes int64) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO recordings (call_id, track, object_key, locator, format, size_bytes, status)
        VALUES ($1, $2, $3, $4, $5, $6, 'ready')
        ON CONFLICT (call_id, track, format) DO UPDATE
        SET object_key = $3, locator = $4, size_bytes = $6, status = 'ready', updated_at = now()`,
		callID, track, objectKey, locator, format, sizeBytes)
	if err != nil {
		return fmt.Errorf("upsert recording: %w", err)
	}
	return nil
}

// GetRecordings lists the tracks of one call for the play endpoint.
func (p *PG) GetRecordings(ctx context.Context, callID string) ([]RecordingRow, error) {
	rows, err := p.pool.Query(ctx, `
        SELECT track, object_key, locator, format, size_bytes, status
        FROM recordings WHERE call_id = $1 AND status = 'ready'`, callID)
	if err != nil {
		return nil, fmt.Errorf("get recordings: %w", err)
	}
	defer rows.Close()
	var out []RecordingRow
	for rows.Next() {
		var r RecordingRow
		if err := rows.Scan(&r.Track, &r.ObjectKey, &r.Locator, &r.Format, &r.SizeBytes, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordingRow is one recordings-table row (play endpoint face).
type RecordingRow struct {
	Track     string
	ObjectKey string
	Locator   string
	Format    string
	SizeBytes int64
	Status    string
}

// --- webhook subscription storage (api.WebhookStore face) ---

// WebhookRow is one subscription row (Secret rides along: the
// dispatcher signs with it; the admin list endpoint must not echo it).
type WebhookRow struct {
	ID       string
	URL      string
	Secret   string
	Events   []string
	Active   bool
	TenantID string
}

// CreateWebhook inserts one subscription.
func (p *PG) CreateWebhook(ctx context.Context, url, secret string, events []string) (string, error) {
	return p.CreateWebhookTenant(ctx, url, secret, events, "")
}

// CreateWebhookTenant inserts one subscription bound to a tenant
// (Phase C tenant plane) or to the admin ("" tenant).
func (p *PG) CreateWebhookTenant(ctx context.Context, url, secret string, events []string, tenantID string) (string, error) {
	var id string
	err := p.pool.QueryRow(ctx, `INSERT INTO webhook_subscriptions (url, secret, events, tenant_id) VALUES ($1, $2, $3, $4) RETURNING id::text`, url, secret, events, tenantID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create webhook: %w", err)
	}
	return id, nil
}

// ListWebhooks returns active subscriptions (secrets included — the
// dispatcher needs them to sign; the admin API never echoes them).
func (p *PG) ListWebhooks(ctx context.Context) ([]WebhookRow, error) {
	rows, err := p.pool.Query(ctx, `SELECT id::text, url, secret, events, active, tenant_id FROM webhook_subscriptions WHERE active`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookRow
	for rows.Next() {
		var r WebhookRow
		if err := rows.Scan(&r.ID, &r.URL, &r.Secret, &r.Events, &r.Active, &r.TenantID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteWebhook deactivates one subscription (soft delete).
func (p *PG) DeleteWebhook(ctx context.Context, id string) (int, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE webhook_subscriptions SET active = false WHERE id = $1::uuid`, id)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// UpdateWebhook patches url/events/active (secret never changes —
// rotating it would silently break delivery verification for the
// customer; delete + re-create issues a deliberate new one).
func (p *PG) UpdateWebhook(ctx context.Context, id string, url *string, events []string, active *bool) (int, error) {
	sets, args := []string{}, []any{}
	bind := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	if url != nil {
		sets = append(sets, "url = "+bind(*url))
	}
	if events != nil {
		sets = append(sets, "events = "+bind(events))
	}
	if active != nil {
		sets = append(sets, "active = "+bind(*active))
	}
	if len(sets) == 0 {
		return 1, nil // nothing to do: idempotent no-op
	}
	args = append(args, id)
	tag, err := p.pool.Exec(ctx, `UPDATE webhook_subscriptions SET `+strings.Join(sets, ", ")+` WHERE id = `+fmt.Sprintf("$%d::uuid", len(args)), args...)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// DeleteWebhookTenant revokes one tenant-owned subscription
// (ownership in the WHERE — a tenant can never delete another
// tenant's or an operator-created endpoint).
func (p *PG) DeleteWebhookTenant(ctx context.Context, id, tenantID string) (int, error) {
	tag, err := p.pool.Exec(ctx, `UPDATE webhook_subscriptions SET active = false WHERE id = $1::uuid AND tenant_id::text = $2`, id, tenantID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// WebhookDeliveryRow is one delivery-log entry for admin troubleshooting.
type WebhookDeliveryRow struct {
	ID        int64
	SubID     string
	URL       string
	Subject   string
	Payload   string
	Status    string
	Attempts  int
	LastError string
	TenantID  string
	CreatedAt time.Time
}

// InsertWebhookDelivery inserts one delivery trail row.
func (p *PG) InsertWebhookDelivery(ctx context.Context, subID, url, subject, payload, status, lastError, tenantID string, attempts int) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (sub_id, url, subject, payload, status, attempts, last_error, tenant_id)
		VALUES (NULLIF($1,'')::uuid, $2, $3, $4, $5, $6, NULLIF($7,''), $8)`,
		subID, url, subject, payload, status, attempts, lastError, tenantID)
	if err != nil {
		return fmt.Errorf("log webhook delivery: %w", err)
	}
	return nil
}

// ListWebhookDeliveries returns recent deliveries for one subscription
// (or all when subID is empty).
func (p *PG) ListWebhookDeliveries(ctx context.Context, subID string, limit int, cursor string) ([]WebhookDeliveryRow, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var (
		rows pgx.Rows
		err  error
	)
	cursorSQL := ""
	args := []any{limit}
	if cursor != "" {
		id, derr := decodeIDCursor(cursor)
		if derr != nil {
			return nil, "", derr
		}
		cursorSQL = " AND id < $2"
		args = append(args, id)
	}
	if subID == "" {
		rows, err = p.pool.Query(ctx, `
			SELECT id, COALESCE(sub_id::text,''), url, subject, payload, status, attempts,
			       COALESCE(last_error,''), COALESCE(tenant_id,''), created_at
			FROM webhook_deliveries
			WHERE true`+cursorSQL+`
			ORDER BY id DESC LIMIT $1`, args...)
	} else {
		rows, err = p.pool.Query(ctx, `
			SELECT id, COALESCE(sub_id::text,''), url, subject, payload, status, attempts,
			       COALESCE(last_error,''), COALESCE(tenant_id,''), created_at
			FROM webhook_deliveries
			WHERE sub_id = $1::uuid`+cursorSQL+`
			ORDER BY id DESC LIMIT $2`, append([]any{subID}, args...)...)
	}
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []WebhookDeliveryRow
	for rows.Next() {
		var r WebhookDeliveryRow
		if err := rows.Scan(&r.ID, &r.SubID, &r.URL, &r.Subject, &r.Payload, &r.Status, &r.Attempts, &r.LastError, &r.TenantID, &r.CreatedAt); err != nil {
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

// GetWebhookDelivery loads one delivery by id.
func (p *PG) GetWebhookDelivery(ctx context.Context, id int64) (*WebhookDeliveryRow, error) {
	var r WebhookDeliveryRow
	err := p.pool.QueryRow(ctx, `
		SELECT id, COALESCE(sub_id::text,''), url, subject, payload, status, attempts,
		       COALESCE(last_error,''), COALESCE(tenant_id,''), created_at
		FROM webhook_deliveries WHERE id = $1`, id).
		Scan(&r.ID, &r.SubID, &r.URL, &r.Subject, &r.Payload, &r.Status, &r.Attempts, &r.LastError, &r.TenantID, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// GetWebhookByID returns one subscription (incl. secret) by id.
func (p *PG) GetWebhookByID(ctx context.Context, id string) (*WebhookRow, error) {
	var r WebhookRow
	err := p.pool.QueryRow(ctx, `
		SELECT id::text, url, secret, events, active, tenant_id
		FROM webhook_subscriptions WHERE id = $1::uuid`, id).
		Scan(&r.ID, &r.URL, &r.Secret, &r.Events, &r.Active, &r.TenantID)
	if err != nil {
		return nil, err
	}
	return &r, nil
}
