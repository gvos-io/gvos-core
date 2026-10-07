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

// UpsertRate inserts or updates one rate row (prefix unique within the
// group for the base variant).
func (p *PG) UpsertRate(ctx context.Context, groupID, prefix string, price float64, initDur, stepDur, minDur int) error {
	_, err := p.pool.Exec(ctx, `
        INSERT INTO rates (group_id, prefix, price, init_dur, step_dur, min_dur)
        VALUES ($1::uuid, $2, $3, $4, $5, $6)
        ON CONFLICT (group_id, prefix, priority, effective_at)
        DO UPDATE SET price = EXCLUDED.price, init_dur = EXCLUDED.init_dur,
                      step_dur = EXCLUDED.step_dur, min_dur = EXCLUDED.min_dur`,
		groupID, prefix, price, initDur, stepDur, minDur)
	if err != nil {
		return fmt.Errorf("upsert rate: %w", err)
	}
	return nil
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
