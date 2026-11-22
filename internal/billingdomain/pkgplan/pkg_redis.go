// pkg_redis.go: shared package-usage counters (redis adoption Phase
// 4). Free minutes and cycle spend become cross-replica aggregates;
// cycle rollover falls out of the cycleKey changing (old keys
// evaporate via TTL). Local counters stay the fail-open fallback and
// single-replica truth.
package pkgplan

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

const cycleTTL = 40 * 24 * time.Hour // cycle end + buffer

// PkgR is the redis usage face.
type PkgR struct {
	rdb *rds.Client
}

// NewPkgR wires the face.
func NewPkgR(c *rds.Client) *PkgR { return &PkgR{rdb: c} }

func usedKey(account, cycleKey string) string {
	return rds.Prefix + "pkg:used:" + account + ":" + cycleKey
}
func spendKey(account, cycleKey string) string {
	return rds.Prefix + "pkg:spend:" + account + ":" + cycleKey
}

// AddUsed advances free-seconds usage and returns the new total.
func (p *PkgR) AddUsed(ctx context.Context, account, cycleKey string, secs float64) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := usedKey(account, cycleKey)
	pipe := p.rdb.Raw().Pipeline()
	pipe.IncrByFloat(ctx, k, secs)
	pipe.Expire(ctx, k, cycleTTL)
	res, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return res[0].(*redis.FloatCmd).Val(), nil
}

// claimUsedScript atomically claims up to `want` free seconds under a
// cycle cap — closes GetUsed/AddUsed TOCTOU across replicas.
// ARGV: wantSecs, maxFreeSecs, ttlSeconds. Returns seconds actually claimed.
var claimUsedScript = rds.NewScript(`
local want = tonumber(ARGV[1])
local maxf = tonumber(ARGV[2])
local ttl = tonumber(ARGV[3])
local used = tonumber(redis.call('GET', KEYS[1]) or '0')
local left = maxf - used
if left < 0 then left = 0 end
local take = want
if take > left then take = left end
if take > 0 then
  redis.call('INCRBYFLOAT', KEYS[1], take)
  redis.call('EXPIRE', KEYS[1], ttl)
end
return tostring(take)
`)

// ClaimUsed atomically reserves free seconds under the cycle allowance.
func (p *PkgR) ClaimUsed(ctx context.Context, account, cycleKey string, want, maxFree float64) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	res, err := claimUsedScript.Run(ctx, p.rdb, []string{usedKey(account, cycleKey)},
		want, maxFree, int64(cycleTTL/time.Second))
	if err != nil {
		return 0, err
	}
	switch v := res.(type) {
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err
	case []byte:
		f, err := strconv.ParseFloat(string(v), 64)
		return f, err
	default:
		f, err := strconv.ParseFloat(fmt.Sprint(v), 64)
		return f, err
	}
}

// AddSpend advances cycle spend (fixed-point integer INCRBY).
func (p *PkgR) AddSpend(ctx context.Context, account, cycleKey string, amt money.Amount) (money.Amount, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := spendKey(account, cycleKey)
	pipe := p.rdb.Raw().Pipeline()
	pipe.IncrBy(ctx, k, int64(amt))
	pipe.Expire(ctx, k, cycleTTL)
	res, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return money.Amount(res[0].(*redis.IntCmd).Val()), nil
}

// GetSpend reads the cycle spend (shortfall evaluation).
func (p *PkgR) GetSpend(ctx context.Context, account, cycleKey string) (money.Amount, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	v, err := p.rdb.Raw().Get(ctx, spendKey(account, cycleKey)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, err
	}
	return money.Amount(n), nil
}

// fail-open logger.
func warn(op string, err error) {
	log.Printf("pkg redis %s failed (fail-open to local): %v", op, err)
}

// GetUsed reads the free-seconds usage (headroom evaluation).
func (p *PkgR) GetUsed(ctx context.Context, account, cycleKey string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	v, err := p.rdb.Raw().Get(ctx, usedKey(account, cycleKey)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, err
	}
	return f, nil
}
