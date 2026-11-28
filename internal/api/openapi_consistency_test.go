// Spec↔route consistency (api-roadmap Phase E seed): every endpoint
// the OpenAPI registry promises must actually be routed. This is the
// regression net for the class of bugs where the spec promised an
// interface the code never implemented (GET /v1/admin/recording used
// to 405 forever; a tenant webhook DELETE route registration was
// silently missed). The reverse direction (unrouted→spec) is covered
// by review — ServeMux exposes no pattern introspection.
package api

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAPISpecRoutesAreRouted(t *testing.T) {
	h := MuxFor(NewServer())
	for _, e := range openAPIEndpoints {
		path := strings.ReplaceAll(e.Path, "{id}", "probe-404-if-missing")
		path = strings.ReplaceAll(path, "{prefix}", "probe")
		path = strings.ReplaceAll(path, "{number}", "probe")
		path = strings.ReplaceAll(path, "{x}", "probe")
		path = strings.ReplaceAll(path, "{call_id}", "probe")

		var body *bytes.Reader
		if e.Body {
			body = bytes.NewReader([]byte(`{}`))
		} else {
			body = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(e.Method, path, body)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		// A missing route is a plaintext "404 page not found" from the
		// mux; a routed one answers JSON (auth 401/403, handler 404
		// envelope, 405, 200…). The auth handlers run before the
		// router can 404 on path params, so handler-level 404s arrive
		// as JSON envelopes — distinguishable from the router's.
		if rec.Code == 404 && !strings.Contains(rec.Header().Get("Content-Type"), "json") &&
			strings.Contains(rec.Body.String(), "page not found") {
			t.Errorf("spec lists %s %s but the mux has no such route (router 404)", e.Method, e.Path)
		}
	}
}

// Sanity of the detector itself: a route absent from the mux must be
// caught (guards against the probe degrading into a no-op).
func TestOpenAPIConsistencyDetectorCatchesMissingRoute(t *testing.T) {
	h := MuxFor(NewServer())
	req := httptest.NewRequest("GET", "/v1/admin/definitely-not-a-route", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !(rec.Code == 404 && strings.Contains(rec.Body.String(), "page not found")) {
		t.Fatalf("detector premise broken: unrouted path answered %d %s", rec.Code, rec.Body.String())
	}
}
