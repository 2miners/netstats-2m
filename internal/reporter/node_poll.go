package reporter

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"
)

// chainAPI is what a polled node has to provide; pollNode turns it into a
// Node. Blocks are addressed by height only.
type chainAPI interface {
	// info: node name/version and network name.
	info(ctx context.Context) (node string, net any, err error)
	// tip: best height and its hash (a changed hash means a new head).
	tip(ctx context.Context) (height float64, hash string, err error)
	// block at a height in the netstats block format (see newBlock).
	block(ctx context.Context, height float64) (map[string]any, error)
	peers(ctx context.Context) (float64, error)
	pending(ctx context.Context) (float64, error)
	// sync: current and highest known height; syncing when behind.
	sync(ctx context.Context) (cur, high float64, syncing bool, err error)
}

type pollNode struct {
	api    chainAPI
	every  time.Duration
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	syncStart float64
	syncing   bool
}

// pollDialer checks the node with tip() and returns a Node that polls it.
func pollDialer(every time.Duration, mk func() chainAPI) Dialer {
	return func(ctx context.Context) (Node, error) {
		api := mk()
		if _, _, err := api.tip(ctx); err != nil {
			return nil, err
		}
		nctx, cancel := context.WithCancel(ctx)
		return &pollNode{api: api, every: every, ctx: nctx, cancel: cancel}, nil
	}
}

func (n *pollNode) Done() <-chan struct{} { return n.ctx.Done() }
func (n *pollNode) Close()                { n.cancel() }

func (n *pollNode) Info() (string, any, any, error) {
	node, net, err := n.api.info(n.ctx)
	return node, net, nil, err
}

func (n *pollNode) Block(ref any) (map[string]any, error) {
	h, ok := ref.(float64)
	if !ok { // "latest" or a hash: take the tip
		var err error
		if h, _, err = n.api.tip(n.ctx); err != nil {
			return nil, err
		}
	}
	return n.api.block(n.ctx, h)
}

func (n *pollNode) Pending() (float64, error) { return n.api.pending(n.ctx) }

func (n *pollNode) Syncing() (any, error) {
	cur, high, syncing, err := n.api.sync(n.ctx)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !syncing {
		n.syncing = false
		return false, nil
	}
	if !n.syncing {
		n.syncing, n.syncStart = true, cur
	}
	return map[string]any{"startingBlock": n.syncStart, "currentBlock": cur, "highestBlock": high}, nil
}

func (n *pollNode) Stats() (NodeStats, error) {
	peers, err := n.api.peers(n.ctx)
	if err != nil {
		return NodeStats{}, err
	}
	s, err := n.Syncing()
	if err != nil {
		return NodeStats{}, err
	}
	return NodeStats{Peers: peers, GasPrice: "0", Syncing: s}, nil
}

func (n *pollNode) Watch(log *slog.Logger, onHead func(ref any), onPending func()) {
	go func() {
		_, last, _ := n.api.tip(n.ctx)
		head := time.NewTicker(n.every)
		pend := time.NewTicker(2 * time.Second)
		defer head.Stop()
		defer pend.Stop()
		lastPending := -1.0
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-head.C:
				h, hash, err := n.api.tip(n.ctx)
				if err != nil {
					if n.ctx.Err() == nil {
						log.Warn("RPC poll failed, reconnecting", "err", err)
						n.Close()
					}
					return
				}
				if hash != last {
					last = hash
					onHead(h)
				}
			case <-pend.C:
				p, err := n.api.pending(n.ctx)
				if err == nil && p != lastPending {
					lastPending = p
					onPending()
				}
			}
		}
	}()
	log.Info("polling node for new blocks", "every", n.every)
}

// newBlock builds a netstats block; fees < 0 means unknown.
func newBlock(height float64, hash, parent string, ts float64, miner string, diff, total float64, txs, size float64, fees float64) map[string]any {
	var f any
	if fees >= 0 {
		f = fees
	}
	return map[string]any{
		"number":          height,
		"hash":            hash,
		"parentHash":      parent,
		"timestamp":       ts,
		"miner":           miner,
		"difficulty":      diff,
		"totalDifficulty": total,
		"size":            size,
		"fees":            f,
		"gasUsed":         float64(0),
		"gasLimit":        float64(0),
		"transactions":    []any{},
		"txCount":         txs,
		"uncles":          []any{},
	}
}

// ---------------- helpers for node JSON ----------------

// num reads a JSON number, a decimal string, a 0x-hex string or a
// "1,234,567" string as float64.
func num(v any) float64 {
	switch x := v.(type) {
	case json.Number:
		f, _ := x.Float64()
		return f
	case float64:
		return x
	case string:
		s := strings.ReplaceAll(strings.TrimSpace(x), ",", "")
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			return hexFloat(s[2:])
		}
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	return 0
}

func hexFloat(h string) float64 {
	b, ok := new(big.Int).SetString(h, 16)
	if !ok {
		return 0
	}
	f, _ := new(big.Float).SetInt(b).Float64()
	return f
}

// compactToDiff turns a Bitcoin-style compact target into difficulty
// relative to maxTarget.
func compactToDiff(compact uint32, maxTarget *big.Int) float64 {
	exp := uint(compact >> 24)
	mant := new(big.Int).SetUint64(uint64(compact & 0x00ffffff))
	t := new(big.Int)
	if exp <= 3 {
		t.Rsh(mant, 8*(3-exp))
	} else {
		t.Lsh(mant, 8*(exp-3))
	}
	if t.Sign() == 0 {
		return 0
	}
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(maxTarget), new(big.Float).SetInt(t)).Float64()
	if math.IsInf(f, 0) {
		return 0
	}
	return f
}

// obj walks nested maps: obj(m, "a", "b") = m["a"]["b"].
func obj(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}
