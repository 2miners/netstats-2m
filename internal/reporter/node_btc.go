package reporter

import (
	"context"
	"log/slog"
	"math"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/2miners/netstats-2m/internal/rpc"
)

// Bitcoin forks (ravend, ...): HTTP JSON-RPC without subscriptions, so new
// blocks and the mempool are polled.
const (
	btcHeadPoll    = time.Second
	btcPendingPoll = 2 * time.Second
)

// Subsidy is the block reward schedule, needed to tell fees from the reward
// in the coinbase: Start coins at height 0, halved every Halving blocks.
type Subsidy struct {
	Start   float64
	Halving int64
}

// at returns the reward at height h in satoshis.
func (s Subsidy) at(h int64) int64 {
	if s.Halving <= 0 {
		return 0
	}
	n := h / s.Halving
	if n >= 63 {
		return 0
	}
	return int64(math.Round(s.Start*1e8)) >> n
}

type btcNode struct {
	sub    Subsidy
	c      *rpc.HTTPClient
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	syncStart float64 // first height seen while syncing
	syncing   bool
}

func BTCDialer(url, user, pass string, sub Subsidy) Dialer {
	return func(ctx context.Context) (Node, error) {
		c := rpc.NewHTTP(url, user, pass)
		var probe struct {
			Blocks float64 `json:"blocks"`
		}
		if err := c.Call(ctx, "getblockchaininfo", &probe); err != nil {
			return nil, err
		}
		nctx, cancel := context.WithCancel(ctx)
		return &btcNode{c: c, ctx: nctx, cancel: cancel, sub: sub}, nil
	}
}

func (n *btcNode) Done() <-chan struct{} { return n.ctx.Done() }
func (n *btcNode) Close()                { n.cancel() }

func (n *btcNode) call(method string, out any, params ...any) error {
	return n.c.Call(n.ctx, method, out, params...)
}

var subverRe = regexp.MustCompile(`^/?([^:/]+):([^/(]+)`)

// Info: "/Ravencoin:4.8.0/" -> "Ravencoin/v4.8.0", net = chain name ("main").
func (n *btcNode) Info() (string, any, any, error) {
	var ni struct {
		Subversion string `json:"subversion"`
	}
	if err := n.call("getnetworkinfo", &ni); err != nil {
		return "", nil, nil, err
	}
	var bi struct {
		Chain string `json:"chain"`
	}
	if err := n.call("getblockchaininfo", &bi); err != nil {
		return "", nil, nil, err
	}
	node := strings.Trim(ni.Subversion, "/")
	if m := subverRe.FindStringSubmatch(ni.Subversion); m != nil {
		node = m[1] + "/v" + m[2]
	}
	return node, bi.Chain, nil, nil
}

type btcBlock struct {
	Hash         string  `json:"hash"`
	Height       float64 `json:"height"`
	Size         float64 `json:"size"`
	MerkleRoot   string  `json:"merkleroot"`
	Time         float64 `json:"time"`
	Difficulty   float64 `json:"difficulty"`
	Chainwork    string  `json:"chainwork"`
	PreviousHash string  `json:"previousblockhash"`
	Tx           []struct {
		Txid string `json:"txid"`
		Vin  []struct {
			Coinbase string `json:"coinbase"`
		} `json:"vin"`
		Vout []struct {
			Value        float64 `json:"value"`
			ScriptPubKey struct {
				Addresses []string `json:"addresses"`
				Address   string   `json:"address"`
			} `json:"scriptPubKey"`
		} `json:"vout"`
	} `json:"tx"`
}

// Block maps a bitcoind block onto the netstats (Ethereum) block format:
// miner = first paid coinbase address, totalDifficulty = chainwork, no gas,
// no uncles; fees = coinbase outputs minus the block reward (coins).
func (n *btcNode) Block(ref any) (map[string]any, error) {
	var hash string
	switch v := ref.(type) {
	case string:
		if v == "latest" || v == "" {
			if err := n.call("getbestblockhash", &hash); err != nil {
				return nil, err
			}
		} else {
			hash = v
		}
	case float64:
		if err := n.call("getblockhash", &hash, int64(v)); err != nil {
			return nil, err
		}
	default:
		if err := n.call("getbestblockhash", &hash); err != nil {
			return nil, err
		}
	}
	var b btcBlock
	if err := n.call("getblock", &b, hash, 2); err != nil {
		return nil, err
	}
	txs := make([]any, 0, len(b.Tx))
	var miner any = ""
	var fees any // unknown unless the block has a coinbase
	for i, tx := range b.Tx {
		txs = append(txs, tx.Txid)
		if i == 0 && len(tx.Vin) > 0 && tx.Vin[0].Coinbase != "" {
			var out int64
			for _, o := range tx.Vout {
				out += int64(math.Round(o.Value * 1e8))
			}
			if f := out - n.sub.at(int64(b.Height)); f > 0 {
				fees = float64(f) / 1e8
			} else {
				fees = float64(0)
			}
			for _, o := range tx.Vout {
				addr := o.ScriptPubKey.Address
				if addr == "" && len(o.ScriptPubKey.Addresses) > 0 {
					addr = o.ScriptPubKey.Addresses[0]
				}
				if o.Value > 0 && addr != "" {
					miner = addr
					break
				}
			}
		}
	}
	work := 0.0
	if w, ok := new(big.Int).SetString(b.Chainwork, 16); ok {
		work, _ = new(big.Float).SetInt(w).Float64()
	}
	return map[string]any{
		"number":           b.Height,
		"hash":             b.Hash,
		"parentHash":       b.PreviousHash,
		"timestamp":        b.Time,
		"miner":            miner,
		"difficulty":       b.Difficulty,
		"totalDifficulty":  work,
		"transactionsRoot": b.MerkleRoot,
		"size":             b.Size,
		"fees":             fees,
		"gasUsed":          float64(0),
		"gasLimit":         float64(0),
		"transactions":     txs,
		"uncles":           []any{},
	}, nil
}

func (n *btcNode) Pending() (float64, error) {
	var m struct {
		Size float64 `json:"size"`
	}
	if err := n.call("getmempoolinfo", &m); err != nil {
		return 0, err
	}
	return m.Size, nil
}

func (n *btcNode) Syncing() (any, error) {
	var bi struct {
		Blocks  float64 `json:"blocks"`
		Headers float64 `json:"headers"`
		IBD     bool    `json:"initialblockdownload"`
	}
	if err := n.call("getblockchaininfo", &bi); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if !bi.IBD && bi.Headers-bi.Blocks <= 2 {
		n.syncing = false
		return false, nil
	}
	if !n.syncing {
		n.syncing = true
		n.syncStart = bi.Blocks
	}
	return map[string]any{"startingBlock": n.syncStart, "currentBlock": bi.Blocks, "highestBlock": bi.Headers}, nil
}

func (n *btcNode) Stats() (NodeStats, error) {
	var peers float64
	if err := n.call("getconnectioncount", &peers); err != nil {
		return NodeStats{}, err
	}
	s, err := n.Syncing()
	if err != nil {
		return NodeStats{}, err
	}
	return NodeStats{Peers: peers, Mining: false, GasPrice: "0", Syncing: s}, nil
}

// Watch polls the best block hash and the mempool size; an RPC error closes
// the node so the reporter reconnects and marks it down meanwhile.
func (n *btcNode) Watch(log *slog.Logger, onHead func(ref any), onPending func()) {
	go func() {
		var last string
		_ = n.call("getbestblockhash", &last)
		head := time.NewTicker(btcHeadPoll)
		pend := time.NewTicker(btcPendingPoll)
		defer head.Stop()
		defer pend.Stop()
		lastPending := -1.0
		for {
			select {
			case <-n.ctx.Done():
				return
			case <-head.C:
				var h string
				if err := n.call("getbestblockhash", &h); err != nil {
					if n.ctx.Err() == nil {
						log.Warn("RPC poll failed, reconnecting", "err", err)
						n.Close()
					}
					return
				}
				if h != last {
					last = h
					onHead(h)
				}
			case <-pend.C:
				p, err := n.Pending()
				if err == nil && p != lastPending {
					lastPending = p
					onPending()
				}
			}
		}
	}()
	log.Info("polling node for new blocks", "every", btcHeadPoll)
}
