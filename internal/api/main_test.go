package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"
)

// TestMain pins the internal token for the signaling face (Phase A
// fail-closed guard) so call-plane tests authenticate like Kamailio.
func TestMain(m *testing.M) {
	os.Setenv("GVOS_INTERNAL_TOKEN", "test-internal-token")
	os.Exit(m.Run())
}

// postInternal is the test-facing POST helper that carries the
// X-GVOS-Internal header (TestMain sets the env).
func postInternal(t *testing.T, url string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// postInternalRaw posts with the internal header and returns the raw response.
func postInternalRaw(t *testing.T, url string, body []byte) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header.Set("X-GVOS-Internal", os.Getenv("GVOS_INTERNAL_TOKEN"))
	return http.DefaultClient.Do(req)
}
