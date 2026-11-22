// Package gvos is the minimal Go SDK for the gvos tenant plane
// (api-roadmap Phase E first slice). It wraps only the Phase C
// read-only surface — the stable contract. Generated clients
// (oapi-codegen) replace this once the spec lands in CI (Phase E
// step 2); the method surface below is the compatibility promise.
package gvos

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Client struct {
	Base string // e.g. https://gvos.example
	Key  string // Bearer key (role tenant|agent)
	HTTP *http.Client
}

func New(base, key string) *Client {
	return &Client{Base: base, Key: key, HTTP: &http.Client{Timeout: 15 * time.Second}}
}

func (c *Client) get(path string, out any) error {
	req, err := http.NewRequest("GET", c.Base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("GET %s: %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) Me() (map[string]any, error) {
	out := map[string]any{}
	return out, c.get("/v1/tenant/me", &out)
}

func (c *Client) Accounts() ([]map[string]any, error) {
	out := map[string]any{}
	if err := c.get("/v1/tenant/accounts", &out); err != nil {
		return nil, err
	}
	v, _ := out["accounts"].([]map[string]any)
	return v, nil
}

func (c *Client) CDRs(cursor int) (map[string]any, error) {
	out := map[string]any{}
	return out, c.get(fmt.Sprintf("/v1/tenant/cdrs?cursor=%d", cursor), &out)
}

func (c *Client) Stats() (map[string]any, error) {
	out := map[string]any{}
	return out, c.get("/v1/tenant/stats", &out)
}
