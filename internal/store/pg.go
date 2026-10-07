// Package store is the PostgreSQL persistence layer for the billing
// control plane. It implements the read paths (accounts, rates) and
// append paths (CDR, balance ledger, audit) against migrations/001_init.sql.
package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gvos/gvos-core/internal/account"
	"github.com/gvos/gvos-core/internal/blocklist"
	"github.com/gvos/gvos-core/internal/callstate"
	"github.com/gvos/gvos-core/internal/rate"
)

// PG wraps the connection pool.
type PG struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*PG, error) {
	pool, err := pgxpool.New(ctx, dsn)
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
               rate_group, postpaid, credit, max_debt, max_concurrent, max_cps
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
			credit, maxDebt                   float64
			maxConc, maxCPS                   int
		)
		if err := rows.Scan(&id, &parent, &name, &username, &group, &postpaid, &credit, &maxDebt, &maxConc, &maxCPS); err != nil {
			return n, fmt.Errorf("scan account: %w", err)
		}
		if err := dst.Put(&account.Account{
			ID: id, ParentID: parent, Name: name, Username: username,
			RateGroup: group, Postpaid: postpaid, Credit: credit, MaxDebt: maxDebt,
			Concurrent: maxConc, CPS: maxCPS,
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
			prefix            string
			price, initPrice  float64
			initDur, stepDur  int
			minDur            int
			days              []int16
			winStart, winEnd  *string
		)
		if err := rows.Scan(&prefix, &price, &initDur, &initPrice, &stepDur, &minDur, &days, &winStart, &winEnd); err != nil {
			return nil, fmt.Errorf("scan rate: %w", err)
		}
		r := rate.Rate{
			Prefix: prefix, Price: price,
			InitDur: initDur, InitRate: initPrice,
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
	return rate.NewTable(rates), nil
}

// InsertCDR persists one billing record into the monthly-partitioned
// cdr table.
func (p *PG) InsertCDR(ctx context.Context, c *callstate.CDR) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO cdr (call_id, tenant_id, account_id, dest, dest_prefix,
                         start_time, end_time, duration, billed_secs,
                         price, cost, release, pdd_ms, sip_code, recorded)
        VALUES ($1, '00000000-0000-0000-0000-000000000001', $2::uuid, $3, $4,
                $5::timestamptz, $5::timestamptz + ($6::int * interval '1 second'),
                $6::int, $7::int, $8::numeric, $9::numeric, $10, $11::int, $12::int, $13::bool)`,
		c.CallID, c.AccountID, c.Dest, c.RatePrefix,
		c.StartTime, c.Duration, c.BilledSecs, c.Price, c.Cost, c.Release, c.PDDMs, c.SIPCode, c.Recorded)
	if err != nil {
		return fmt.Errorf("insert cdr: %w", err)
	}
	return nil
}

// AppendLedger writes a balance movement row (append-only).
func (p *PG) AppendLedger(ctx context.Context, accountID string, delta, balance float64, reason, callID string) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO balance_ledger (account_id, delta, balance, reason, call_id)
        VALUES ($1::uuid, $2, $3, $4, $5)`,
		accountID, delta, balance, reason, callID)
	return err
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
func (p *PG) CreateAccountWithHA1(ctx context.Context, tenantID, parentID, name, username, ha1, rateGroup string, postpaid bool, credit float64) (string, error) {
	var id string
	err := p.pool.QueryRow(ctx, `
        INSERT INTO accounts (tenant_id, parent_id, name, username, secret_hash,
                              ha1, rate_group, postpaid, credit)
        VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9)
        RETURNING id::text`,
		tenantID, nullIfEmpty(parentID), name, username, "x", ha1, rateGroup, postpaid, credit).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create account: %w", err)
	}
	return id, nil
}

// TopUp adjusts an account balance (delta signed) and returns the new
// balance; it also appends the ledger row so topup and adjustment stay
// in the append-only history.
func (p *PG) TopUp(ctx context.Context, accountID string, delta float64, reason, operator string) (float64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("topup begin: %w", err)
	}
	defer tx.Rollback(ctx) // safe no-op after commit

	var balance float64
	if err := tx.QueryRow(ctx, `
        UPDATE accounts SET credit = credit + $1
        WHERE id = $2::uuid AND deleted_at IS NULL
        RETURNING credit`, delta, accountID).Scan(&balance); err != nil {
		return 0, fmt.Errorf("topup update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO balance_ledger (account_id, delta, balance, reason, operator)
        VALUES ($1::uuid, $2, $3, $4, $5)`,
		accountID, delta, balance, reason, operator); err != nil {
		return 0, fmt.Errorf("topup ledger: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("topup commit: %w", err)
	}
	return balance, nil
}

// UpsertRate replaces the current rate for a prefix: superseded rows
// are marked ended_at (the loader reads only live rows), and a fresh
// row carries the new price - so a price change is a version, not a
// mutation (audit trail intact).
func (p *PG) UpsertRate(ctx context.Context, groupID, prefix string, price float64, initDur, stepDur, minDur int) error {
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
		groupID, prefix, price, initDur, stepDur, minDur); err != nil {
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

// QueryGatewayDaily reads live CDR aggregates per gateway (day '' = all).
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
