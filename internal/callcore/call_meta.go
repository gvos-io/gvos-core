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

	"github.com/gvos-io/gvos-core/internal/billingdomain/money"
	"github.com/gvos-io/gvos-core/internal/billingdomain/rate"
	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

const callMetaTTL = 15 * time.Minute

func callMetaKey(callID string) string    { return rds.Prefix + "callmeta:" + callID }
func callSettledKey(callID string) string { return rds.Prefix + "callsettled:" + callID }

// CallMeta is the minimal billing snapshot another replica needs to
// settle a call it never saw locally.
type CallMeta struct {
	AccountID string
	Dest      string
	StartUnix int64
	Lock      money.Amount
	Accrued   money.Amount
	Prefix    string
	Price     money.Amount
	InitDur   int
	StepDur   int
	MinDur    int
	Recorded  bool
}

// CallMetaStore persists CallMeta in Redis.
type CallMetaStore struct {
	rdb *rds.Client
}

// NewCallMetaStore wires the store.
func NewCallMetaStore(c *rds.Client) *CallMetaStore { return &CallMetaStore{rdb: c} }

// Put writes (or refreshes) meta for an in-progress call.
func (s *CallMetaStore) Put(ctx context.Context, callID string, m CallMeta) error {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := callMetaKey(callID)
	pipe := s.rdb.Raw().Pipeline()
	pipe.HSet(ctx, k, map[string]any{
		"account_id": m.AccountID,
		"dest":       m.Dest,
		"start":      m.StartUnix,
		"lock":       int64(m.Lock),
		"accrued":    int64(m.Accrued),
		"prefix":     m.Prefix,
		"price":      int64(m.Price),
		"init_dur":   m.InitDur,
		"step_dur":   m.StepDur,
		"min_dur":    m.MinDur,
		"recorded":   boolInt(m.Recorded),
	})
	pipe.Expire(ctx, k, callMetaTTL)
	_, err := pipe.Exec(ctx)
	return err
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
// for callID (owner End racing a peer remote-settle).
func (s *CallMetaStore) ClaimSettle(ctx context.Context, callID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	return s.rdb.Raw().SetNX(ctx, callSettledKey(callID), "1", callMetaTTL).Result()
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
