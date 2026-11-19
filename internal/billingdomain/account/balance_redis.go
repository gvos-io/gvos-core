// balance_redis.go: the Redis-side balance authority (redis-adoption
// Phase 1). Account *configuration* (the chain) stays in the Go
// memory map; only Credit/MaxDebt movements cross to Redis, executed
// atomically by Lua so a multi-account reservation is all-or-nothing
// even under concurrent replicas. Amounts are INCRBY integers
// (ADR-0003 fixed point) — no floating point ever crosses the wire.
package account

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/lixuanqun/gvos-core/internal/billingdomain/money"
	"github.com/lixuanqun/gvos-core/internal/shared/rds"
)

func balKey(id string) string { return rds.Prefix + "bal:" + id }

// reserveScript: KEYS = balance keys of the prepaid chain (in order),
// ARGV[1] = est (fixed-point int), ARGV[2..] = maxDebt per key in the
// same order. Checks then debits in one pass; rolls back on the first
// failure. Returns 1 on success, 0 on insufficient balance.
var reserveScript = rds.NewScript(`
local est = tonumber(ARGV[1])
local charged = {}
for i, k in ipairs(KEYS) do
  local maxdebt = tonumber(ARGV[i + 1])
  local bal = tonumber(redis.call('GET', k) or '0')
  if bal - maxdebt < est then
    for j = 1, i - 1 do
      redis.call('INCRBY', KEYS[j], est)
    end
    return 0
  end
  redis.call('INCRBY', k, -est)
  charged[i] = true
end
return 1
`)

// refundScript: KEYS = chain keys, ARGV[1] = signed delta (positive =
// refund remainder, negative = extra charge). Applied to every key.
var refundScript = rds.NewScript(`
local delta = tonumber(ARGV[1])
for _, k in ipairs(KEYS) do
  redis.call('INCRBY', k, delta)
end
return 1
`)

// BalanceR is the Redis authority face of Store: when non-nil it owns
// the money; the in-memory map stays a configuration cache.
type BalanceR struct {
	s   *Store
	rdb *rds.Client
}

// NewBalanceR wires the authority onto a store.
func NewBalanceR(s *Store, c *rds.Client) *BalanceR { return &BalanceR{s: s, rdb: c} }

// Seed writes each account's balance with SETNX semantics: a live
// value from another replica is never clobbered (restarts keep the
// movements other replicas made). Called once at boot after Load.
func (b *BalanceR) Seed(ctx context.Context) error {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	for id, a := range b.s.accounts {
		if err := b.rdb.Raw().SetNX(ctx, balKey(id), int64(a.Credit), 0).Err(); err != nil {
			return fmt.Errorf("seed %s: %w", id, err)
		}
	}
	return nil
}

// chainIDs returns the prepaid chain (Go walks the config tree; Redis
// executes the money).
func (b *BalanceR) chainIDs(id string) ([]string, []money.Amount, error) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	chain, err := b.s.chainLocked(id)
	if err != nil {
		return nil, nil, err
	}
	var keys []string
	var debts []money.Amount
	for _, a := range chain {
		if a.Postpaid {
			continue
		}
		keys = append(keys, balKey(a.ID))
		debts = append(debts, a.MaxDebt)
	}
	return keys, debts, nil
}

// Reserve mirrors Store.Reserve on the authority. Fail-closed: a
// Redis error is returned and the caller must refuse the call.
func (b *BalanceR) Reserve(ctx context.Context, id string, est money.Amount) (string, error) {
	keys, debts, err := b.chainIDs(id)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 { // fully postpaid chain: nothing to lock
		return id + ":call:" + fmt.Sprintf("%d", now()), nil
	}
	args := make([]any, 0, len(debts)+1)
	args = append(args, int64(est))
	for _, d := range debts {
		args = append(args, int64(d))
	}
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	res, err := reserveScript.Run(ctx, b.rdb, keys, args...)
	if err != nil {
		return "", fmt.Errorf("balance authority unavailable: %w", err)
	}
	if v, _ := res.(int64); v != 1 {
		return "", fmt.Errorf("account %q insufficient balance (redis authority)", id)
	}
	return id + ":call:" + fmt.Sprintf("%d", now()), nil
}

// Commit reconciles est against actual across the chain (delta
// refund/charge). Fail-open by design: settlement must not drop the
// CDR; the error is loud and the hour rollback to PG catches up.
func (b *BalanceR) Commit(ctx context.Context, token, id string, est, actual money.Amount) error {
	keys, _, err := b.chainIDs(id)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	_, err = refundScript.Run(ctx, b.rdb, keys, int64(est.Sub(actual)))
	return err
}

// Release cancels a reservation fully (call never connected).
func (b *BalanceR) Release(ctx context.Context, token, id string, est money.Amount) error {
	return b.Commit(ctx, token, id, est, 0)
}

// Get reads the authoritative balance (falls back to memory when the
// key is missing — pre-seed window).
func (b *BalanceR) Get(ctx context.Context, id string) (money.Amount, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	v, err := b.rdb.Raw().Get(ctx, balKey(id)).Result()
	if err == redis.Nil {
		b.s.mu.Lock()
		defer b.s.mu.Unlock()
		a, ok := b.s.accounts[id]
		if !ok {
			return 0, fmt.Errorf("account %q not found", id)
		}
		return a.Credit, nil
	}
	if err != nil {
		return 0, err
	}
	var amt money.Amount
	if _, err := fmt.Sscanf(v, "%d", &amt); err != nil {
		return 0, fmt.Errorf("balance %s: %w", id, err)
	}
	return amt, nil
}

// ReserveCtx is the BalanceAuthority face used by callcore via the
// api-layer adapter.
func (b *BalanceR) ReserveCtx(ctx context.Context, id string, est money.Amount) (string, error) {
	return b.Reserve(ctx, id, est)
}

// CommitCtx is the BalanceAuthority settle face.
func (b *BalanceR) CommitCtx(ctx context.Context, token, id string, est, actual money.Amount) error {
	return b.Commit(ctx, token, id, est, actual)
}

// SetAbsolute writes the authoritative balance (TopUp /
// broadcast-sync path).
func (b *BalanceR) SetAbsolute(ctx context.Context, id string, amt money.Amount) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	return b.rdb.Raw().Set(ctx, balKey(id), int64(amt), 0).Err()
}
