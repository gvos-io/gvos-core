// Shared helpers for the admin/event handlers. Kept tiny on purpose:
// these exist so admin.go stays declarative.
package main

import (
	"context"
	"log"
	"time"
)

func logPrintf(format string, args ...any) { log.Printf(format, args...) }

func contextWithTimeoutDuration(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
