// admission_redis.go: the cross-replica admission ledger (redis
// adoption Phase 2). Concurrent = per-account ZSET (member=callID,
// score=start unix); the whole-chain check pipelines ZCARD over the
// chain keys. CPS = per-second counter with a 1s TTL. The local calls
// map stays the runtime-detail owner (interim driver, teardown); this
// ledger is the shared truth replicas admit against.
package callcore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

const (
	// ZombiTTL bounds how long a crashed replica's calls linger in the
	// ledger before any replica's sweeper settles them.
	ZombieTTL = 15 * time.Minute
	// ZombieTTLSeconds is the Lua-side spelling of ZombiTTL (the script
	// body is concatenated, not parameterised — EXPIRE takes the number
	// verbatim).
	ZombieTTLSeconds = int(ZombieTTL / time.Second)
)

// AdmissionR is the Redis admission face.
type AdmissionR struct {
	rdb *rds.Client
}

// NewAdmissionR wires the ledger.
func NewAdmissionR(c *rds.Client) *AdmissionR { return &AdmissionR{rdb: c} }

func callsKey(id string) string { return rds.Prefix + "calls:" + id }

func cpsKey(id string, sec int64) string {
	return fmt.Sprintf("%scps:%s:%d", rds.Prefix, id, sec)
}

// Start registers a call on the caller's own ledger key. The chain
// variant (StartChain) mirrors the member onto every ancestor key so
// their sub-tree caps can ZCARD their own key — one pipelined round
// trip, member shared.
func (a *AdmissionR) Start(ctx context.Context, callID, accountID string, start time.Time) error {
	return a.StartChain(ctx, callID, []string{accountID}, start)
}

// StartChain registers the call under every chain account (own +
// ancestors): ancestors' ZCARD then counts their sub-tree directly.
func (a *AdmissionR) StartChain(ctx context.Context, callID string, chain []string, start time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	pipe := a.rdb.Raw().Pipeline()
	for _, id := range chain {
		k := callsKey(id)
		pipe.ZAdd(ctx, k, redis.Z{Score: float64(start.Unix()), Member: callID})
		pipe.Expire(ctx, k, ZombieTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// End removes the call from every chain key it was mirrored onto.
func (a *AdmissionR) End(ctx context.Context, callID, accountID string) error {
	return a.EndChain(ctx, callID, []string{accountID})
}

// EndChain mirrors removal (same chain the call was started with).
func (a *AdmissionR) EndChain(ctx context.Context, callID string, chain []string) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	pipe := a.rdb.Raw().Pipeline()
	for _, id := range chain {
		pipe.ZRem(ctx, callsKey(id), callID)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// DecrCPS rolls back the per-second counter after a successful AdmitCall
// whose subsequent reserve failed — without this the CPS slot is a
// ghost occupancy until the 1s TTL expires.
func (a *AdmissionR) DecrCPS(ctx context.Context, accountID string, start time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := cpsKey(accountID, start.Unix())
	n, err := a.rdb.Raw().Decr(ctx, k).Result()
	if err != nil {
		return err
	}
	if n < 0 {
		_ = a.rdb.Raw().Del(ctx, k).Err()
	}
	return nil
}

// LiveConcurrent counts the account's live calls (ZCARD).
func (a *AdmissionR) LiveConcurrent(ctx context.Context, accountID string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	n, err := a.rdb.Raw().ZCard(ctx, callsKey(accountID)).Result()
	return int(n), err
}

// AdmitCall is the atomic single-round-trip admission+registration.
// One Lua script over the whole chain key set: the concurrent check
// (ZCARD+1 <= cap on every capped key), the CPS INCR check, then the
// ZADD registration onto every chain key — the check and the write
// execute back-to-back inside Redis, closing the TOCTOU oversell that
// the two-step AdmitConcurrent→StartChain pair leaves between replicas.
//
// KEYS   = chain calls keys, in chain order (own first)
// ARGV   = [callID, startUnix, unused pad, cpsKey, cpsCap,
//          then per-KEYS: cap (0 = uncapped)]
// Return = 0 admitted; a KEYS index i (>0) when KEYS[i] is over its
// cap; -1 when the CPS cap rejected.
var admitScript = rds.NewScript(`
local cid = ARGV[1]
local score = tonumber(ARGV[2])
local cps = ARGV[4]
local cpsCap = tonumber(ARGV[5])
-- Drop ledger members older than the zombie TTL before ZCARD so a
-- busy account's key-level EXPIRE refresh cannot keep crashed-replica
-- callIDs forever.
local cutoff = score - ` + fmt.Sprint(ZombieTTLSeconds) + `
for i = 1, #KEYS do
  redis.call('ZREMRANGEBYSCORE', KEYS[i], '-inf', cutoff)
end
for i = 1, #KEYS do
  local cap = tonumber(ARGV[5 + i])
  if cap > 0 then
    local live = redis.call('ZCARD', KEYS[i])
    if live + 1 > cap then
      return i
    end
  end
end
if cpsCap > 0 then
  local n = redis.call('INCR', cps)
  if n == 1 then redis.call('EXPIRE', cps, 1) end
  if n > cpsCap then
    return -1
  end
end
for i = 1, #KEYS do
  redis.call('ZADD', KEYS[i], score, cid)
  redis.call('EXPIRE', KEYS[i], ` + fmt.Sprint(ZombieTTLSeconds) + `)
end
return 0
`)

// AdmitCall runs admitScript. chain = account IDs (own first, then
// ancestors), caps maps account ID -> concurrent cap (0/absent =
// uncapped). Errors: *ErrLimit carries which chain key or the CPS
// window rejected. Redis failure is admission failure — the merged
// path is money-adjacent, so "could not check" must refuse the call
// (the old two-step path logged and continued on StartChain errors,
// which under-counted a shared ledger).
func (a *AdmissionR) AdmitCall(ctx context.Context, chain []string, caps map[string]int, callID string, start time.Time, cpsCap int) error {
	if len(chain) == 0 {
		return errors.New("admit: empty chain")
	}
	keys := make([]string, 0, len(chain))
	args := make([]any, 0, len(chain)+6)
	args = append(args, callID, start.Unix())
	// Pad ARGV[3] (unused by the script's current form, kept so the
	// KEYS/ARGV contract above stays stable).
	args = append(args, 0)
	args = append(args, cpsKey(chain[0], start.Unix()), cpsCap)
	for _, id := range chain {
		keys = append(keys, callsKey(id))
		args = append(args, caps[id])
	}
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	res, err := admitScript.Run(ctx, a.rdb, keys, args...)
	if err != nil {
		return fmt.Errorf("admission authority unavailable: %w", err)
	}
	v, _ := res.(int64)
	switch {
	case v == 0:
		return nil
	case v == -1:
		return &ErrLimit{AccountID: chain[0], Kind: "cps",
			Detail: "starts this second (ledger) over cap"}
	default:
		i := int(v) - 1 // KEYS index -> chain index
		if i < 0 || i >= len(chain) {
			return fmt.Errorf("admit: bad script result %d", v)
		}
		if i == 0 {
			return &ErrLimit{AccountID: chain[i], Kind: "concurrent",
				Detail: "live (ledger) over cap"}
		}
		return &ErrLimit{AccountID: chain[i], Kind: "concurrent",
			Detail: "sub-tree live (ledger) over cap"}
	}
}

// AdmitConcurrent checks "own live < cap" for the exact account and
// "sub-tree live < cap" for every capped ancestor, one pipelined
// round trip: the Go side walks the config tree for the chain, Redis
// counts.
func (a *AdmissionR) AdmitConcurrent(ctx context.Context, chain []string, own string, caps map[string]int) error {
	keys := make([]string, 0, len(chain))
	for _, id := range chain {
		keys = append(keys, callsKey(id))
	}
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	pipe := a.rdb.Raw().Pipeline()
	for _, k := range keys {
		pipe.ZCard(ctx, k)
	}
	res, err := pipe.Exec(ctx)
	if err != nil {
		return err
	}
	for i, id := range chain {
		cap := caps[id]
		if cap <= 0 {
			continue // uncapped
		}
		live := int(res[i].(*redis.IntCmd).Val())
		// The starting call is not in the ledger yet: +1 the check.
		if live+1 > cap {
			if id == own {
				return &ErrLimit{AccountID: id, Kind: "concurrent",
					Detail: fmt.Sprintf("%d live (ledger) >= cap %d", live, cap)}
			}
			return &ErrLimit{AccountID: id, Kind: "concurrent",
				Detail: fmt.Sprintf("sub-tree live %d (ledger) >= cap %d", live, cap)}
		}
	}
	return nil
}

// AdmitCPS increments-and-checks the per-second counter (INCR then
// compare; EXPIRE 1s on the window). First increment of the second
// sets the TTL.
func (a *AdmissionR) AdmitCPS(ctx context.Context, accountID string, sec int64, cap int) error {
	if cap <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := cpsKey(accountID, sec)
	pipe := a.rdb.Raw().Pipeline()
	pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, time.Second)
	res, err := pipe.Exec(ctx)
	if err != nil {
		return err
	}
	if n := int(res[0].(*redis.IntCmd).Val()); n > cap {
		return &ErrLimit{AccountID: accountID, Kind: "cps",
			Detail: fmt.Sprintf("%d starts this second (ledger) >= cap %d", n, cap)}
	}
	return nil
}

// Zombies lists ledger members older than the TTL (the sweeper's
// cross-replica settle source).
func (a *AdmissionR) Zombies(ctx context.Context, accountID string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	cutoff := fmt.Sprintf("%d", time.Now().Add(-ZombieTTL).Unix())
	return a.rdb.Raw().ZRangeByScore(ctx, callsKey(accountID), &redis.ZRangeBy{
		Max: cutoff, Min: "-inf", Count: 100,
	}).Result()
}

// Remove drops a settled zombie from the ledger.
func (a *AdmissionR) Remove(ctx context.Context, accountID, callID string) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	return a.rdb.Raw().ZRem(ctx, callsKey(accountID), callID).Err()
}
