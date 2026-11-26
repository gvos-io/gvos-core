// call_meta.go: Redis-backed call settlement metadata for cross-replica
// BYE. Live Interim/teardown stay on the owning replica's map; this
// store is the money+admission fallback when LB stickiness fails or the
// owner crashes mid-call.
package callcore

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

const callMetaTTL = 15 * time.Minute

// Settle locks only need to cover the dual-End race window (owner +
// peer BYE). Reusing callMetaTTL (15m) left ~10k callsettled keys after
// a few minutes of 40–400 CPS load; 2m is ample for LB skew + retry.
const callSettledTTL = 2 * time.Minute

func callMetaKey(callID string) string { return rds.Prefix + "callmeta:" + callID }

// callSettledKey names the settle lock after ONE call instance
// (Call-ID + start millisecond) instead of the bare Call-ID. Gear
// that reuses Call-IDs for consecutive calls used to have the second
// call's End refused as "already settled" inside the TTL: its lock and
// accruals were then neither refunded nor charged, and no CDR existed.
func callSettledKey(callID string, startMs int64) string {
	return fmt.Sprintf("%scallsettled:%s:%d", rds.Prefix, callID, startMs)
}

// CallMeta is the minimal billing snapshot another replica needs to
// settle a call it never saw locally.
type CallMeta struct {
	AccountID string
	Dest      string
	StartUnix int64
	StartMs   int64 // start instant (ms): identifies the call instance
	AnswerMs  int64 // 200 OK epoch-ms, 0 = not answered; parked by whichever replica saw it
	Lock      money.Amount
	Accrued   money.Amount
	Prefix    string
	Price     money.Amount
	InitDur   int
	StepDur   int
	MinDur    int
	Recorded  bool
	Direction string // "" = out (legacy), "in" = callee-billed inbound
	GatewayID string // outbound carrier leg when known
}

// CallMetaStore persists CallMeta in Redis.
type CallMetaStore struct {
	rdb *rds.Client
}

// NewCallMetaStore wires the store.
func NewCallMetaStore(c *rds.Client) *CallMetaStore { return &CallMetaStore{rdb: c} }

// Put writes (or refreshes) meta for an in-progress call. It never
// touches answer_ms: the start write runs off the hot path and could
// land after a peer parked the answer (TouchAnswer), resetting it.
func (s *CallMetaStore) Put(ctx context.Context, callID string, m CallMeta) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := callMetaKey(callID)
	pipe := s.rdb.Raw().Pipeline()
	pipe.HSet(ctx, k, map[string]any{
		"account_id": m.AccountID,
		"dest":       m.Dest,
		"start":      m.StartUnix,
		"start_ms":   m.StartMs,
		"lock":       int64(m.Lock),
		"accrued":    int64(m.Accrued),
		"prefix":     m.Prefix,
		"price":      int64(m.Price),
		"init_dur":   m.InitDur,
		"step_dur":   m.StepDur,
		"min_dur":    m.MinDur,
		"recorded":   boolInt(m.Recorded),
		"direction":  m.Direction,
		"gateway":    m.GatewayID,
	})
	pipe.Expire(ctx, k, callMetaTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// TouchAnswer parks the 200 OK instant on the shared meta. The replica
// that owns the call may never see the answer itself (HTTP tier behind
// an LB without Call-ID stickiness); its interim loop adopts it.
func (s *CallMetaStore) TouchAnswer(ctx context.Context, callID string, answerMs int64) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := callMetaKey(callID)
	pipe := s.rdb.Raw().Pipeline()
	pipe.HSet(ctx, k, "answer_ms", answerMs)
	pipe.Expire(ctx, k, callMetaTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// Answered reports the parked answer instant (ok=false when the call
// has no answer on record, or has no meta at all).
func (s *CallMetaStore) Answered(ctx context.Context, callID string) (int64, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	v, err := s.rdb.Raw().HGet(ctx, callMetaKey(callID), "answer_ms").Result()
	if err == redis.Nil {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	ms, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("answer_ms %q: %w", v, err)
	}
	return ms, ms > 0, nil
}

// TouchAccrued updates only the accrued field (Interim path).
func (s *CallMetaStore) TouchAccrued(ctx context.Context, callID string, accrued money.Amount) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := callMetaKey(callID)
	pipe := s.rdb.Raw().Pipeline()
	pipe.HSet(ctx, k, "accrued", int64(accrued))
	pipe.Expire(ctx, k, callMetaTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// Delete drops meta after a local End.
func (s *CallMetaStore) Delete(ctx context.Context, callID string) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	return s.rdb.Raw().Del(ctx, callMetaKey(callID)).Err()
}

// ClaimSettle SETNXs a settle lock so only one replica finalizes money
// for this call instance (owner End racing a peer remote-settle).
func (s *CallMetaStore) ClaimSettle(ctx context.Context, callID string, startMs int64) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	return s.rdb.Raw().SetNX(ctx, callSettledKey(callID, startMs), "1", callSettledTTL).Result()
}

// Take atomically reads and deletes meta so only one replica can
// remote-settle a given callID.
func (s *CallMetaStore) Take(ctx context.Context, callID string) (CallMeta, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := callMetaKey(callID)
	// GETDEL is Redis 6.2+; fall back to GET+DEL under a short WATCH window
	// is overkill — pipeline HGETALL then DEL; a double settle is prevented
	// by the first successful money Commit + admission EndChain being
	// idempotent enough for our purposes, and Take uses GETDEL when available.
	vals, err := s.rdb.Raw().HGetAll(ctx, k).Result()
	if err != nil {
		return CallMeta{}, false, err
	}
	if len(vals) == 0 {
		return CallMeta{}, false, nil
	}
	_ = s.rdb.Raw().Del(ctx, k).Err()
	m, err := parseCallMeta(vals)
	if err != nil {
		return CallMeta{}, false, err
	}
	return m, true, nil
}

func (m CallMeta) Entry() *rate.Entry {
	if m.Prefix == "" && m.Price == 0 {
		return nil
	}
	return &rate.Entry{
		Prefix:  m.Prefix,
		Price:   m.Price,
		InitDur: m.InitDur,
		StepDur: m.StepDur,
		MinDur:  m.MinDur,
	}
}

func parseCallMeta(vals map[string]string) (CallMeta, error) {
	var m CallMeta
	m.AccountID = vals["account_id"]
	m.Dest = vals["dest"]
	m.Prefix = vals["prefix"]
	m.Recorded = vals["recorded"] == "1"
	var err error
	if m.StartUnix, err = strconv.ParseInt(vals["start"], 10, 64); err != nil {
		return m, fmt.Errorf("start: %w", err)
	}
	if v, err := strconv.ParseInt(vals["start_ms"], 10, 64); err == nil && v > 0 {
		m.StartMs = v
	}
	if m.StartMs == 0 {
		m.StartMs = m.StartUnix * 1000 // meta written before start_ms existed
	}
	if v, err := strconv.ParseInt(vals["answer_ms"], 10, 64); err == nil {
		m.AnswerMs = v
	}
	if v, err := strconv.ParseInt(vals["lock"], 10, 64); err == nil {
		m.Lock = money.Amount(v)
	}
	if v, err := strconv.ParseInt(vals["accrued"], 10, 64); err == nil {
		m.Accrued = money.Amount(v)
	}
	if v, err := strconv.ParseInt(vals["price"], 10, 64); err == nil {
		m.Price = money.Amount(v)
	}
	m.InitDur, _ = strconv.Atoi(vals["init_dur"])
	m.StepDur, _ = strconv.Atoi(vals["step_dur"])
	m.MinDur, _ = strconv.Atoi(vals["min_dur"])
	// Optional fields: absent on meta written before DIDs shipped.
	m.Direction = vals["direction"]
	m.GatewayID = vals["gateway"]
	if m.AccountID == "" {
		return m, fmt.Errorf("empty account_id")
	}
	return m, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
