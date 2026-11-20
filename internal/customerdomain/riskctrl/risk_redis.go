// risk_redis.go: the cross-replica risk windows (redis adoption
// Phase 3). Three state families move to shared ZSETs/counters so
// every replica evaluates the same aggregates:
//
//	rfreq   gvos:rfreq:{account}   ZSET member={sec}.{nano} 60s window
//	gfreq   gvos:gfreq:{user}      ZSET same shape (reg storms)
//	costday gvos:costday:{day}:{account} string INCRBY 48h TTL
//
// Failure semantics are fail-open per the adoption plan: the fraud
// engine going down must not take the platform down — errors log and
// the local (per-replica) fallback evaluates. localMap stays the
// single-replica truth; this face is the N>1 truth.
package riskctrl

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

const costDayTTL = 48 * time.Hour

// RiskR is the Redis aggregate face for the engine.
type RiskR struct {
	rdb *rds.Client
}

// NewRiskR wires the face.
func NewRiskR(c *rds.Client) *RiskR { return &RiskR{rdb: c} }

func rfreqKey(a string) string { return rds.Prefix + "rfreq:" + a }
func gfreqKey(u string) string { return rds.Prefix + "gfreq:" + u }
func costDayKey(day int64, a string) string {
	return fmt.Sprintf("%scostday:%d:%s", rds.Prefix, day, a)
}

// SlidingAdd adds "now" to the window ZSET, prunes below the cutoff
// and returns the live count — one Lua round trip (ZADD +
// ZREMRANGEBYSCORE + ZCARD must be atomic under concurrent replicas).
var slidingAddScript = rds.NewScript(`
local cutoff = tonumber(ARGV[1])
redis.call('ZADD', KEYS[1], ARGV[2], ARGV[3])
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', cutoff)
redis.call('EXPIRE', KEYS[1], 90)
return redis.call('ZCARD', KEYS[1])
`)

// SlidingCount returns the live count after adding the current
// attempt (member = sec.nano so multi-replica duplicates stay
// distinct; the count includes this attempt).
func (r *RiskR) SlidingCount(ctx context.Context, key string, now time.Time) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	sec, nano := now.Unix(), now.Nanosecond()
	res, err := slidingAddScript.Run(ctx, r.rdb, []string{key},
		fmt.Sprintf("%d", sec-60), fmt.Sprintf("%d", sec), fmt.Sprintf("%d.%09d", sec, nano))
	if err != nil {
		return 0, err
	}
	n, _ := res.(int64)
	return int(n), nil
}

// CostDayAdd advances the day accumulator and returns the new total.
func (r *RiskR) CostDayAdd(ctx context.Context, day int64, account string, amt money.Amount) (money.Amount, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := costDayKey(day, account)
	pipe := r.rdb.Raw().Pipeline()
	pipe.IncrBy(ctx, k, int64(amt))
	pipe.Expire(ctx, k, costDayTTL)
	res, err := pipe.Exec(ctx)
	if err != nil {
		return 0, err
	}
	return money.Amount(res[0].(*redis.IntCmd).Val()), nil
}

// CostDaySub reconciles down at settlement (actual < est).
func (r *RiskR) CostDaySub(ctx context.Context, day int64, account string, amt money.Amount) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := costDayKey(day, account)
	pipe := r.rdb.Raw().Pipeline()
	pipe.IncrBy(ctx, k, -int64(amt))
	pipe.Expire(ctx, k, costDayTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// CostDayGet reads the accumulator (rule evaluation without adding).
func (r *RiskR) CostDayGet(ctx context.Context, day int64, account string) (money.Amount, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	v, err := r.rdb.Raw().Get(ctx, costDayKey(day, account)).Result()
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

// warn is the fail-open logger.
func warn(op string, err error) {
	log.Printf("risk redis %s failed (fail-open to local): %v", op, err)
}

// ctx0 is a small helper keeping the engine's method signatures
// unchanged (the hot path has no request context to carry).
func ctx0() context.Context { return context.Background() }
