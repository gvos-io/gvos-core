// balance_redis.go: the Redis-side balance authority (redis-adoption
// Phase 1). Account *configuration* (the chain) stays in the Go
// memory map; money movements charge the leaf prepaid account only
// (Mode A: terminal billing — ancestors keep concurrent/CPS caps via
// the admission ledger, not co-debit of the same est). Amounts are
// INCRBY integers (ADR-0003 fixed point).
package account

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

func balKey(id string) string { return rds.Prefix + "bal:" + id }

// reserveScript: KEYS[1] = leaf balance key, ARGV[1] = est,
// ARGV[2] = maxDebt. Returns 1 on success, 0 on insufficient balance.
var reserveScript = rds.NewScript(`
local est = tonumber(ARGV[1])
local maxdebt = tonumber(ARGV[2])
local bal = tonumber(redis.call('GET', KEYS[1]) or '0')
if bal - maxdebt < est then
  return 0
end
redis.call('INCRBY', KEYS[1], -est)
return 1
`)

// refundScript: KEYS[1] = leaf key, ARGV[1] = signed delta (positive =
// refund remainder, negative = extra charge).
var refundScript = rds.NewScript(`
local delta = tonumber(ARGV[1])
redis.call('INCRBY', KEYS[1], delta)
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

// leafKeys returns the prepaid leaf account's balance key (Mode A:
// only the calling account is debited). Postpaid leaf → empty.
func (b *BalanceR) leafKeys(id string) ([]string, money.Amount, error) {
	b.s.mu.RLock()
	defer b.s.mu.RUnlock()
	a, ok := b.s.accounts[id]
	if !ok {
		return nil, 0, fmt.Errorf("account %q not found", id)
	}
	if a.Postpaid {
		return nil, 0, nil
	}
	return []string{balKey(id)}, a.MaxDebt, nil
}

// Reserve mirrors Store.Reserve on the authority. Fail-closed: a
// Redis error is returned and the caller must refuse the call.
func (b *BalanceR) Reserve(ctx context.Context, id string, est money.Amount) (string, error) {
	keys, debt, err := b.leafKeys(id)
	if err != nil {
		return "", err
	}
	if len(keys) == 0 { // postpaid leaf: nothing to lock
		return id + ":call:" + fmt.Sprintf("%d", now()), nil
	}
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	res, err := reserveScript.Run(ctx, b.rdb, keys, int64(est), int64(debt))
	if err != nil {
		return "", fmt.Errorf("balance authority unavailable: %w", err)
	}
	if v, _ := res.(int64); v != 1 {
		return "", fmt.Errorf("account %q insufficient balance (redis authority)", id)
	}
	return id + ":call:" + fmt.Sprintf("%d", now()), nil
}

// Commit reconciles est against actual on the leaf (delta
// refund/charge). Fail-open by design: settlement must not drop the
// CDR; the error is loud and the hour rollback to PG catches up.
func (b *BalanceR) Commit(ctx context.Context, token, id string, est, actual money.Amount) error {
	keys, _, err := b.leafKeys(id)
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

// SetAbsolute writes the authoritative balance (seed / peer-sync path).
// Prefer IncrBy for TopUp so concurrent call deductions are not wiped.
func (b *BalanceR) SetAbsolute(ctx context.Context, id string, amt money.Amount) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	return b.rdb.Raw().Set(ctx, balKey(id), int64(amt), 0).Err()
}

// IncrBy applies a signed delta on the Redis authority and returns the
// post-increment balance. TopUp must use this (not SetAbsolute) so a
// concurrent Reserve/Interim deduction is not clobbered by a stale PG
// absolute.
func (b *BalanceR) IncrBy(ctx context.Context, id string, delta money.Amount) (money.Amount, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	n, err := b.rdb.Raw().IncrBy(ctx, balKey(id), int64(delta)).Result()
	if err != nil {
		return 0, err
	}
	return money.Amount(n), nil
}

// SeedOne SETNXs a single account's credit (create-account path). Live
// values from other replicas are never overwritten.
func (b *BalanceR) SeedOne(ctx context.Context, id string, credit money.Amount) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	return b.rdb.Raw().SetNX(ctx, balKey(id), int64(credit), 0).Err()
}

// GetBalance is the BalanceAuthority face used by interim affordability.
func (b *BalanceR) GetBalance(ctx context.Context, id string) (money.Amount, error) {
	return b.Get(ctx, id)
}
