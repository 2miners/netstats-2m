package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/2miners/netstats-2m/internal/rpc"
)

// Bitcoin forks (ravend, ...): HTTP JSON-RPC without subscriptions, so new
// blocks and the mempool are polled.
const (
	btcHeadPoll    = time.Second
	btcPendingPoll = 5 * time.Second
	// a synced node is re-checked this rarely (getblockchaininfo is one of the
	// heavier bitcoind calls); while syncing every stats round asks again
	syncRecheck = 30 * time.Second
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
	syncedAt  time.Time // last "not syncing" answer

	// what this node supports, learnt on first use (forks differ a lot)
	caps map[string]bool
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
		return &btcNode{c: c, ctx: nctx, cancel: cancel, sub: sub, caps: map[string]bool{}}, nil
	}
}

func (n *btcNode) Done() <-chan struct{} { return n.ctx.Done() }
func (n *btcNode) Close()                { n.cancel() }

func (n *btcNode) call(method string, out any, params ...any) error {
	return n.c.Call(n.ctx, method, out, params...)
}

// try runs a call that the node may not support: a node error marks the
// variant unsupported (skipped from then on), a transport error is returned.
func (n *btcNode) try(name string, method string, out any, params ...any) (ok bool, err error) {
	n.mu.Lock()
	bad := n.caps["no-"+name]
	n.mu.Unlock()
	if bad {
		return false, nil
	}
	err = n.call(method, out, params...)
	var rerr *rpc.Error
	if errors.As(err, &rerr) {
		n.mu.Lock()
		n.caps["no-"+name] = true
		n.mu.Unlock()
		return false, nil
	}
	return err == nil, err
}

// Info: "/Ravencoin:4.8.0/" -> "Ravencoin/v4.8.0" (the last part of a
// multi-part subversion: "/pearlwire:0.5.0/pearld:1.5.0/" -> "pearld/v1.5.0"),
// net = chain name ("main").
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
	return subverName(ni.Subversion), bi.Chain, nil, nil
}

func subverName(sv string) string {
	parts := strings.Split(strings.Trim(sv, "/"), "/")
	last := parts[len(parts)-1]
	if i := strings.IndexByte(last, '('); i >= 0 {
		last = last[:i]
	}
	if name, ver, ok := strings.Cut(last, ":"); ok {
		return name + "/v" + ver
	}
	return last
}

type btcTx struct {
	Txid string `json:"txid"`
	Vout []struct {
		Value        float64 `json:"value"`
		ScriptPubKey struct {
			Addresses []string `json:"addresses"`
			Address   string   `json:"address"`
		} `json:"scriptPubKey"`
	} `json:"vout"`
}

type btcBlock struct {
	Hash         string            `json:"hash"`
	Height       float64           `json:"height"`
	Size         float64           `json:"size"`
	MerkleRoot   string            `json:"merkleroot"`
	Time         float64           `json:"time"`
	Difficulty   float64           `json:"difficulty"`
	Chainwork    string            `json:"chainwork"`
	PreviousHash string            `json:"previousblockhash"`
	Tx           []json.RawMessage `json:"tx"` // txids (verbosity 1) or objects (2)
	NTx          float64           `json:"nTx"`
	TxCount      float64           `json:"txcount"`
}

// Block maps a bitcoind block onto the netstats (Ethereum) block format:
// miner = first paid coinbase address, totalDifficulty = chainwork, no gas,
// no uncles; fees in coins (getblockstats, else coinbase minus the reward,
// else unknown).
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

	// light block first (BTC blocks have thousands of transactions)
	var b btcBlock
	if err := n.call("getblock", &b, hash, 1); err != nil {
		return nil, err
	}
	var full []btcTx
	if len(b.Tx) == 0 && (b.NTx > 0 || b.TxCount > 0) {
		// verbosity 1 without the tx list (Nexa): take the full block
		if err := n.call("getblock", &b, hash, 2); err != nil {
			return nil, err
		}
	}
	txs := make([]any, 0, len(b.Tx))
	for _, raw := range b.Tx {
		var id string
		if json.Unmarshal(raw, &id) != nil {
			var t btcTx
			_ = json.Unmarshal(raw, &t)
			full = append(full, t)
			id = t.Txid
		}
		txs = append(txs, id)
	}

	var coinbase *btcTx
	if len(full) > 0 {
		coinbase = &full[0] // the first transaction is the coinbase
	} else if len(txs) > 0 {
		coinbase = n.coinbase(txs[0].(string), hash)
	}

	var miner any = ""
	var cbOut int64 = -1
	if coinbase != nil {
		cbOut = 0
		for _, o := range coinbase.Vout {
			cbOut += int64(math.Round(o.Value * 1e8))
		}
		for _, o := range coinbase.Vout {
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
		"fees":             n.fees(hash, int64(b.Height), cbOut),
		"gasUsed":          float64(0),
		"gasLimit":         float64(0),
		// the count only: BTC blocks have thousands of txids, too much to
		// send in history and to keep on the server
		"transactions": []any{},
		"txCount":      float64(len(txs)),
		"uncles":       []any{},
	}, nil
}

// coinbase fetches the coinbase tx: with the block hash (works without
// txindex on modern nodes), without it (older forks), or nil.
func (n *btcNode) coinbase(txid, blockHash string) *btcTx {
	var t btcTx
	if ok, _ := n.try("rawtx-blockhash", "getrawtransaction", &t, txid, 1, blockHash); ok {
		return &t
	}
	if ok, _ := n.try("rawtx", "getrawtransaction", &t, txid, 1); ok {
		return &t
	}
	var b struct {
		Tx []btcTx `json:"tx"`
	}
	if ok, _ := n.try("block-verbose", "getblock", &b, blockHash, 2); ok && len(b.Tx) > 0 {
		return &b.Tx[0]
	}
	return nil
}

// fees: getblockstats totalfee when the node has it (BTC sats, BCH/Nexa
// coins), else coinbase outputs minus the reward schedule, else nil.
func (n *btcNode) fees(hash string, height, cbOut int64) any {
	var st struct {
		TotalFee json.Number `json:"totalfee"`
		Subsidy  json.Number `json:"subsidy"`
	}
	if ok, _ := n.try("blockstats", "getblockstats", &st, hash, []string{"totalfee", "subsidy"}); ok {
		f, err := st.TotalFee.Float64()
		if err == nil {
			if !strings.ContainsAny(string(st.Subsidy), ".eE") {
				f /= 1e8 // integers are satoshis (Bitcoin Core)
			}
			return f
		}
	}
	if cbOut < 0 || n.sub.Start <= 0 {
		return nil
	}
	if f := cbOut - n.sub.at(height); f > 0 {
		return float64(f) / 1e8
	}
	return float64(0)
}

func (n *btcNode) Pending() (float64, error) {
	var m struct {
		Size float64 `json:"size"`
	}
	if ok, err := n.try("mempoolinfo", "getmempoolinfo", &m); ok || err != nil {
		return m.Size, err
	}
	var pool []json.RawMessage
	if ok, err := n.try("rawmempool", "getrawmempool", &pool); ok || err != nil {
		return float64(len(pool)), err
	}
	return 0, nil
}

func (n *btcNode) Syncing() (any, error) {
	n.mu.Lock()
	if !n.syncing && time.Since(n.syncedAt) < syncRecheck {
		n.mu.Unlock()
		return false, nil
	}
	n.mu.Unlock()
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
		n.syncedAt = time.Now()
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
	ok, err := n.try("connectioncount", "getconnectioncount", &peers)
	if err != nil {
		return NodeStats{}, err
	}
	if !ok { // zebrad
		var ni struct {
			Connections float64 `json:"connections"`
		}
		if err := n.call("getnetworkinfo", &ni); err != nil {
			return NodeStats{}, err
		}
		peers = ni.Connections
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
