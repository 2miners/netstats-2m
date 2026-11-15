package rpc

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// JSONClient talks plain HTTP+JSON to node APIs (REST or JSON-RPC 2.0) with
// optional basic or digest auth (monerod's rpc-login is digest).
type JSONClient struct {
	Base       string // http://host:port
	User, Pass string
	http       *http.Client
	id         atomic.Uint64

	mu     sync.Mutex
	digest map[string]string // last digest challenge
	nc     int
}

func NewJSON(base, user, pass string) *JSONClient {
	return &JSONClient{Base: strings.TrimRight(base, "/"), User: user, Pass: pass, http: &http.Client{Timeout: DefaultTimeout}}
}

// HTTPError is a non-2xx answer.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %.200s", e.Status, e.Body) }

// Get decodes GET base+path into out.
func (c *JSONClient) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post sends body as JSON and decodes the answer into out.
func (c *JSONClient) Post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, b, out)
}

// Call2 is a JSON-RPC 2.0 call at base+path; params may be a list or an object.
func (c *JSONClient) Call2(ctx context.Context, path, method string, params, out any) error {
	if params == nil {
		params = []any{}
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	err := c.Post(ctx, path, map[string]any{"jsonrpc": "2.0", "id": c.id.Add(1), "method": method, "params": params}, &r)
	if err != nil {
		var he *HTTPError
		// some nodes answer JSON-RPC errors with HTTP 500 and a JSON body
		if !(errorsAs(err, &he) && json.Unmarshal([]byte(he.Body), &r) == nil && r.Error != nil) {
			return err
		}
	}
	if r.Error != nil {
		return &Error{Method: method, Code: r.Error.Code, Message: r.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func errorsAs(err error, he **HTTPError) bool {
	e, ok := err.(*HTTPError)
	if ok {
		*he = e
	}
	return ok
}

func (c *JSONClient) do(ctx context.Context, method, path string, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, DefaultTimeout)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, bytes.NewReader(body))
		if err != nil {
			return err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		c.auth(req, path)
		resp, err := c.http.Do(req)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 && c.setDigest(resp.Header.Get("WWW-Authenticate")) {
			continue // retry with the digest answer
		}
		if resp.StatusCode/100 != 2 {
			return &HTTPError{Status: resp.StatusCode, Body: string(bytes.TrimSpace(data))}
		}
		if out == nil {
			return nil
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		return dec.Decode(out)
	}
	return &HTTPError{Status: http.StatusUnauthorized}
}

func (c *JSONClient) auth(req *http.Request, path string) {
	if c.User == "" && c.Pass == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.digest == nil {
		req.SetBasicAuth(c.User, c.Pass)
		return
	}
	d := c.digest
	c.nc++
	nc := fmt.Sprintf("%08x", c.nc)
	cb := make([]byte, 8)
	_, _ = rand.Read(cb)
	cnonce := hex.EncodeToString(cb)
	ha1 := md5hex(c.User + ":" + d["realm"] + ":" + c.Pass)
	ha2 := md5hex(req.Method + ":" + path)
	var resp string
	if strings.Contains(d["qop"], "auth") {
		resp = md5hex(ha1 + ":" + d["nonce"] + ":" + nc + ":" + cnonce + ":auth:" + ha2)
	} else {
		resp = md5hex(ha1 + ":" + d["nonce"] + ":" + ha2)
	}
	h := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", algorithm=MD5, response="%s"`,
		c.User, d["realm"], d["nonce"], path, resp)
	if strings.Contains(d["qop"], "auth") {
		h += fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	}
	if d["opaque"] != "" {
		h += fmt.Sprintf(`, opaque="%s"`, d["opaque"])
	}
	req.Header.Set("Authorization", h)
}

// setDigest stores a Digest challenge; false when the server wants something else.
func (c *JSONClient) setDigest(h string) bool {
	if c.User == "" && c.Pass == "" || !strings.HasPrefix(strings.ToLower(h), "digest ") {
		return false
	}
	d := map[string]string{}
	for _, part := range splitParams(h[len("digest "):]) {
		k, v, ok := strings.Cut(part, "=")
		if ok {
			d[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	c.mu.Lock()
	c.digest, c.nc = d, 0
	c.mu.Unlock()
	return true
}

// splitParams splits a=b, c="d, e" on commas outside quotes.
func splitParams(s string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			cur.WriteRune(r)
		case r == ',' && !inQ:
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}
