package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAPIDocument(t *testing.T) {
	s := NewServer()
	srv := httptest.NewServer(MuxFor(s))
	defer srv.Close()
	defer flush(s)

	resp, err := http.Get(srv.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("openapi=%v", doc["openapi"])
	}
	comp, _ := doc["components"].(map[string]any)
	if comp == nil {
		t.Fatal("missing components")
	}
	schemes, _ := comp["securitySchemes"].(map[string]any)
	if schemes["BearerAuth"] == nil || schemes["InternalToken"] == nil {
		t.Fatalf("securitySchemes=%v", schemes)
	}
	paths, _ := doc["paths"].(map[string]any)
	if paths["/v1/admin/axb/bindings"] == nil {
		t.Fatal("missing axb path")
	}
}
