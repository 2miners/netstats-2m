package kaspa

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

// KaspadMessage payload field numbers (protowire messages.proto).
const (
	fGetConnectedPeerInfoReq  = 1016
	fGetConnectedPeerInfoResp = 1017
	fGetBlockReq              = 1025
	fGetBlockResp             = 1026
	fGetBlockDagInfoReq       = 1035
	fGetBlockDagInfoResp      = 1036
	fGetInfoReq               = 1063
	fGetInfoResp              = 1064
	fEstimateHashrateReq      = 1072
	fEstimateHashrateResp     = 1073

	fError = 1000 // RPCError in every response
)

const callTimeout = 15 * time.Second

// Client is one MessageStream; calls are serialised (one request in flight)
// and answered in order.
type Client struct {
	mu     sync.Mutex
	pw     *io.PipeWriter
	body   io.ReadCloser
	br     *bufio.Reader
	respCh chan roundTrip
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

type roundTrip struct {
	resp *http.Response
	err  error
}

// Dial opens the stream to a kaspad gRPC address (host:port, plaintext).
func Dial(ctx context.Context, addr string) (*Client, error) {
	tr := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, a)
		},
	}
	sctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(sctx, http.MethodPost, "http://"+addr+"/protowire.RPC/MessageStream", pr)
	if err != nil {
		cancel()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/grpc")
	req.Header.Set("TE", "trailers")
	c := &Client{pw: pw, cancel: cancel, done: make(chan struct{}), respCh: make(chan roundTrip, 1)}
	// the server may hold its response headers until it has something to
	// send, so the round trip runs while the first request is written
	go func() {
		resp, err := tr.RoundTrip(req)
		c.respCh <- roundTrip{resp, err}
	}()
	// first call proves the stream works
	if _, err := c.call(ctx, fGetInfoReq, nil, fGetInfoResp); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) Done() <-chan struct{} { return c.done }

func (c *Client) Close() {
	c.once.Do(func() {
		close(c.done)
		c.cancel()
		_ = c.pw.Close()
		if c.body != nil {
			_ = c.body.Close()
		}
	})
}

// call sends one request (payload field req with body) and waits for the
// message carrying field want, skipping anything else (notifications).
func (c *Client) call(ctx context.Context, req int, body []byte, want int) (msg, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil, errors.New("kaspa: stream closed")
	default:
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	payload := appendBytes(nil, req, body) // KaspadMessage{oneof payload}
	frame := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(frame[1:], uint32(len(payload)))
	frame = append(frame, payload...)

	type result struct {
		m   msg
		err error
	}
	res := make(chan result, 1)
	go func() {
		if _, err := c.pw.Write(frame); err != nil {
			res <- result{nil, err}
			return
		}
		if c.br == nil {
			rt := <-c.respCh
			if rt.err != nil {
				res <- result{nil, rt.err}
				return
			}
			if rt.resp.StatusCode != http.StatusOK {
				res <- result{nil, fmt.Errorf("kaspa: HTTP %d", rt.resp.StatusCode)}
				return
			}
			c.body = rt.resp.Body
			c.br = bufio.NewReader(rt.resp.Body)
		}
		for {
			var hdr [5]byte
			if _, err := io.ReadFull(c.br, hdr[:]); err != nil {
				res <- result{nil, err}
				return
			}
			buf := make([]byte, binary.BigEndian.Uint32(hdr[1:]))
			if _, err := io.ReadFull(c.br, buf); err != nil {
				res <- result{nil, err}
				return
			}
			m, err := decode(buf)
			if err != nil {
				res <- result{nil, err}
				return
			}
			if _, ok := m[want]; ok {
				r := m.sub(want)
				if e := r.sub(fError); e.str(1) != "" {
					res <- result{nil, fmt.Errorf("kaspa: %s", e.str(1))}
					return
				}
				res <- result{r, nil}
				return
			}
		}
	}()
	select {
	case r := <-res:
		if r.err != nil {
			c.Close()
		}
		return r.m, r.err
	case <-ctx.Done():
		c.Close() // a half-read stream can't be reused
		return nil, ctx.Err()
	}
}

// DagInfo is GetBlockDagInfo.
type DagInfo struct {
	Network    string
	Difficulty float64
	DaaScore   uint64
	Tips       []string
	Sink       string // virtual selected parent (rusty-kaspa field 10); "" on older nodes
}

func (c *Client) DagInfo(ctx context.Context) (DagInfo, error) {
	m, err := c.call(ctx, fGetBlockDagInfoReq, nil, fGetBlockDagInfoResp)
	if err != nil {
		return DagInfo{}, err
	}
	return DagInfo{Network: m.str(1), Difficulty: m.double(5), DaaScore: m.uint(9), Tips: m.strs(4), Sink: m.str(10)}, nil
}

// Block is GetBlock without transactions.
type Block struct {
	Hash, SelectedParent, BlueWork string
	Timestamp                      int64 // ms
	DaaScore, BlueScore            uint64
	Difficulty                     float64
	TxCount                        int
}

func (c *Client) Block(ctx context.Context, hash string) (Block, error) {
	req := appendString(nil, 1, hash) // includeTransactions (3) = false
	m, err := c.call(ctx, fGetBlockReq, req, fGetBlockResp)
	if err != nil {
		return Block{}, err
	}
	b := m.sub(3)
	h, vd := b.sub(1), b.sub(3)
	return Block{
		Hash: vd.str(1), SelectedParent: vd.str(13), BlueWork: h.str(10),
		Timestamp: int64(h.uint(6)), DaaScore: h.uint(9), BlueScore: h.uint(13),
		Difficulty: vd.double(11), TxCount: vd.count(14),
	}, nil
}

// Info is GetInfo: mempool size, version, sync state.
type Info struct {
	Mempool uint64
	Version string
	Synced  bool
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	m, err := c.call(ctx, fGetInfoReq, nil, fGetInfoResp)
	if err != nil {
		return Info{}, err
	}
	return Info{Mempool: m.uint(2), Version: m.str(3), Synced: m.bool(5)}, nil
}

// Peers is the number of connected peers.
func (c *Client) Peers(ctx context.Context) (int, error) {
	m, err := c.call(ctx, fGetConnectedPeerInfoReq, nil, fGetConnectedPeerInfoResp)
	if err != nil {
		return 0, err
	}
	return m.count(1), nil
}

// Hashrate is EstimateNetworkHashesPerSecond over the last window blocks.
func (c *Client) Hashrate(ctx context.Context, window uint32) (uint64, error) {
	m, err := c.call(ctx, fEstimateHashrateReq, appendUint(nil, 1, uint64(window)), fEstimateHashrateResp)
	if err != nil {
		return 0, err
	}
	return m.uint(1), nil
}
