package reporter

import (
	"context"
	"errors"
	"sync"

	"github.com/2miners/netstats-2m/internal/kaspa"
)

// kaspaAPI samples the DAG (10 blocks a second) every poll: the reported
// block is the sink (virtual selected parent), numbered by its DAA score.
// History by height does not exist on a DAG, so only samples are sent;
// "blockTime" carries the real average block time between two samples and
// "algos" the node's hashrate estimate.
type kaspaAPI struct {
	addr string

	mu       sync.Mutex
	c        *kaspa.Client
	lastHash string
	lastH    float64
	prevTs   int64
	prevDaa  uint64
	blockT   float64
}

var errKaspaNoHistory = errors.New("kaspa: no blocks by height on a DAG")

func (a *kaspaAPI) client(ctx context.Context) (*kaspa.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.c != nil {
		select {
		case <-a.c.Done():
			a.c = nil
		default:
			return a.c, nil
		}
	}
	c, err := kaspa.Dial(ctx, a.addr)
	if err != nil {
		return nil, err
	}
	a.c = c
	return c, nil
}

func (a *kaspaAPI) info(ctx context.Context) (string, any, error) {
	c, err := a.client(ctx)
	if err != nil {
		return "", nil, err
	}
	in, err := c.Info(ctx)
	if err != nil {
		return "", nil, err
	}
	d, err := c.DagInfo(ctx)
	if err != nil {
		return "", nil, err
	}
	return nodeName("kaspad", in.Version), d.Network, nil
}

func (a *kaspaAPI) sink(ctx context.Context, c *kaspa.Client) (kaspa.DagInfo, string, error) {
	d, err := c.DagInfo(ctx)
	if err != nil {
		return d, "", err
	}
	h := d.Sink
	if h == "" && len(d.Tips) > 0 {
		h = d.Tips[0]
	}
	if h == "" {
		return d, "", errors.New("kaspa: no sink")
	}
	return d, h, nil
}

func (a *kaspaAPI) tip(ctx context.Context) (float64, string, error) {
	c, err := a.client(ctx)
	if err != nil {
		return 0, "", err
	}
	_, h, err := a.sink(ctx, c)
	if err != nil {
		return 0, "", err
	}
	b, err := c.Block(ctx, h)
	if err != nil {
		return 0, "", err
	}
	a.mu.Lock()
	a.lastHash, a.lastH = h, float64(b.DaaScore)
	a.mu.Unlock()
	return float64(b.DaaScore), h, nil
}

func (a *kaspaAPI) block(ctx context.Context, height float64) (map[string]any, error) {
	a.mu.Lock()
	hash, h := a.lastHash, a.lastH
	a.mu.Unlock()
	if hash == "" || height != h {
		return nil, errKaspaNoHistory
	}
	c, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	d, err := c.DagInfo(ctx)
	if err != nil {
		return nil, err
	}
	b, err := c.Block(ctx, hash)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.prevDaa > 0 && b.DaaScore > a.prevDaa && b.Timestamp > a.prevTs {
		a.blockT = float64(b.Timestamp-a.prevTs) / 1000 / float64(b.DaaScore-a.prevDaa)
	}
	a.prevTs, a.prevDaa = b.Timestamp, b.DaaScore
	bt := a.blockT
	a.mu.Unlock()

	out := newBlock(float64(b.DaaScore), b.Hash, b.SelectedParent, float64(b.Timestamp)/1000, "",
		b.Difficulty, num("0x"+b.BlueWork), float64(b.TxCount), 0, -1)
	if bt > 0 {
		out["blockTime"] = bt
	}
	algo := map[string]any{"name": "kHeavyHash", "difficulty": d.Difficulty, "shareTime": bt, "every": "block every"}
	if hr, err := c.Hashrate(ctx, 1000); err == nil {
		algo["hashrate"] = float64(hr)
	}
	out["algos"] = []any{algo}
	return out, nil
}

func (a *kaspaAPI) peers(ctx context.Context) (float64, error) {
	c, err := a.client(ctx)
	if err != nil {
		return 0, err
	}
	n, err := c.Peers(ctx)
	return float64(n), err
}

func (a *kaspaAPI) pending(ctx context.Context) (float64, error) {
	c, err := a.client(ctx)
	if err != nil {
		return 0, err
	}
	in, err := c.Info(ctx)
	return float64(in.Mempool), err
}

func (a *kaspaAPI) sync(ctx context.Context) (float64, float64, bool, error) {
	c, err := a.client(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	in, err := c.Info(ctx)
	if err != nil {
		return 0, 0, false, err
	}
	a.mu.Lock()
	h := a.lastH
	a.mu.Unlock()
	return h, h, !in.Synced, nil
}
