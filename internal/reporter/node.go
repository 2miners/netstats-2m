package reporter

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/2miners/netstats-2m/internal/rpc"
)

// Node is one live connection to the chain node. Errors from its calls make
// the reporter close it, mark the node down and dial again.
type Node interface {
	Done() <-chan struct{}
	Close()
	// Info returns the node name/version, network id and coinbase for "hello".
	Info() (node string, net, coinbase any, err error)
	// Block fetches a block by "latest", height (float64) or hash (string),
	// in the netstats block format.
	Block(ref any) (map[string]any, error)
	Pending() (float64, error)
	// Syncing is false or {startingBlock, currentBlock, highestBlock}.
	Syncing() (any, error)
	Stats() (NodeStats, error)
	// Watch reports new heads and pending transactions until the node is closed.
	Watch(log *slog.Logger, onHead func(ref any), onPending func())
}

type NodeStats struct {
	Peers    float64
	Mining   bool
	GasPrice any
	Syncing  any
}

// Dialer opens a Node.
type Dialer func(ctx context.Context) (Node, error)

// ---------------- Ethereum: WebSocket JSON-RPC with subscriptions ----------------

type ethNode struct{ c *rpc.Client }

func EthDialer(url string) Dialer {
	return func(ctx context.Context) (Node, error) {
		c, err := rpc.Dial(ctx, url)
		if err != nil {
			return nil, err
		}
		return &ethNode{c: c}, nil
	}
}

func (n *ethNode) Done() <-chan struct{} { return n.c.Done() }
func (n *ethNode) Close()                { n.c.Close() }

func (n *ethNode) Info() (string, any, any, error) {
	var node, netv string
	if err := n.c.Call("web3_clientVersion", &node); err != nil {
		return "", nil, nil, err
	}
	if err := n.c.Call("net_version", &netv); err != nil {
		return "", nil, nil, err
	}
	var coinbase any
	if err := n.c.Call("eth_coinbase", &coinbase); err != nil {
		coinbase = nil
	}
	var netID any
	if v, err := parseIntJS(netv); err == nil {
		netID = v
	}
	return node, netID, coinbase, nil
}

func (n *ethNode) Block(ref any) (map[string]any, error) {
	var raw json.RawMessage
	var err error
	switch v := ref.(type) {
	case string:
		if len(v) == 66 {
			err = n.c.Call("eth_getBlockByHash", &raw, v, false)
		} else {
			err = n.c.Call("eth_getBlockByNumber", &raw, v, false)
		}
	case float64:
		err = n.c.Call("eth_getBlockByNumber", &raw, rpc.BlockTag(v), false)
	default:
		err = n.c.Call("eth_getBlockByNumber", &raw, "latest", false)
	}
	if err != nil {
		return nil, err
	}
	return rpc.FormatBlock(raw), nil
}

func (n *ethNode) Pending() (float64, error) {
	var hex string
	if err := n.c.Call("eth_getBlockTransactionCountByNumber", &hex, "pending"); err != nil {
		return 0, err
	}
	p, _ := rpc.HexToFloat(hex)
	return p, nil
}

func (n *ethNode) Syncing() (any, error) {
	var raw json.RawMessage
	if err := n.c.Call("eth_syncing", &raw); err != nil {
		return nil, err
	}
	return rpc.FormatSyncing(raw), nil
}

func (n *ethNode) Stats() (NodeStats, error) {
	var peers, gasPrice, syncing json.RawMessage
	var mining bool
	var first error
	var mu sync.Mutex
	setErr := func(e error) {
		mu.Lock()
		if first == nil {
			first = e
		}
		mu.Unlock()
	}
	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); setErr(n.c.Call("net_peerCount", &peers)) }()
	go func() {
		defer wg.Done()
		// newer nodes drop eth_mining: an error means "not mining", not a dead node
		var m bool
		if n.c.Call("eth_mining", &m) == nil {
			mining = m
		}
	}()
	go func() { defer wg.Done(); setErr(n.c.Call("eth_gasPrice", &gasPrice)) }()
	go func() { defer wg.Done(); setErr(n.c.Call("eth_syncing", &syncing)) }()
	wg.Wait()
	if first != nil {
		return NodeStats{}, first
	}
	var peersHex, gasHex string
	_ = json.Unmarshal(peers, &peersHex)
	_ = json.Unmarshal(gasPrice, &gasHex)
	p, _ := rpc.HexToFloat(peersHex)
	g, _ := rpc.HexToDecimal(gasHex)
	return NodeStats{Peers: p, Mining: mining, GasPrice: g, Syncing: rpc.FormatSyncing(syncing)}, nil
}

func (n *ethNode) Watch(log *slog.Logger, onHead func(ref any), onPending func()) {
	err := n.c.Subscribe("newHeads", func(raw json.RawMessage) {
		var h struct {
			Hash *string `json:"hash"`
		}
		_ = json.Unmarshal(raw, &h)
		var ref any = "latest"
		if h.Hash != nil {
			ref = *h.Hash
		}
		onHead(ref)
	})
	if err != nil {
		log.Error("couldn't set up chain filter", "err", err)
	} else {
		log.Info("installed chain filter")
	}
	err = n.c.Subscribe("newPendingTransactions", func(json.RawMessage) { onPending() })
	if err != nil {
		log.Error("couldn't set up pending filter", "err", err)
	} else {
		log.Info("installed pending filter")
	}
}
