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
        SELECT id::text, coalesce(parent_id::text, ''), name,
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
			id, parent, name, group string
			postpaid                 bool
			credit, maxDebt          float64
			maxConc, maxCPS          int
		)
		if err := rows.Scan(&id, &parent, &name, &group, &postpaid, &credit, &maxDebt, &maxConc, &maxCPS); err != nil {
			return n, fmt.Errorf("scan account: %w", err)
		}
		if err := dst.Put(&account.Account{
			ID: id, ParentID: parent, Name: name, RateGroup: group,
			Postpaid: postpaid, Credit: credit, MaxDebt: maxDebt,
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
                         price, cost, release)
        VALUES ($1, '00000000-0000-0000-0000-000000000001', $2::uuid, $3, $4,
                $5::timestamptz, $5::timestamptz + ($6::int * interval '1 second'),
                $6::int, $7::int, $8::numeric, $9::numeric, $10)`,
		c.CallID, c.AccountID, c.Dest, c.RatePrefix,
		c.StartTime, c.Duration, c.BilledSecs, c.Price, c.Cost, c.Release)
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
