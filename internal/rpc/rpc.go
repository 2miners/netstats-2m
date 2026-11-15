// Package rpc is a minimal Ethereum JSON-RPC client over one WebSocket:
// requests with a timeout, eth_subscribe notifications, and formatters that give
// the block / syncing shapes the netstats server expects (numbers as numbers,
// difficulty and totalDifficulty as decimal strings).
package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DefaultTimeout bounds every request, so a hung node shows up as down instead
// of piling up requests.
const DefaultTimeout = 15 * time.Second

var ErrClosed = errors.New("rpc connection closed")

type response struct {
	result json.RawMessage
	err    error
}

type Client struct {
	conn    *websocket.Conn
	timeout time.Duration

	mu      sync.Mutex
	id      uint64
	pending map[uint64]chan response
	subs    map[string]func(json.RawMessage)
	closed  bool
	writeMu sync.Mutex

	done chan struct{}
}

// Dial connects to ws://host:port.
func Dial(ctx context.Context, url string) (*Client, error) {
	d := websocket.Dialer{HandshakeTimeout: DefaultTimeout}
	conn, _, err := d.DialContext(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(64 << 20)
	c := &Client{
		conn:    conn,
		timeout: DefaultTimeout,
		pending: map[uint64]chan response{},
		subs:    map[string]func(json.RawMessage){},
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Done is closed when the connection is gone.
func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = map[uint64]chan response{}
	c.subs = map[string]func(json.RawMessage){}
	c.mu.Unlock()

	_ = c.conn.Close()
	for _, ch := range pending {
		ch <- response{err: ErrClosed}
	}
	close(c.done)
}

func (c *Client) readLoop() {
	defer c.Close()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		c.dispatch(data)
	}
}

type message struct {
	ID     *uint64         `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Params *struct {
		Subscription string          `json:"subscription"`
		Result       json.RawMessage `json:"result"`
	} `json:"params"`
}

func (c *Client) dispatch(data []byte) {
	var batch []message
	if len(data) > 0 && data[0] == '[' {
		if json.Unmarshal(data, &batch) != nil {
			return
		}
	} else {
		var m message
		if json.Unmarshal(data, &m) != nil {
			return
		}
		batch = []message{m}
	}
	for _, m := range batch {
		if m.Method == "eth_subscription" && m.Params != nil {
			c.mu.Lock()
			fn := c.subs[m.Params.Subscription]
			c.mu.Unlock()
			if fn != nil {
				fn(m.Params.Result)
			}
			continue
		}
		if m.ID == nil {
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[*m.ID]
		delete(c.pending, *m.ID)
		c.mu.Unlock()
		if !ok {
			continue
		}
		if m.Error != nil {
			ch <- response{err: fmt.Errorf("rpc error %d: %s", m.Error.Code, m.Error.Message)}
		} else {
			ch <- response{result: m.Result}
		}
	}
}

// Call sends a request and decodes the result into out (if not nil).
func (c *Client) Call(method string, out any, params ...any) error {
	if params == nil {
		params = []any{}
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.id++
	id := c.id
	c.pending[id] = ch
	c.mu.Unlock()

	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	c.writeMu.Lock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(c.timeout))
	err := c.conn.WriteMessage(websocket.TextMessage, req)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.Close()
		return err
	}

	t := time.NewTimer(c.timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil {
			return json.Unmarshal(r.result, out)
		}
		return nil
	case <-t.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("rpc timeout after %s (%s)", c.timeout, method)
	}
}

// Subscribe runs eth_subscribe and calls fn for every notification.
func (c *Client) Subscribe(kind string, fn func(json.RawMessage)) error {
	var id string
	if err := c.Call("eth_subscribe", &id, kind); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	c.subs[id] = fn
	return nil
}

// ---- formatters ----

// HexToFloat parses a 0x quantity into a float64 (JS Number semantics).
func HexToFloat(s string) (float64, bool) {
	n, ok := new(big.Int).SetString(trim0x(s), 16)
	if !ok {
		return 0, false
	}
	f, _ := new(big.Float).SetInt(n).Float64()
	return f, true
}

// HexToDecimal parses a 0x quantity into a decimal string.
func HexToDecimal(s string) (string, bool) {
	n, ok := new(big.Int).SetString(trim0x(s), 16)
	if !ok {
		return "", false
	}
	return n.String(), true
}

func trim0x(s string) string {
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		s = s[2:]
	}
	if s == "" {
		return "0"
	}
	return s
}

// BlockTag renders a block number for eth_getBlockByNumber.
func BlockTag(n float64) string { return "0x" + strconv.FormatUint(uint64(n), 16) }

// FormatBlock converts a raw block like web3 1.x outputBlockFormatter did.
func FormatBlock(raw json.RawMessage) map[string]any {
	var b map[string]any
	if json.Unmarshal(raw, &b) != nil || b == nil {
		return nil
	}
	for _, k := range []string{"number", "gasLimit", "gasUsed", "size", "timestamp", "baseFeePerGas"} {
		if s, ok := b[k].(string); ok {
			if f, ok := HexToFloat(s); ok {
				b[k] = f
			}
		}
	}
	for _, k := range []string{"difficulty", "totalDifficulty"} {
		if s, ok := b[k].(string); ok {
			if d, ok := HexToDecimal(s); ok {
				b[k] = d
			}
		}
	}
	return b
}

// FormatSyncing converts eth_syncing's result (false or an object of quantities).
func FormatSyncing(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	for _, k := range []string{"startingBlock", "currentBlock", "highestBlock", "knownStates", "pulledStates"} {
		if s, ok := m[k].(string); ok {
			if f, ok := HexToFloat(s); ok {
				m[k] = f
			}
		}
	}
	return m
}
