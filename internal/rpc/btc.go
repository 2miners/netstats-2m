package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
)

// HTTPClient is a bitcoind-style JSON-RPC client (HTTP POST + basic auth),
// used for ravend and other Bitcoin forks.
type HTTPClient struct {
	url, user, pass string
	http            *http.Client
	id              atomic.Uint64
}

func NewHTTP(url, user, pass string) *HTTPClient {
	return &HTTPClient{url: url, user: user, pass: pass, http: &http.Client{Timeout: DefaultTimeout}}
}

// Call runs one request; bitcoind answers errors with HTTP 500 and a JSON body.
func (c *HTTPClient) Call(ctx context.Context, method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "1.0", "id": c.id.Add(1), "method": method, "params": params})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" || c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("%s: HTTP %d: %.200s", method, resp.StatusCode, bytes.TrimSpace(data))
	}
	if r.Error != nil {
		return fmt.Errorf("%s: %s (code %d)", method, r.Error.Message, r.Error.Code)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}
