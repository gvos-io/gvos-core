package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// Phase E gate: the served spec must parse, cover every tenant-plane
// route, and carry the OpenAPI 3.1 marker (contract = implementation).
func TestOpenAPISpecCoversTenantPlane(t *testing.T) {
	s := NewServer()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/openapi.json", nil)
	s.handleOpenAPI(w, req)
	if w.Code != 200 {
		t.Fatalf("openapi status = %d", w.Code)
	}
	var spec struct {
		OpenAPI string `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &spec); err != nil {
		t.Fatalf("spec not json: %v", err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.1") {
		t.Errorf("openapi version = %q, want 3.1.x", spec.OpenAPI)
	}
	for _, p := range []string{"/v1/tenant/me", "/v1/tenant/cdrs", "/v1/tenant/webhooks", "/v1/tenant/cdr-stream"} {
		if _, ok := spec.Paths[p]; !ok {
			t.Errorf("spec missing %s", p)
		}
	}
	// internal routes stay out of the public spec
	for p := range spec.Paths {
		if strings.Contains(p, "/v1/internal/") {
			t.Errorf("internal route %s leaked into the public spec", p)
		}
	}
}
