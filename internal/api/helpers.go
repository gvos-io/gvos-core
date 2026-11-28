// Shared helpers for the admin/event handlers. Kept tiny on purpose:
// these exist so admin.go stays declarative.
package api

import "github.com/gvos-io/gvos-core/internal/shared/acl"

import (
	"context"
	"log"
	"time"
)

func logPrintf(format string, args ...any) { log.Printf(format, args...) }

func contextWithTimeoutDuration(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// NewTestKey mints a key bound to (role, tenant) in the server's acl
// store and returns the plaintext. Test infrastructure, exported for
// the SDK e2e test (in-package tests use issueKey).
func NewTestKey(s *Server, role acl.Role, tenantID string) string {
	plaintext, prefix, hash := acl.GenerateKey(role)
	s.acl.Add(acl.Key{ID: prefix, TenantID: tenantID, Role: role, Prefix: prefix, Hash: hash})
	return plaintext
}
