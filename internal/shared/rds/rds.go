// Package rds is the Redis client layer: connection lifecycle, key
// prefix constants, script helpers (EvalSha with Eval fallback) and
// the single-runner lock. It follows internal/shared/events.go's
// house style: a small New/Close pair, explicit constants, and no
// exported surface beyond what the callers need.
//
// Redis is the *runtime-state authority* for balances, admission and
// risk windows (redis-adoption-plan). nil Client = Redis off: the
// in-memory per-replica implementations remain authoritative, which
// is correct only for single-replica deployments.
package rds

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Key prefix for every G-VOS key (see redis-adoption-plan §4 for the
// full layout table).
const (
	Prefix = "gvos:"

	// CallTimeout is the ceiling for any single Redis round trip on
	// the hot path; beyond it the caller applies its fail-open /
	// fail-closed policy.
	CallTimeout = 500 * time.Millisecond

	// LockTTL is the default single-runner lock TTL.
	LockTTL = 50 * time.Second
)

// Client wraps go-redis with connection lifecycle.
type Client struct {
	rdb *redis.Client
}

// New parses the URL, pings once, and returns a ready client. A
// failed ping is a hard error: the redis-adoption plan makes a
// configured-but-unreachable Redis fatal at startup (it is the money
// authority; silent degradation is how balances oversell).
func New(url string) (*Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opt)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, err
	}
	return &Client{rdb: rdb}, nil
}

// Close is idempotent.
func (c *Client) Close() error {
	if c == nil || c.rdb == nil {
		return nil
	}
	rdb := c.rdb
	c.rdb = nil // poison the handle; double close is a no-op
	return rdb.Close()
}

// Raw exposes the underlying client for domain packages that need
// commands this wrapper does not cover. Callers must apply
// CallTimeout themselves.
func (c *Client) Raw() *redis.Client { return c.rdb }

// script is a Lua script cached by SHA (EvalSha, Eval fallback).
type script struct {
	src  string
	sha1 string
}

func newScript(src string) *script {
	sum := sha1.Sum([]byte(src))
	return &script{src: src, sha1: hex.EncodeToString(sum[:])}
}

// Run evaluates the script with keys and args.
func (s *script) Run(ctx context.Context, c *Client, keys []string, args ...any) (any, error) {
	res := c.rdb.EvalSha(ctx, s.sha1, keys, args...)
	if err := res.Err(); err != nil {
		// NOSCRIPT: first use after restart/flush — fall back to EVAL
		// and the server re-caches the SHA. Prefix match, not exact:
		// server builds differ slightly in the message tail.
		if redis.HasErrorPrefix(err, "NOSCRIPT") {
			res = c.rdb.Eval(ctx, s.src, keys, args...)
		} else {
			return nil, err
		}
	}
	return res.Result()
}

// Acquire takes the single-runner lock (SET NX EX). Returns false
// when another replica holds it — the caller skips this round.
func (c *Client) Acquire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := c.rdb.SetNX(ctx, Prefix+key, "1", ttl).Result()
	return ok, err
}

// warn is the shared failure logger (fail-open paths stay loud).
func warn(op string, err error) {
	log.Printf("redis %s failed (fail-open path): %v", op, err)
}
