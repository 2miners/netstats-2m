// Package primus is a client for a Primus server ("websockets" transformer, JSON
// parser, primus-emit plugin), as used by the netstats server on /api.
//
// It answers "primus::ping::<ms>" with "primus::pong::<ms>" (Primus 7+ servers
// ping), and sends its own "primus::ping::<ms>" every 25 s (Primus ≤6 servers
// close a client that stays silent for 35 s). "primus::server::close" means the
// server refused us (bad secret): the caller then waits longer before retrying.
package primus

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pingEvery = 25 * time.Second
	writeWait = 10 * time.Second
	idleLimit = 90 * time.Second
)

// ErrServerClose is returned by Run when the server sent primus::server::close.
var ErrServerClose = errors.New("server closed the connection (primus::server::close)")

// Conn is one open connection.
type Conn struct {
	ws      *websocket.Conn
	writeMu sync.Mutex
}

// Emit sends {"emit": [event, args...]}.
func (c *Conn) Emit(event string, args ...any) error {
	b, err := json.Marshal(map[string]any{"emit": append([]any{event}, args...)})
	if err != nil {
		return err
	}
	return c.write(b)
}

func (c *Conn) write(b []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	return c.ws.WriteMessage(websocket.TextMessage, b)
}

func (c *Conn) writeString(s string) error {
	b, _ := json.Marshal(s)
	return c.write(b)
}

// Close closes the connection.
func (c *Conn) Close() { _ = c.ws.Close() }

// Endpoint turns "wss://host" (+ pathname "/api") into the WebSocket URL.
func Endpoint(server, pathname string) (string, error) {
	u, err := url.Parse(server)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	u.Path = strings.TrimRight(u.Path, "/") + pathname
	q := u.Query()
	q.Set("_primuscb", strconv.FormatInt(time.Now().UnixMilli(), 10)+"-0")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// Run dials, calls onOpen, then dispatches events to onEvent until the
// connection ends. It returns the reason.
func Run(ctx context.Context, server, pathname string, onOpen func(*Conn), onEvent func(c *Conn, event string, args []json.RawMessage)) error {
	ep, err := Endpoint(server, pathname)
	if err != nil {
		return err
	}
	d := websocket.Dialer{HandshakeTimeout: 15 * time.Second, Proxy: websocket.DefaultDialer.Proxy}
	ws, _, err := d.DialContext(ctx, ep, nil)
	if err != nil {
		return err
	}
	c := &Conn{ws: ws}
	defer c.Close()

	ws.SetReadLimit(10 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(idleLimit))
	ws.SetPingHandler(func(data string) error {
		_ = ws.SetReadDeadline(time.Now().Add(idleLimit))
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		return ws.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(writeWait))
	})

	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				c.Close()
				return
			case <-t.C:
				if c.writeString("primus::ping::"+strconv.FormatInt(time.Now().UnixMilli(), 10)) != nil {
					c.Close()
					return
				}
			}
		}
	}()

	onOpen(c)

	for {
		_, raw, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		_ = ws.SetReadDeadline(time.Now().Add(idleLimit))

		if len(raw) > 0 && raw[0] == '"' {
			var s string
			if json.Unmarshal(raw, &s) != nil || !strings.HasPrefix(s, "primus::") {
				continue
			}
			switch {
			case strings.HasPrefix(s, "primus::ping::"):
				_ = c.writeString("primus::pong::" + strings.TrimPrefix(s, "primus::ping::"))
			case s == "primus::server::close":
				return ErrServerClose
			}
			continue
		}

		var msg struct {
			Emit []json.RawMessage `json:"emit"`
		}
		if json.Unmarshal(raw, &msg) != nil || len(msg.Emit) == 0 {
			continue
		}
		var event string
		if json.Unmarshal(msg.Emit[0], &event) != nil {
			continue
		}
		onEvent(c, event, msg.Emit[1:])
	}
}
