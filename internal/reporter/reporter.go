// Package reporter is the Go port of the netstats client (lib/node.js): it
// follows one node over WebSocket JSON-RPC and reports to the netstats server.
package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/2miners/netstats-2m/internal/primus"
	"github.com/2miners/netstats-2m/internal/rpc"
)

const (
	maxBlocksHistory   = 40
	updateInterval     = 5 * time.Second
	pingInterval       = 3 * time.Second
	maxHistoryUpdate   = 50
	connectionRetry    = 5 * time.Second
	serverRetry        = 5 * time.Second
	serverRefusedRetry = time.Minute
)

// Config is the env of the JS client.
type Config struct {
	ID       string // camelCase(INSTANCE_NAME)
	Info     map[string]any
	RPCURL   string // ws://RPC_HOST:RPC_PORT
	Server   string // WS_SERVER
	Secret   string // WS_SECRET
	Log      *slog.Logger
}

type stats struct {
	Active   bool    `json:"active"`
	Mining   bool    `json:"mining"`
	Peers    float64 `json:"peers"`
	Pending  float64 `json:"pending"`
	GasPrice any     `json:"gasPrice"`
	Block    any     `json:"block"`
	Syncing  any     `json:"syncing"`
	Uptime   float64 `json:"uptime"`
}

// Reporter holds the state of lib/node.js.
type Reporter struct {
	cfg Config
	log *slog.Logger

	mu          sync.Mutex
	st          stats
	lastBlock   float64
	lastStats   string
	lastPending float64
	tries, down float64

	client   *rpc.Client // current RPC connection (nil when down)
	rpcUp    bool        // _web3 in JS: connected and initialised
	server   *primus.Conn
	socketUp bool

	queue    *blockQueue
	serverOnce sync.Once
	statsBusy  bool // a getStats round is in flight
	ctx        context.Context
	chain    chainDebounce
	lastPendingLog time.Time
	pendingTimer   *time.Timer
}

func New(cfg Config) *Reporter {
	r := &Reporter{cfg: cfg, log: cfg.Log}
	r.st = stats{GasPrice: float64(0), Syncing: false, Block: map[string]any{
		"number": float64(0), "hash": "?", "difficulty": float64(0), "totalDifficulty": float64(0),
		"transactions": []any{}, "uncles": []any{},
	}}
	r.lastStats = r.statsJSON()
	r.chain = chainDebounce{minTime: 50 * time.Millisecond, maxDebouncer: 20}
	r.queue = newBlockQueue(r.fetchQueuedBlock, r.getPending)
	return r
}

func (r *Reporter) statsJSON() string {
	b, _ := json.Marshal(r.st)
	return string(b)
}

// Run blocks until ctx is cancelled.
func (r *Reporter) Run(ctx context.Context) {
	r.ctx = ctx
	go r.timers(ctx)
	r.rpcLoop(ctx)
}

// ---------------- node RPC ----------------

func (r *Reporter) rpcLoop(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		r.log.Info("starting RPC connection", "url", r.cfg.RPCURL)
		c, err := rpc.Dial(ctx, r.cfg.RPCURL)
		if err != nil {
			r.log.Warn("RPC connection attempt failed", "attempt", attempt, "err", err, "retry_in", connectionRetry)
			attempt++
			sleep(ctx, connectionRetry)
			continue
		}
		attempt = 0
		r.log.Info("RPC connection established")
		r.mu.Lock()
		r.client = c
		r.rpcUp = true
		r.mu.Unlock()

		r.init(c)

		select {
		case <-ctx.Done():
			c.Close()
			return
		case <-c.Done():
		}

		r.mu.Lock()
		wasUp := r.rpcUp
		r.client = nil
		r.rpcUp = false
		r.mu.Unlock()
		if wasUp {
			r.log.Warn("RPC connection lost")
			r.setInactive()
		}
	}
}

// fail drops the current RPC connection (rpcLoop marks the node inactive and reconnects).
func (r *Reporter) fail(c *rpc.Client, err error) {
	r.log.Warn("RPC error, reconnecting", "err", err)
	c.Close()
}

func (r *Reporter) init(c *rpc.Client) {
	r.getInfo(c)
	// like the JS client: the server connection starts after the first node connection
	r.serverOnce.Do(func() { go r.serverLoop(r.ctx) })
	// syncing check, then subscriptions (once per connection)
	go func() {
		var raw json.RawMessage
		if err := c.Call("eth_syncing", &raw); err == nil {
			sync := rpc.FormatSyncing(raw)
			if m, ok := sync.(map[string]any); ok {
				r.mu.Lock()
				setProgress(m)
				r.st.Syncing = m
				cur, _ := m["currentBlock"].(float64)
				push := r.lastBlock != cur
				r.mu.Unlock()
				if push {
					r.queue.push(cur)
				}
				r.log.Info("SYNC UPDATE", "sync", m)
			} else {
				r.mu.Lock()
				r.st.Syncing = false
				r.mu.Unlock()
			}
		}
		r.setFilters(c)
	}()
	r.mu.Lock()
	up := r.socketUp
	r.mu.Unlock()
	if up {
		go r.getStats(true)
	}
}

func (r *Reporter) getInfo(c *rpc.Client) {
	var node, net string
	if err := c.Call("web3_clientVersion", &node); err != nil {
		r.log.Error("couldn't get version", "err", err)
		return
	}
	if err := c.Call("net_version", &net); err != nil {
		r.log.Error("couldn't get version", "err", err)
		return
	}
	var coinbase any
	if err := c.Call("eth_coinbase", &coinbase); err != nil {
		coinbase = nil
	}
	r.mu.Lock()
	r.cfg.Info["node"] = node
	if n, err := parseIntJS(net); err == nil {
		r.cfg.Info["net"] = n
	} else {
		r.cfg.Info["net"] = nil
	}
	r.cfg.Info["api"] = "1.0.0"
	r.cfg.Info["coinbase"] = coinbase
	r.mu.Unlock()
}

func (r *Reporter) setFilters(c *rpc.Client) {
	err := c.Subscribe("newHeads", func(raw json.RawMessage) {
		var h struct {
			Hash *string `json:"hash"`
		}
		_ = json.Unmarshal(raw, &h)
		var ref any = "latest"
		if h.Hash != nil {
			ref = *h.Hash
		}
		r.onChainHead(ref)
	})
	if err != nil {
		r.log.Error("couldn't set up chain filter", "err", err)
	} else {
		r.log.Info("installed chain filter")
	}
	err = c.Subscribe("newPendingTransactions", func(json.RawMessage) { r.onPendingTx() })
	if err != nil {
		r.log.Error("couldn't set up pending filter", "err", err)
	} else {
		r.log.Info("installed pending filter")
	}
}

// chainDebounce is the chain-filter throttling of the JS client.
type chainDebounce struct {
	lastLog       time.Time
	lastBlockSent time.Time
	debouncer     int
	debouncerCnt  int
	minTime       time.Duration
	maxDebouncer  int
	timer         *time.Timer
}

func (r *Reporter) onChainHead(ref any) {
	r.mu.Lock()
	d := &r.chain
	now := time.Now()
	since := now.Sub(d.lastLog)
	d.lastLog = now
	if since < d.minTime {
		d.debouncer++
		d.debouncerCnt++
		if d.debouncerCnt > 100 {
			d.minTime = time.Duration(math.Max(float64(d.minTime+time.Millisecond), float64(200*time.Millisecond)))
			if d.maxDebouncer-1 > 5 {
				d.maxDebouncer--
			} else {
				d.maxDebouncer = 5
			}
		}
	} else {
		if since > 5*time.Second {
			d.minTime = 50 * time.Millisecond
			d.maxDebouncer = 20
			d.debouncerCnt = 0
		}
		d.debouncer = 0
	}
	direct := d.debouncer < d.maxDebouncer || now.Sub(d.lastBlockSent) > 5*time.Second
	if direct {
		if now.Sub(d.lastBlockSent) > 5*time.Second {
			d.lastBlockSent = now
		}
	} else {
		if d.timer != nil {
			d.timer.Stop()
		}
		d.timer = time.AfterFunc(120*time.Millisecond, func() { r.queue.push(ref) })
	}
	r.mu.Unlock()
	if direct {
		r.queue.push(ref)
	}
}

func (r *Reporter) onPendingTx() {
	r.mu.Lock()
	now := time.Now()
	since := now.Sub(r.lastPendingLog)
	r.lastPendingLog = now
	r.st.Pending++
	if since > 50*time.Millisecond {
		r.mu.Unlock()
		go r.getPending()
		return
	}
	if r.pendingTimer != nil {
		r.pendingTimer.Stop()
	}
	r.pendingTimer = time.AfterFunc(5*time.Millisecond, r.getPending)
	r.mu.Unlock()
}

func (r *Reporter) rpcClient() *rpc.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.rpcUp {
		return nil
	}
	return r.client
}

func (r *Reporter) getBlock(c *rpc.Client, ref any) (map[string]any, error) {
	var raw json.RawMessage
	var err error
	switch v := ref.(type) {
	case string:
		if len(v) == 66 {
			err = c.Call("eth_getBlockByHash", &raw, v, false)
		} else {
			err = c.Call("eth_getBlockByNumber", &raw, v, false)
		}
	case float64:
		err = c.Call("eth_getBlockByNumber", &raw, rpc.BlockTag(v), false)
	default:
		err = c.Call("eth_getBlockByNumber", &raw, "latest", false)
	}
	if err != nil {
		return nil, err
	}
	return rpc.FormatBlock(raw), nil
}

func (r *Reporter) fetchQueuedBlock(ref any) {
	c := r.rpcClient()
	if c == nil {
		return
	}
	b, err := r.getBlock(c, ref)
	r.validateLatestBlock(b, err)
}

func (r *Reporter) getLatestBlock() {
	c := r.rpcClient()
	if c == nil {
		return
	}
	b, err := r.getBlock(c, "latest")
	r.validateLatestBlock(b, err)
}

// formatBlock is the JS client's check (number >= 0, difficulty and
// totalDifficulty present; logsBloom dropped).
func formatBlock(b map[string]any) map[string]any {
	if b == nil {
		return nil
	}
	n, ok := b["number"].(float64)
	if !ok || n < 0 {
		return nil
	}
	if _, ok := b["difficulty"]; !ok {
		return nil
	}
	if _, ok := b["totalDifficulty"]; !ok {
		return nil
	}
	delete(b, "logsBloom")
	return b
}

func (r *Reporter) validateLatestBlock(raw map[string]any, err error) {
	if err != nil {
		r.log.Debug("getLatestBlock couldn't fetch block", "err", err)
		return
	}
	block := formatBlock(raw)
	if block == nil {
		r.log.Debug("got bad block")
		return
	}
	number := block["number"].(float64)

	r.mu.Lock()
	cur, _ := r.st.Block.(map[string]any)
	if curN, _ := cur["number"].(float64); curN == number {
		a, _ := json.Marshal(cur)
		b, _ := json.Marshal(block)
		if string(a) == string(b) {
			r.mu.Unlock()
			return
		}
	}
	r.st.Block = block
	var historyRange []float64
	if number-r.lastBlock > 1 {
		from := math.Max(number-maxBlocksHistory, r.lastBlock+1)
		for x := from; x < math.Max(number, 0); x++ {
			historyRange = append(historyRange, x)
		}
	}
	if number > r.lastBlock {
		r.lastBlock = number
	}
	r.mu.Unlock()

	r.emit("block", map[string]any{"id": r.cfg.ID, "block": block})

	if historyRange != nil && r.queue.idle() {
		go r.getHistory(historyRange)
	}
}

func (r *Reporter) getPending() {
	c := r.rpcClient()
	if c == nil {
		return
	}
	var hex string
	if err := c.Call("eth_getBlockTransactionCountByNumber", &hex, "pending"); err != nil {
		r.log.Debug("getPending error", "err", err)
		return
	}
	pending, _ := rpc.HexToFloat(hex)
	r.mu.Lock()
	r.st.Pending = pending
	changed := r.lastPending != pending
	r.lastPending = pending
	r.mu.Unlock()
	if changed {
		r.emit("pending", map[string]any{"id": r.cfg.ID, "stats": map[string]any{"pending": pending}})
	}
}

func (r *Reporter) getStats(forced bool) {
	r.mu.Lock()
	// one round at a time: with a hung node each round waits for the 15 s timeout
	if r.statsBusy {
		r.mu.Unlock()
		return
	}
	r.statsBusy = true
	defer func() { r.mu.Lock(); r.statsBusy = false; r.mu.Unlock() }()
	if r.socketUp {
		r.lastStats = r.statsJSON()
	}
	c := r.client
	up := r.rpcUp
	r.mu.Unlock()
	if !up || c == nil {
		return
	}

	type res struct {
		peers, gasPrice json.RawMessage
		mining          bool
		syncing         json.RawMessage
		err             error
	}
	var out res
	var wg sync.WaitGroup
	var emu sync.Mutex
	setErr := func(e error) {
		emu.Lock()
		if out.err == nil {
			out.err = e
		}
		emu.Unlock()
	}
	wg.Add(4)
	go func() { defer wg.Done(); setErr(c.Call("net_peerCount", &out.peers)) }()
	go func() {
		defer wg.Done()
		// newer nodes drop eth_mining: an error means "not mining", not a dead node
		var m bool
		if c.Call("eth_mining", &m) == nil {
			out.mining = m
		}
	}()
	go func() { defer wg.Done(); setErr(c.Call("eth_gasPrice", &out.gasPrice)) }()
	go func() { defer wg.Done(); setErr(c.Call("eth_syncing", &out.syncing)) }()
	wg.Wait()

	r.mu.Lock()
	r.tries++
	if out.err != nil {
		r.mu.Unlock()
		r.log.Warn("getStats error", "err", out.err)
		r.fail(c, out.err)
		return
	}
	var peersHex, gasHex string
	_ = json.Unmarshal(out.peers, &peersHex)
	_ = json.Unmarshal(out.gasPrice, &gasHex)
	peers, _ := rpc.HexToFloat(peersHex)
	gas, _ := rpc.HexToDecimal(gasHex)
	r.st.Active = true
	r.st.Peers = peers
	r.st.GasPrice = gas
	r.st.Mining = out.mining
	sync := rpc.FormatSyncing(out.syncing)
	if m, ok := sync.(map[string]any); ok {
		setProgress(m)
		r.st.Syncing = m
	} else {
		r.st.Syncing = false
	}
	r.setUptime()
	r.mu.Unlock()
	r.sendStatsUpdate(forced)
}

func setProgress(m map[string]any) {
	cur, _ := m["currentBlock"].(float64)
	start, _ := m["startingBlock"].(float64)
	high, _ := m["highestBlock"].(float64)
	p := (cur - start) / (high - start)
	if math.IsNaN(p) || math.IsInf(p, 0) {
		m["progress"] = nil
	} else {
		m["progress"] = p
	}
}

func (r *Reporter) setUptime() {
	if r.tries > 0 {
		r.st.Uptime = (r.tries - r.down) / r.tries * 100
	}
}

func (r *Reporter) setInactive() {
	r.mu.Lock()
	r.st.Active = false
	r.st.Peers = 0
	r.st.Mining = false
	r.down++
	r.setUptime()
	r.mu.Unlock()
	r.sendStatsUpdate(true)
}

func (r *Reporter) sendStatsUpdate(force bool) {
	r.mu.Lock()
	changed := r.lastStats != r.statsJSON()
	payload := map[string]any{"id": r.cfg.ID, "stats": map[string]any{
		"active": r.st.Active, "syncing": r.st.Syncing, "mining": r.st.Mining,
		"peers": r.st.Peers, "gasPrice": r.st.GasPrice, "uptime": r.st.Uptime,
	}}
	r.mu.Unlock()
	if changed || force {
		r.emit("stats", payload)
	}
}

// getHistory answers a history request (list of heights) or, without one,
// sends the 49 blocks below the current one.
func (r *Reporter) getHistory(list []float64) {
	if list == nil {
		r.mu.Lock()
		n, _ := r.st.Block.(map[string]any)["number"].(float64)
		r.mu.Unlock()
		for x := n - 1; x > n-maxHistoryUpdate; x-- {
			list = append(list, x)
		}
	}
	if len(list) == 0 {
		return
	}
	c := r.rpcClient()
	if c == nil {
		return
	}
	results := make([]any, 0, len(list))
	for _, n := range list {
		b, err := r.getBlock(c, n)
		if err != nil {
			r.log.Debug("history fetch failed", "err", err)
			return
		}
		if fb := formatBlock(b); fb != nil {
			results = append(results, fb)
		} else {
			results = append(results, false)
		}
	}
	for i, j := 0, len(results)-1; i < j; i, j = i+1, j-1 {
		results[i], results[j] = results[j], results[i]
	}
	r.emit("history", map[string]any{"id": r.cfg.ID, "history": results})
}

// ---------------- server socket ----------------

func (r *Reporter) emit(event string, payload any) {
	r.mu.Lock()
	c := r.server
	up := r.socketUp
	r.mu.Unlock()
	if !up || c == nil {
		return
	}
	if err := c.Emit(event, payload); err != nil {
		r.log.Debug("socket emit error", "err", err)
	}
}

func (r *Reporter) serverLoop(ctx context.Context) {
	for ctx.Err() == nil {
		r.log.Info("starting socket connection", "server", r.cfg.Server)
		err := primus.Run(ctx, r.cfg.Server, "/api",
			func(c *primus.Conn) {
				r.mu.Lock()
				r.server = c
				info := cloneMap(r.cfg.Info)
				r.mu.Unlock()
				r.log.Info("socket open, logging in")
				_ = c.Emit("hello", map[string]any{"id": r.cfg.ID, "info": info, "secret": r.cfg.Secret})
			},
			r.onServerEvent)

		r.mu.Lock()
		r.server = nil
		r.socketUp = false
		r.mu.Unlock()

		wait := serverRetry
		if errors.Is(err, primus.ErrServerClose) {
			wait = serverRefusedRetry
		}
		if ctx.Err() == nil {
			r.log.Warn("socket connection ended", "err", err, "retry_in", wait)
			sleep(ctx, wait)
		}
	}
}

func (r *Reporter) onServerEvent(c *primus.Conn, event string, args []json.RawMessage) {
	switch event {
	case "ready":
		r.mu.Lock()
		r.socketUp = true
		r.mu.Unlock()
		r.log.Info("socket connection established")
		go func() {
			r.getLatestBlock()
			r.getPending()
			r.getStats(true)
		}()
	case "history":
		var req struct {
			List []float64 `json:"list"`
		}
		var list []float64
		if len(args) > 0 && json.Unmarshal(args[0], &req) == nil && req.List != nil {
			list = req.List
		}
		go r.getHistory(list)
	case "node-pong":
		var p struct {
			ClientTime float64 `json:"clientTime"`
		}
		if len(args) > 0 && json.Unmarshal(args[0], &p) == nil {
			latency := math.Ceil((float64(time.Now().UnixMilli()) - p.ClientTime) / 2)
			_ = c.Emit("latency", map[string]any{"id": r.cfg.ID, "latency": latency})
		}
	}
}

// ---------------- timers ----------------

func (r *Reporter) timers(ctx context.Context) {
	stats := time.NewTicker(updateInterval)
	ping := time.NewTicker(pingInterval)
	defer stats.Stop()
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stats.C:
			go r.getStats(false)
		case <-ping.C:
			r.mu.Lock()
			c, up := r.server, r.socketUp
			r.mu.Unlock()
			if up && c != nil {
				_ = c.Emit("node-ping", map[string]any{"id": r.cfg.ID, "clientTime": float64(time.Now().UnixMilli())})
			}
		}
	}
}

// ---------------- helpers ----------------

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// parseIntJS is parseInt(s, 10) for net_version.
func parseIntJS(s string) (float64, error) {
	var n float64
	digits := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + float64(ch-'0')
		digits++
	}
	if digits == 0 {
		return 0, errors.New("NaN")
	}
	return n, nil
}

// blockQueue is async.queue(worker, 1) with drain → getPending.
type blockQueue struct {
	mu      sync.Mutex
	items   []any
	running bool
	worker  func(any)
	drain   func()
}

func newBlockQueue(worker func(any), drain func()) *blockQueue {
	return &blockQueue{worker: worker, drain: drain}
}

func (q *blockQueue) push(ref any) {
	q.mu.Lock()
	q.items = append(q.items, ref)
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()
	go q.run()
}

func (q *blockQueue) run() {
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.running = false
			q.mu.Unlock()
			q.drain()
			return
		}
		ref := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		q.worker(ref)
	}
}

func (q *blockQueue) idle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.running && len(q.items) == 0
}
