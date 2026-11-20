// reg_redis.go: the persistent registration mirror (redis adoption
// Phase 5 + ADR-0002). Each Apply dual-writes a per-user hash with a
// TTL of the granted expiry — restarts no longer lose the table, and
// a signaling node that lost its local usrloc can ask gv-core for the
// contact state (fallback lookup: local first, redis mirror second).
package regtable

import (
	"context"
	"log"
	"strconv"
	"time"


	"github.com/gvos-io/gvos-core/internal/shared/rds"
)

// RegR is the redis mirror face.
type RegR struct {
	rdb *rds.Client
}

// NewRegR wires the face.
func NewRegR(c *rds.Client) *RegR { return &RegR{rdb: c} }

func regKey(user string) string { return rds.Prefix + "reg:" + user }

// Apply mirrors one event (idempotent HSET per user; the TTL tracks
// the granted expiry so dead registrations evaporate on their own).
func (r *RegR) Apply(ctx context.Context, user, op string, expires int64, srcIP string, at int64) {
	if user == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	k := regKey(user)
	pipe := r.rdb.Raw().Pipeline()
	pipe.HSet(ctx, k, "last_reg", at, "online", op == "reg")
	if op == "reg" {
		pipe.HSet(ctx, k, "expires", expires, "src_ip", srcIP)
		if ttl := expires - at; ttl > 0 {
			pipe.Expire(ctx, k, time.Duration(ttl)*time.Second)
		}
	}
	if _, err := pipe.Exec(ctx); err != nil {
		log.Printf("reg redis mirror failed (fail-open, memory only): %v", err)
	}
}

// Lookup reads the mirrored entry for the fallback path (ADR-0002:
// the signaling edge asks here after a local usrloc miss).
func (r *RegR) Lookup(ctx context.Context, user string) (Entry, bool) {
	ctx, cancel := context.WithTimeout(ctx, rds.CallTimeout)
	defer cancel()
	m, err := r.rdb.Raw().HGetAll(ctx, regKey(user)).Result()
	if err != nil || len(m) == 0 {
		return Entry{}, false
	}
	e := Entry{User: user}
	if v, ok := m["last_reg"]; ok {
		e.LastReg, _ = strconv.ParseInt(v, 10, 64)
	}
	if v, ok := m["expires"]; ok {
		e.Expires, _ = strconv.ParseInt(v, 10, 64)
	}
	e.SrcIP = m["src_ip"]
	e.Online = m["online"] == "1"
	return e, true
}
