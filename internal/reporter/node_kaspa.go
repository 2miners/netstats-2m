package reporter

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/2miners/netstats-2m/internal/kaspa"
)

// kaspaAPI samples the DAG (10 blocks a second) every poll: the reported
// block is the sink (virtual selected parent), numbered by its DAA score.
// History by height does not exist on a DAG, so only samples are sent;
// "blockTime" carries the real average block time over the samples of the
// last kaspaTimeWindow and "algos" the node's hashrate estimate.
type kaspaAPI struct {
	addr string

	mu       sync.Mutex
	c        *kaspa.Client
	lastHash string
	lastH    float64
	diff     float64 // network difficulty from the last tip poll
	samples  []kaspaSample
	hr       float64
	hrAt     time.Time
}

type kaspaSample struct {
	ts  int64 // block timestamp, ms
	daa uint64
}

const (
	// average block time over this window: a 1 s poll sees only ~10 blocks,
	// too few for a steady number
	kaspaTimeWindow = 30 * time.Second
	// EstimateNetworkHashesPerSecond walks 1000 blocks: once per this period
	kaspaHashrateEvery = 10 * time.Second
)

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
	d, h, err := a.sink(ctx, c)
	if err != nil {
		return 0, "", err
	}
	a.mu.Lock()
	same := h == a.lastHash
	a.diff = d.Difficulty
	lastH := a.lastH
	a.mu.Unlock()
	if same { // nothing new: skip GetBlock
		return lastH, h, nil
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
	b, err := c.Block(ctx, hash)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	bt := a.addSample(b.Timestamp, b.DaaScore)
	diff := a.diff
	hr, fresh := a.hr, time.Since(a.hrAt) < kaspaHashrateEvery
	a.mu.Unlock()
	if !fresh {
		if v, err := c.Hashrate(ctx, 1000); err == nil {
			hr = float64(v)
			a.mu.Lock()
			a.hr, a.hrAt = hr, time.Now()
			a.mu.Unlock()
		}
	}

	out := newBlock(float64(b.DaaScore), b.Hash, b.SelectedParent, float64(b.Timestamp)/1000, "",
		b.Difficulty, num("0x"+b.BlueWork), float64(b.TxCount), 0, -1)
	if bt > 0 {
		out["blockTime"] = bt
	}
	algo := map[string]any{"name": "kHeavyHash", "difficulty": diff, "shareTime": bt, "every": "block every"}
	if hr > 0 {
		algo["hashrate"] = hr
	}
	out["algos"] = []any{algo}
	return out, nil
}

// addSample records a sampled block and returns the average block time over
// kaspaTimeWindow (0 until two samples). Caller holds a.mu.
func (a *kaspaAPI) addSample(ts int64, daa uint64) float64 {
	n := len(a.samples)
	switch {
	case n > 0 && daa == a.samples[n-1].daa: // the same sample again
	case n > 0 && (daa < a.samples[n-1].daa || ts < a.samples[n-1].ts):
		a.samples = append(a.samples[:0], kaspaSample{ts, daa}) // sink went back: start over
	default:
		a.samples = append(a.samples, kaspaSample{ts, daa})
	}
	cut := ts - kaspaTimeWindow.Milliseconds()
	i := 0
	for i < len(a.samples)-2 && a.samples[i].ts < cut {
		i++
	}
	a.samples = a.samples[i:]
	first, last := a.samples[0], a.samples[len(a.samples)-1]
	if last.daa <= first.daa || last.ts <= first.ts {
		return 0
	}
	return float64(last.ts-first.ts) / 1000 / float64(last.daa-first.daa)
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
