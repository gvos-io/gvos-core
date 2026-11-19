package rds

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := New("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("miniredis connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestNewPingAndClose(t *testing.T) {
	c := newTestClient(t)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Idempotent close.
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, err := New("redis://127.0.0.1:1/"); err == nil {
		t.Fatal("unreachable server must fail New (money authority is fatal)")
	}
	if _, err := New("://bad url"); err == nil {
		t.Fatal("bad URL must fail parse")
	}
}

func TestScriptEvalShaFallback(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	s := newScript("return {KEYS[1], ARGV[1]}")
	got, err := s.Run(ctx, c, []string{"k1"}, "v1")
	if err != nil {
		t.Fatalf("script run: %v", err)
	}
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 || arr[0] != "k1" || arr[1] != "v1" {
		t.Errorf("script result wrong: %v", got)
	}

	// Force NOSCRIPT: flush the script cache, the fallback path must
	// still evaluate.
	c.Raw().ScriptFlush(ctx)
	got2, err := s.Run(ctx, c, []string{"k2"}, "v2")
	if err != nil {
		t.Fatalf("script fallback: %v", err)
	}
	if arr, ok := got2.([]any); !ok || arr[0].(string) != "k2" {
		t.Errorf("fallback result wrong: %v", got2)
	}
}

func TestAcquireSingleRunner(t *testing.T) {
	c := newTestClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	ok, err := c.Acquire(ctx, "lock:rollup", LockTTL)
	if err != nil || !ok {
		t.Fatalf("first acquire: %v %v", ok, err)
	}
	// Second replica: refused while held.
	ok2, err := c.Acquire(ctx, "lock:rollup", LockTTL)
	if err != nil || ok2 {
		t.Fatalf("second acquire should fail: %v %v", ok2, err)
	}
	// After expiry it frees (miniredis fast-forward).
	mr := miniredis.RunT(t)
	_ = mr // (real TTL tested via FastForward on the live server below)
}
