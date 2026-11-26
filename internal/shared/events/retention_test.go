package events

import (
	"strings"
	"testing"
	"time"
)

// A broker URL carries credentials; it must never reach the log with
// them.
func TestRedactURLHidesPasswords(t *testing.T) {
	got := redactURL("nats://gvos-core:s3cret@nats-1:4222, nats://gvos-core:s3cret@nats-2:4222,nats://plain:4222")
	if strings.Contains(got, "s3cret") {
		t.Fatalf("password still present: %q", got)
	}
	if got != "nats://gvos-core@nats-1:4222,nats://gvos-core@nats-2:4222,nats://plain:4222" {
		t.Errorf("redactURL = %q", got)
	}
}

// The stream's retention: a bounded default, overridable, invalid
// values fall back to the default.
func TestStreamRetention(t *testing.T) {
	t.Setenv("GVOS_JS_MAX_AGE", "")
	t.Setenv("GVOS_JS_MAX_BYTES", "")
	if streamMaxAge() != DefaultStreamMaxAge {
		t.Errorf("default max age = %s, want %s", streamMaxAge(), DefaultStreamMaxAge)
	}
	if streamMaxBytes() != 0 {
		t.Errorf("default byte cap = %d, want 0 (none)", streamMaxBytes())
	}

	t.Setenv("GVOS_JS_MAX_AGE", "48h")
	t.Setenv("GVOS_JS_MAX_BYTES", "1073741824")
	if streamMaxAge() != 48*time.Hour || streamMaxBytes() != 1073741824 {
		t.Errorf("overrides -> max age %s, byte cap %d", streamMaxAge(), streamMaxBytes())
	}

	t.Setenv("GVOS_JS_MAX_AGE", "garbage")
	t.Setenv("GVOS_JS_MAX_BYTES", "-5")
	if streamMaxAge() != DefaultStreamMaxAge || streamMaxBytes() != 0 {
		t.Errorf("invalid overrides -> max age %s, byte cap %d; want the defaults", streamMaxAge(), streamMaxBytes())
	}
}
