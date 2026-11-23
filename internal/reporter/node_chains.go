package reporter

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/2miners/netstats-2m/internal/rpc"
)

// ChainConfig selects and configures a polled node type (CHAIN=...).
type ChainConfig struct {
	Kind       string // monero, ergo, ckb, beam, grin, ae, substrate, quai, kaspa, neb
	URL        string // http://host:port
	User, Pass string
	OwnerPass  string // grin: owner API secret (peers, sync status)
	Name       string // node name shown on the page, e.g. "Zephyr"
	Poll       time.Duration
}

// ChainDialer returns the Dialer for a polled chain kind.
func ChainDialer(c ChainConfig) (Dialer, error) {
	if c.Poll <= 0 {
		c.Poll = time.Second
	}
	var mk func() chainAPI
	switch c.Kind {
	case "monero":
		mk = func() chainAPI { return &moneroAPI{c: rpc.NewJSON(c.URL, c.User, c.Pass), name: c.Name} }
	case "ergo":
		mk = func() chainAPI { return &ergoAPI{c: rpc.NewJSON(c.URL, "", "")} }
	case "ckb":
		mk = func() chainAPI { return &ckbAPI{c: rpc.NewJSON(c.URL, c.User, c.Pass)} }
	case "beam":
		mk = func() chainAPI { return &beamAPI{c: rpc.NewJSON(c.URL, "", "")} }
	case "grin":
		mk = func() chainAPI {
			return &grinAPI{c: rpc.NewJSON(c.URL, c.User, c.Pass), owner: rpc.NewJSON(c.URL, c.User, c.OwnerPass), name: c.Name, hasOwner: c.OwnerPass != ""}
		}
	case "ae":
		mk = func() chainAPI { return &aeAPI{c: rpc.NewJSON(c.URL, "", "")} }
	case "kaspa":
		addr := strings.TrimPrefix(c.URL, "http://")
		mk = func() chainAPI { return &kaspaAPI{addr: addr} }
	case "quai":
		mk = func() chainAPI { return &quaiAPI{c: rpc.NewJSON(c.URL, c.User, c.Pass)} }
	case "neb":
		mk = func() chainAPI { return &nebAPI{c: rpc.NewJSON(c.URL, "", "")} }
	case "substrate":
		mk = func() chainAPI { return &substrateAPI{c: rpc.NewJSON(c.URL, c.User, c.Pass), name: c.Name} }
	default:
		return nil, fmt.Errorf("unknown chain kind %q", c.Kind)
	}
	return pollDialer(c.Poll, mk), nil
}

func nodeName(name, version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	if version == "" {
		return name
	}
	return name + "/v" + version
}

// ---------------- monero family (Zephyr): json_rpc, digest auth ----------------

type moneroAPI struct {
	c    *rpc.JSONClient
	name string
}

func (a *moneroAPI) call(ctx context.Context, method string, params any) (map[string]any, error) {
	var r map[string]any
	if err := a.c.Call2(ctx, "/json_rpc", method, params, &r); err != nil {
		return nil, err
	}
	return r, nil
}

func (a *moneroAPI) info(ctx context.Context) (string, any, error) {
	r, err := a.call(ctx, "get_info", nil)
	if err != nil {
		return "", nil, err
	}
	name := a.name
	if name == "" {
		name = "Monero"
	}
	return nodeName(name, str(r["version"])), str(r["nettype"]), nil
}

func (a *moneroAPI) tip(ctx context.Context) (float64, string, error) {
	r, err := a.call(ctx, "get_last_block_header", nil)
	if err != nil {
		return 0, "", err
	}
	h := obj(r, "block_header")
	return num(obj(h, "height")), str(obj(h, "hash")), nil
}

func (a *moneroAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	r, err := a.call(ctx, "get_block_header_by_height", map[string]any{"height": int64(height)})
	if err != nil {
		return nil, err
	}
	h := obj(r, "block_header")
	total := num(obj(h, "wide_cumulative_difficulty"))
	if total == 0 {
		total = num(obj(h, "cumulative_difficulty"))
	}
	diff := num(obj(h, "wide_difficulty"))
	if diff == 0 {
		diff = num(obj(h, "difficulty"))
	}
	return newBlock(num(obj(h, "height")), str(obj(h, "hash")), str(obj(h, "prev_hash")), num(obj(h, "timestamp")), "",
		diff, total, num(obj(h, "num_txes")), num(obj(h, "block_size")), -1), nil
}

func (a *moneroAPI) peers(ctx context.Context) (float64, error) {
	r, err := a.call(ctx, "get_info", nil)
	if err != nil {
		return 0, err
	}
	return num(r["incoming_connections_count"]) + num(r["outgoing_connections_count"]), nil
}

func (a *moneroAPI) pending(ctx context.Context) (float64, error) {
	r, err := a.call(ctx, "get_info", nil)
	if err != nil {
		return 0, err
	}
	return num(r["tx_pool_size"]), nil
}

func (a *moneroAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	r, err := a.call(ctx, "get_info", nil)
	if err != nil {
		return 0, 0, false, err
	}
	cur, target := num(r["height"]), num(r["target_height"])
	synced, _ := r["synchronized"].(bool)
	if target < cur {
		target = cur
	}
	return cur, target, !synced && target-cur > 2, nil
}

// ---------------- Ergo: REST ----------------

type ergoAPI struct{ c *rpc.JSONClient }

func (a *ergoAPI) nodeInfo(ctx context.Context) (map[string]any, error) {
	var r map[string]any
	err := a.c.Get(ctx, "/info", &r)
	return r, err
}

func (a *ergoAPI) info(ctx context.Context) (string, any, error) {
	r, err := a.nodeInfo(ctx)
	if err != nil {
		return "", nil, err
	}
	return nodeName("Ergo", str(r["appVersion"])), str(r["network"]), nil
}

func (a *ergoAPI) tip(ctx context.Context) (float64, string, error) {
	r, err := a.nodeInfo(ctx)
	if err != nil {
		return 0, "", err
	}
	return num(r["fullHeight"]), str(r["bestFullHeaderId"]), nil
}

func (a *ergoAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	var ids []string
	if err := a.c.Get(ctx, "/blocks/at/"+strconv.FormatInt(int64(height), 10), &ids); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("ergo: no block at %v", height)
	}
	var b map[string]any
	if err := a.c.Get(ctx, "/blocks/"+ids[0], &b); err != nil {
		return nil, err
	}
	h := obj(b, "header")
	txs, _ := obj(b, "blockTransactions", "transactions").([]any)
	return newBlock(num(obj(h, "height")), str(obj(h, "id")), str(obj(h, "parentId")), num(obj(h, "timestamp"))/1000,
		str(obj(h, "powSolutions", "pk")), num(obj(h, "difficulty")), 0, float64(len(txs)), num(b["size"]), -1), nil
}

func (a *ergoAPI) peers(ctx context.Context) (float64, error) {
	r, err := a.nodeInfo(ctx)
	return num(r["peersCount"]), err
}

func (a *ergoAPI) pending(ctx context.Context) (float64, error) {
	r, err := a.nodeInfo(ctx)
	return num(r["unconfirmedCount"]), err
}

func (a *ergoAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	r, err := a.nodeInfo(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	cur, high := num(r["fullHeight"]), num(r["headersHeight"])
	if m := num(r["maxPeerHeight"]); m > high {
		high = m
	}
	return cur, high, high-cur > 2, nil
}

// ---------------- Nervos CKB: JSON-RPC 2.0 ----------------

type ckbAPI struct{ c *rpc.JSONClient }

var maxTarget256 = new(big.Int).Lsh(big.NewInt(1), 256)

func (a *ckbAPI) call(ctx context.Context, method string, params any, out any) error {
	return a.c.Call2(ctx, "/", method, params, out)
}

func (a *ckbAPI) info(ctx context.Context) (string, any, error) {
	var ni, bi map[string]any
	if err := a.call(ctx, "local_node_info", nil, &ni); err != nil {
		return "", nil, err
	}
	if err := a.call(ctx, "get_blockchain_info", nil, &bi); err != nil {
		return "", nil, err
	}
	v, _, _ := strings.Cut(str(ni["version"]), " ")
	return nodeName("CKB", v), str(bi["chain"]), nil
}

func (a *ckbAPI) tip(ctx context.Context) (float64, string, error) {
	var h map[string]any
	if err := a.call(ctx, "get_tip_header", nil, &h); err != nil {
		return 0, "", err
	}
	return num(h["number"]), str(h["hash"]), nil
}

func (a *ckbAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	var b map[string]any
	if err := a.call(ctx, "get_block_by_number", []any{"0x" + strconv.FormatInt(int64(height), 16)}, &b); err != nil {
		return nil, err
	}
	h, _ := obj(b, "header").(map[string]any)
	txs, _ := b["transactions"].([]any)
	miner := ""
	if len(txs) > 0 { // cellbase: the miner's lock args
		if outs, _ := obj(txs[0], "outputs").([]any); len(outs) > 0 {
			miner = str(obj(outs[0], "lock", "args"))
		}
	}
	ct, _ := strconv.ParseUint(strings.TrimPrefix(str(h["compact_target"]), "0x"), 16, 32)
	return newBlock(num(h["number"]), str(h["hash"]), str(h["parent_hash"]), num(h["timestamp"])/1000, miner,
		compactToDiff(uint32(ct), maxTarget256), 0, float64(len(txs)), 0, -1), nil
}

func (a *ckbAPI) peers(ctx context.Context) (float64, error) {
	var ni map[string]any
	err := a.call(ctx, "local_node_info", nil, &ni)
	return num(ni["connections"]), err
}

func (a *ckbAPI) pending(ctx context.Context) (float64, error) {
	var p map[string]any
	err := a.call(ctx, "tx_pool_info", nil, &p)
	return num(p["pending"]) + num(p["proposed"]), err
}

func (a *ckbAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	var bi map[string]any
	if err := a.call(ctx, "get_blockchain_info", nil, &bi); err != nil {
		return 0, 0, false, err
	}
	cur, _, err := a.tip(ctx)
	ibd, _ := bi["is_initial_block_download"].(bool)
	return cur, cur, ibd, err
}

// ---------------- Beam: explorer-node HTTP API ----------------

type beamAPI struct{ c *rpc.JSONClient }

func (a *beamAPI) status(ctx context.Context) (map[string]any, error) {
	var r map[string]any
	err := a.c.Get(ctx, "/status", &r)
	return r, err
}

func (a *beamAPI) info(ctx context.Context) (string, any, error) {
	return "Beam", "mainnet", nil
}

func (a *beamAPI) tip(ctx context.Context) (float64, string, error) {
	r, err := a.status(ctx)
	if err != nil {
		return 0, "", err
	}
	return num(r["height"]), str(r["hash"]), nil
}

func (a *beamAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	var b map[string]any
	if err := a.c.Get(ctx, "/block?height="+strconv.FormatInt(int64(height), 10), &b); err != nil {
		return nil, err
	}
	kernels, _ := b["kernels"].([]any)
	fees := 0.0
	for _, k := range kernels {
		fees += num(obj(k, "fee"))
	}
	txs := float64(len(kernels))
	if txs > 0 {
		txs-- // the coinbase kernel
	}
	return newBlock(num(b["height"]), str(b["hash"]), str(b["prev"]), num(b["timestamp"]), "",
		num(b["difficulty"]), num(b["chainwork"]), txs, 0, fees/1e8), nil
}

func (a *beamAPI) peers(ctx context.Context) (float64, error) {
	r, err := a.status(ctx)
	return num(r["peers_count"]), err
}

func (a *beamAPI) pending(ctx context.Context) (float64, error) { return 0, nil }

func (a *beamAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	h, _, err := a.tip(ctx)
	return h, h, false, err
}

// ---------------- Grin / MWC: node API v2 ----------------

type grinAPI struct {
	c, owner *rpc.JSONClient
	name     string
	hasOwner bool
}

func (a *grinAPI) foreign(ctx context.Context, method string, params any) (any, error) {
	var r map[string]any
	if err := a.c.Call2(ctx, "/v2/foreign", method, params, &r); err != nil {
		return nil, err
	}
	if e, ok := r["Err"]; ok {
		return nil, fmt.Errorf("%s: %v", method, e)
	}
	return r["Ok"], nil
}

func (a *grinAPI) status(ctx context.Context) (map[string]any, error) {
	if !a.hasOwner {
		return nil, nil
	}
	var r map[string]any
	if err := a.owner.Call2(ctx, "/v2/owner", "get_status", nil, &r); err != nil {
		return nil, err
	}
	s, _ := r["Ok"].(map[string]any)
	return s, nil
}

func (a *grinAPI) info(ctx context.Context) (string, any, error) {
	v, err := a.foreign(ctx, "get_version", nil)
	if err != nil {
		return "", nil, err
	}
	return nodeName(a.name, str(obj(v, "node_version"))), "mainnet", nil
}

func (a *grinAPI) tip(ctx context.Context) (float64, string, error) {
	t, err := a.foreign(ctx, "get_tip", nil)
	if err != nil {
		return 0, "", err
	}
	return num(obj(t, "height")), str(obj(t, "last_block_pushed")), nil
}

func (a *grinAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	b, err := a.foreign(ctx, "get_block", []any{int64(height), nil, nil})
	if err != nil {
		return nil, err
	}
	h := obj(b, "header")
	total := num(obj(h, "total_difficulty"))
	diff := 0.0
	if height > 0 { // per-block difficulty = total difficulty delta
		if p, err := a.foreign(ctx, "get_header", []any{int64(height) - 1, nil, nil}); err == nil {
			diff = total - num(obj(p, "total_difficulty"))
		}
	}
	ts := 0.0
	if t, err := time.Parse(time.RFC3339, str(obj(h, "timestamp"))); err == nil {
		ts = float64(t.Unix())
	}
	kernels, _ := obj(b, "kernels").([]any)
	fees, txs := 0.0, 0.0
	for _, k := range kernels {
		if str(obj(k, "features")) == "Coinbase" {
			continue
		}
		txs++
		fees += num(obj(k, "fee"))
	}
	return newBlock(num(obj(h, "height")), str(obj(h, "hash")), str(obj(h, "previous")), ts, "",
		diff, total, txs, 0, fees/1e9), nil
}

func (a *grinAPI) peers(ctx context.Context) (float64, error) {
	s, err := a.status(ctx)
	return num(obj(s, "connections")), err
}

func (a *grinAPI) pending(ctx context.Context) (float64, error) {
	v, err := a.foreign(ctx, "get_pool_size", nil)
	if err != nil {
		return 0, nil // older nodes: unknown, not an error
	}
	return num(v), nil
}

func (a *grinAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	cur, _, err := a.tip(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	s, err := a.status(ctx)
	if err != nil || s == nil {
		return cur, cur, false, err
	}
	st := str(s["sync_status"])
	high := num(obj(s, "sync_info", "highest_height"))
	if high < cur {
		high = cur
	}
	return cur, high, st != "" && st != "no_sync", nil
}

// ---------------- Aeternity: HTTP API ----------------

type aeAPI struct{ c *rpc.JSONClient }

// Aeternity's highest target 0x2100ffff (0xffff << 240).
var aeMaxTarget = new(big.Int).Lsh(big.NewInt(0xffff), 240)

func (a *aeAPI) status(ctx context.Context) (map[string]any, error) {
	var r map[string]any
	err := a.c.Get(ctx, "/v3/status", &r)
	return r, err
}

func (a *aeAPI) info(ctx context.Context) (string, any, error) {
	r, err := a.status(ctx)
	if err != nil {
		return "", nil, err
	}
	return nodeName("Aeternity", str(r["node_version"])), str(r["network_id"]), nil
}

func (a *aeAPI) tip(ctx context.Context) (float64, string, error) {
	var b map[string]any
	if err := a.c.Get(ctx, "/v3/key-blocks/current", &b); err != nil {
		return 0, "", err
	}
	return num(b["height"]), str(b["hash"]), nil
}

func (a *aeAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	hs := strconv.FormatInt(int64(height), 10)
	var b map[string]any
	if err := a.c.Get(ctx, "/v3/key-blocks/height/"+hs, &b); err != nil {
		return nil, err
	}
	// transactions of the generation = in its micro blocks
	txs := 0.0
	var g map[string]any
	if err := a.c.Get(ctx, "/v3/generations/height/"+hs, &g); err == nil {
		mbs, _ := g["micro_blocks"].([]any)
		for _, mb := range mbs {
			var cnt map[string]any
			if a.c.Get(ctx, "/v3/micro-blocks/hash/"+str(mb)+"/transactions/count", &cnt) == nil {
				txs += num(cnt["count"])
			}
		}
	}
	return newBlock(num(b["height"]), str(b["hash"]), str(b["prev_key_hash"]), num(b["time"])/1000, str(b["beneficiary"]),
		compactToDiff(uint32(num(b["target"])), aeMaxTarget), 0, txs, 0, -1), nil
}

func (a *aeAPI) peers(ctx context.Context) (float64, error) {
	r, err := a.status(ctx)
	return num(r["peer_count"]), err
}

func (a *aeAPI) pending(ctx context.Context) (float64, error) {
	r, err := a.status(ctx)
	return num(r["pending_transactions_count"]), err
}

func (a *aeAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	r, err := a.status(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	syncing, _ := r["syncing"].(bool)
	h := num(r["top_block_height"])
	return h, h, syncing, nil
}

// ---------------- Substrate (Quantus QTC): JSON-RPC 2.0 + storage ----------------

type substrateAPI struct {
	c    *rpc.JSONClient
	name string
}

const (
	// twox128("QPoW") ++ twox128("CurrentDifficulty"), twox128("Timestamp") ++ twox128("Now")
	keyQPoWDifficulty = "0xf70798a3fbcee5a2d178b8c516e36ae559769e16105568cee9f916e870cb6e6f"
	keyTimestampNow   = "0xf0c365c3cf59d671eb72da0e7a4113c49f1f0515f462cdcf84e0f1d6045dfcbb"
)

func (a *substrateAPI) call(ctx context.Context, method string, params any, out any) error {
	return a.c.Call2(ctx, "/", method, params, out)
}

func (a *substrateAPI) info(ctx context.Context) (string, any, error) {
	var v, chain string
	if err := a.call(ctx, "system_version", nil, &v); err != nil {
		return "", nil, err
	}
	_ = a.call(ctx, "system_chain", nil, &chain)
	name := a.name
	if name == "" {
		name = "Substrate"
	}
	return nodeName(name, v), chain, nil
}

func (a *substrateAPI) tip(ctx context.Context) (float64, string, error) {
	var hash string
	if err := a.call(ctx, "chain_getBlockHash", nil, &hash); err != nil {
		return 0, "", err
	}
	var h map[string]any
	if err := a.call(ctx, "chain_getHeader", []any{hash}, &h); err != nil {
		return 0, "", err
	}
	return num(h["number"]), hash, nil
}

// storageLE reads a little-endian unsigned storage value at a block.
func (a *substrateAPI) storageLE(ctx context.Context, key, at string) float64 {
	var v *string
	if a.call(ctx, "state_getStorage", []any{key, at}, &v) != nil || v == nil {
		return 0
	}
	b, err := hex.DecodeString(strings.TrimPrefix(*v, "0x"))
	if err != nil {
		return 0
	}
	if len(b) == 8 {
		return float64(binary.LittleEndian.Uint64(b))
	}
	for i, j := 0, len(b)-1; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	f, _ := new(big.Float).SetInt(new(big.Int).SetBytes(b)).Float64()
	return f
}

func (a *substrateAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	var hash string
	if err := a.call(ctx, "chain_getBlockHash", []any{int64(height)}, &hash); err != nil {
		return nil, err
	}
	var sb map[string]any
	if err := a.call(ctx, "chain_getBlock", []any{hash}, &sb); err != nil {
		return nil, err
	}
	h := obj(sb, "block", "header")
	ext, _ := obj(sb, "block", "extrinsics").([]any)
	txs := float64(len(ext))
	if txs > 0 {
		txs-- // the timestamp inherent
	}
	return newBlock(num(obj(h, "number")), hash, str(obj(h, "parentHash")), a.storageLE(ctx, keyTimestampNow, hash)/1000, "",
		a.storageLE(ctx, keyQPoWDifficulty, hash), 0, txs, 0, -1), nil
}

func (a *substrateAPI) health(ctx context.Context) (map[string]any, error) {
	var r map[string]any
	err := a.call(ctx, "system_health", nil, &r)
	return r, err
}

func (a *substrateAPI) peers(ctx context.Context) (float64, error) {
	r, err := a.health(ctx)
	return num(r["peers"]), err
}

func (a *substrateAPI) pending(ctx context.Context) (float64, error) {
	var p []any
	err := a.call(ctx, "author_pendingExtrinsics", nil, &p)
	return float64(len(p)), err
}

func (a *substrateAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	r, err := a.health(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	var s map[string]any
	_ = a.call(ctx, "system_syncState", nil, &s)
	cur, high := num(s["currentBlock"]), num(s["highestBlock"])
	if high < cur {
		high = cur
	}
	syncing, _ := r["isSyncing"].(bool)
	return cur, high, syncing, nil
}

// ---------------- Quai: go-quai zone chain JSON-RPC ----------------

// quaiAPI follows the zone chain (the KawPoW blocks); every block carries
// "algos": difficulty, hashrate and share time of KawPoW, SHA256 and Scrypt
// from quai_getMiningInfo, shown as one card per algorithm on the page.
type quaiAPI struct {
	c *rpc.JSONClient

	mu     sync.Mutex
	tipH   float64
	algos  []any
	algoAt time.Time
}

func (a *quaiAPI) call(ctx context.Context, method string, params any, out any) error {
	return a.c.Call2(ctx, "/", method, params, out)
}

func (a *quaiAPI) getBlock(ctx context.Context, ref string) (map[string]any, error) {
	var b map[string]any
	if err := a.call(ctx, "quai_getBlockByNumber", []any{ref, false}, &b); err != nil {
		return nil, err
	}
	if b == nil {
		return nil, fmt.Errorf("quai: no block %s", ref)
	}
	return b, nil
}

func (a *quaiAPI) info(ctx context.Context) (string, any, error) {
	// go-quai has no client-version call (extraData is the block producer's
	// version, not ours), so the name only
	var netv string
	if err := a.call(ctx, "net_version", nil, &netv); err != nil {
		return "", nil, err
	}
	return "go-quai", netv, nil
}

func (a *quaiAPI) tip(ctx context.Context) (float64, string, error) {
	b, err := a.getBlock(ctx, "latest")
	if err != nil {
		return 0, "", err
	}
	h := num(obj(b, "woHeader", "number"))
	a.mu.Lock()
	a.tipH = h
	a.mu.Unlock()
	return h, str(b["hash"]), nil
}

// miningAlgos is quai_getMiningInfo (it analyses the last ~176 blocks), cached
// for 10 s and asked only for the tip: history blocks don't need it.
func (a *quaiAPI) miningAlgos(ctx context.Context) []any {
	a.mu.Lock()
	if a.algos != nil && time.Since(a.algoAt) < 10*time.Second {
		v := a.algos
		a.mu.Unlock()
		return v
	}
	a.mu.Unlock()
	var mi map[string]any
	if a.call(ctx, "quai_getMiningInfo", nil, &mi) != nil {
		return nil
	}
	algo := func(name, diff, hr, share string) map[string]any {
		return map[string]any{"name": name, "difficulty": num(mi[diff]), "hashrate": num(mi[hr]), "shareTime": num(mi[share])}
	}
	v := []any{
		algo("KawPoW", "kawpowDifficulty", "kawpowHashRate", "avgKawpowShareTime"),
		algo("SHA256", "shaDifficulty", "shaHashRate", "avgShaShareTime"),
		algo("Scrypt", "scryptDifficulty", "scryptHashRate", "avgScryptShareTime"),
	}
	a.mu.Lock()
	a.algos, a.algoAt = v, time.Now()
	a.mu.Unlock()
	return v
}

func (a *quaiAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	b, err := a.getBlock(ctx, "0x"+strconv.FormatInt(int64(height), 16))
	if err != nil {
		return nil, err
	}
	wo := obj(b, "woHeader")
	txs, _ := b["transactions"].([]any)
	out := newBlock(num(obj(wo, "number")), str(b["hash"]), str(obj(wo, "parentHash")), num(obj(wo, "timestamp")),
		str(obj(wo, "primaryCoinbase")), num(obj(wo, "difficulty")), 0, float64(len(txs)),
		num(obj(b, "header", "size")), num(obj(b, "header", "totalFees"))/1e18)
	a.mu.Lock()
	isTip := height >= a.tipH
	a.mu.Unlock()
	if isTip {
		if v := a.miningAlgos(ctx); v != nil {
			out["algos"] = v
		}
	}
	return out, nil
}

func (a *quaiAPI) peers(ctx context.Context) (float64, error) {
	var p string
	err := a.call(ctx, "net_peerCount", nil, &p)
	return num(p), err
}

func (a *quaiAPI) pending(ctx context.Context) (float64, error) {
	var st map[string]any
	if err := a.call(ctx, "txpool_status", nil, &st); err != nil {
		return 0, err
	}
	return num(st["pending"]) + num(st["queued"]) + num(st["qi"]), nil
}

func (a *quaiAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	h, _, err := a.tip(ctx)
	return h, h, false, err
}
