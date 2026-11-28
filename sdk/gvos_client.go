// Package gvos is the minimal Go SDK for the gvos tenant plane
// (api-roadmap Phase E first slice). It wraps only the Phase C
// read-only surface — the stable contract. Generated clients
// (oapi-codegen) replace this once the spec lands in CI (Phase E
// step 2); the method surface below is the compatibility promise.
package gvos

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"iter"
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

// --- Error envelope + verbs (Phase E extension) --------------------

// APIError is the wire error envelope (api-conventions §3).
type APIError struct {
	HTTPStatus int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gvos: %d %s: %s", e.HTTPStatus, e.Code, e.Message)
}

// IsCode reports whether err carries the stable code (nil-safe).
func IsCode(err error, code string) bool {
	ae, ok := err.(*APIError)
	return ok && ae.Code == code
}

// do runs one request: body (nil = none), idemKey adds
// Idempotency-Key. out decodes 2xx bodies; error envelopes decode
// into *APIError.
func (c *Client) do(method, path string, body, out any, idemKey string) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.Base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Wire shape: {"error":{"code","message"}} (conventions §3).
		var env struct {
			Error APIError `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&env); err == nil && env.Error.Code != "" {
			env.Error.HTTPStatus = resp.StatusCode
			return &env.Error
		}
		return &APIError{
			HTTPStatus: resp.StatusCode, Code: "HTTP_ERROR",
			Message: fmt.Sprintf("%s %s: %d", method, path, resp.StatusCode),
		}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// --- Tenant write plane (M10) --------------------------------------

// ProvisionChild creates a child account under the tenant (transfer
// semantics: initial_credit allocates from the tenant's own balance).
// The SIP credential shows once in the response.
type SIPCredential struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	SIPPassword string `json:"sip_password"`
	SIPRealm    string `json:"sip_realm"`
	Credit      string `json:"credit"`
}

func (c *Client) ProvisionChild(username, name string, initialCredit float64) (*SIPCredential, error) {
	var out SIPCredential
	return &out, c.do("POST", "/v1/tenant/accounts",
		map[string]any{"username": username, "name": name, "initial_credit": initialCredit}, &out, "")
}

// Allocate moves credit down to a child (positive delta only).
func (c *Client) Allocate(childID string, delta float64) error {
	return c.do("POST", "/v1/tenant/accounts/"+childID+"/topup",
		map[string]any{"delta": delta}, nil, "")
}

// --- Origination (M11 F1) ------------------------------------------

// CallState is one lifecycle projection.
type CallState struct {
	CallID string `json:"call_id"`
	State  string `json:"state"`
}

// Originate starts a platform-dialed call (202 async; state via GET
// or the call.state webhook). idemKey makes retries safe.
func (c *Client) Originate(caller, callee, accountID string, idemKey string) (*CallState, error) {
	var out CallState
	return &out, c.do("POST", "/v1/calls",
		map[string]any{"caller": caller, "callee": callee, "account_id": accountID}, &out, idemKey)
}

func (c *Client) CallStatus(callID string) (map[string]any, error) {
	out := map[string]any{}
	return out, c.do("GET", "/v1/calls/"+callID, nil, &out, "")
}

func (c *Client) CancelCall(callID string) error {
	return c.do("DELETE", "/v1/calls/"+callID, nil, nil, "")
}

// --- Cursor pagination helper --------------------------------------

// Pager walks any cursor-paginated list face (page = request fn
// returning next_cursor; the SDK stops on the empty cursor).
func Pager[T any](c *Client, pathOf func(cursor string) string, page func(out map[string]any) []T) iter.Seq[T] {
	return func(yield func(T) bool) {
		cursor := ""
		for {
			out := map[string]any{}
			if err := c.do("GET", pathOf(cursor), nil, &out, ""); err != nil {
				return
			}
			for _, v := range page(out) {
				if !yield(v) {
					return
				}
			}
			next, _ := out["next_cursor"].(string)
			if next == "" {
				return
			}
			cursor = next
		}
	}
}
