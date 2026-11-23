package reporter

// Neural Engine Byte (NEB): neb-node JSON-RPC (spec/node.md section 7.2 of
// https://github.com/SilasEigen/neb), loopback, no authentication.

import (
	"context"
	"math"
	"math/big"

	"github.com/2miners/netstats-2m/internal/rpc"
)

// nebSubsidy is the block reward in base units (20 NEB, 1 NEB = 1e8): the
// coinbase pays it plus the fees, split over the miner and the project output.
const (
	nebSubsidy = 20e8
	nebCoin    = 1e8
)

var nebDiff1 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

type nebAPI struct{ c *rpc.JSONClient }

func (a *nebAPI) call(ctx context.Context, method string, params any, out any) error {
	if params == nil {
		params = []any{}
	}
	return a.c.Call2(ctx, "/", method, params, out)
}

func (a *nebAPI) getinfo(ctx context.Context) (map[string]any, error) {
	var r map[string]any
	err := a.call(ctx, "getinfo", nil, &r)
	return r, err
}

func (a *nebAPI) info(ctx context.Context) (string, any, error) {
	r, err := a.getinfo(ctx)
	if err != nil {
		return "", nil, err
	}
	return nodeName("neb-node", str(r["version"])), str(r["network"]), nil
}

func (a *nebAPI) tip(ctx context.Context) (float64, string, error) {
	r, err := a.getinfo(ctx)
	if err != nil {
		return 0, "", err
	}
	return num(r["blocks"]), str(r["bestblockhash"]), nil
}

// nebDifficulty is the block difficulty in attempts, (2^256-1) / target, the
// target being the compact `mantissa << shift` (spec/node.md section 4.1), the
// same scale as the pool's.
func nebDifficulty(bits uint32) float64 {
	shift, mant := bits>>24, bits&0xFFFFFF
	if shift > 232 || mant < 0x800000 {
		return 0
	}
	t := new(big.Int).Lsh(big.NewInt(int64(mant)), uint(shift))
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(nebDiff1), new(big.Float).SetInt(t)).Float64()
	if math.IsInf(f, 0) {
		return 0
	}
	return f
}

func (a *nebAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	var hash string
	if err := a.call(ctx, "getblockhash", []any{int64(height)}, &hash); err != nil {
		return nil, err
	}
	var b map[string]any
	if err := a.call(ctx, "getblock", []any{hash, true}, &b); err != nil {
		return nil, err
	}
	txs, _ := b["tx"].([]any)
	miner, fees := "", -1.0
	if len(txs) > 0 { // coinbase: miner output first, then the project output
		if cb, _ := txs[0].(map[string]any); cb != nil {
			if cbFlag, _ := cb["coinbase"].(bool); cbFlag {
				outs, _ := cb["outputs"].([]any)
				var paid float64
				for i, o := range outs {
					if i == 0 {
						miner = str(obj(o, "lock"))
					}
					paid += num(obj(o, "amount"))
				}
				if paid >= nebSubsidy {
					fees = (paid - nebSubsidy) / nebCoin
				}
			}
		}
	}
	return newBlock(num(b["height"]), str(b["hash"]), str(b["parent"]), num(b["time"]), miner,
		nebDifficulty(uint32(num(b["bits"]))), 0, float64(len(txs)), num(b["size"]), fees), nil
}

func (a *nebAPI) peers(ctx context.Context) (float64, error) {
	r, err := a.getinfo(ctx)
	return num(r["connections"]), err
}

func (a *nebAPI) pending(ctx context.Context) (float64, error) {
	var txs []any
	err := a.call(ctx, "getrawmempool", nil, &txs)
	return float64(len(txs)), err
}

// sync: the node has no sync call; compare its tip with the highest peer.
func (a *nebAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	cur, _, err := a.tip(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	var peers []map[string]any
	if err := a.call(ctx, "getpeerinfo", nil, &peers); err != nil {
		return 0, 0, false, err
	}
	high := cur
	for _, p := range peers {
		high = max(high, num(p["height"]))
	}
	return cur, high, high-cur > 2, nil
}
