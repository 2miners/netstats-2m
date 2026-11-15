// netstats-2m reports an Ethereum-family node, or a Bitcoin fork such as
// ravend, to a netstats server.
//
// Configuration is the environment of the former JS client:
//
//	CHAIN                eth (default) or btc (bitcoind-style JSON-RPC: RVN, ...)
//	WS_HOST, WS_PORT     eth: node WebSocket RPC (default localhost:8546); the
//	                     old RPC_HOST, RPC_PORT names work too, WS_* wins
//	RPC_HOST, RPC_PORT   btc: node HTTP RPC (default localhost:8766)
//	RPC_USER, RPC_PASSWORD  btc: rpcuser / rpcpassword of the node
//	CHAIN=monero|ergo|ckb|beam|grin|ae|substrate|quai  other node APIs, polled:
//	                     RPC_HOST, RPC_PORT, RPC_USER, RPC_PASSWORD (monero
//	                     digest login, grin foreign API secret),
//	                     RPC_OWNER_PASSWORD (grin owner API), NODE_NAME,
//	                     POLL_INTERVAL (default 1s)
//	SUBSIDY_START, SUBSIDY_HALVING  btc: block reward schedule to tell fees
//	                     from the reward (default Ravencoin: 5000, 2100000)
//	WS_SERVER            netstats server, e.g. wss://ethw-stats.2miners.com
//	WS_SECRET            server secret
//	INSTANCE_NAME        name on the page (required with NODE_ENV=production)
//	CONTACT_DETAILS      optional contact shown on the page
//	LISTENING_PORT       p2p port, display only (default 30303)
//	VERBOSITY            0 errors, 1 + warnings, 2 + info, 3 everything (default 0)
//
// Settings can also come from a file given with -env (KEY=VALUE lines).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/2miners/netstats-2m/internal/camel"
	"github.com/2miners/netstats-2m/internal/reporter"
)

var version = "1.0.0"

func loadEnvFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// nodeEnv returns the first non-empty of the given variables, else def.
func nodeEnv(def string, keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return def
}

func osRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func main() {
	envFile := flag.String("env", "", "file with KEY=VALUE settings (real env wins)")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *envFile != "" {
		if err := loadEnvFile(*envFile); err != nil {
			fmt.Fprintln(os.Stderr, "env file:", err)
			os.Exit(1)
		}
	}

	level := slog.LevelError
	switch getenv("VERBOSITY", "0") {
	case "1":
		level = slog.LevelWarn
	case "2":
		level = slog.LevelInfo
	case "3":
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	name := os.Getenv("INSTANCE_NAME")
	if os.Getenv("NODE_ENV") == "production" && name == "" {
		log.Error("no instance name specified")
		os.Exit(1)
	}
	if name == "" {
		name, _ = os.Hostname()
	}

	var port any = 30303
	if p := os.Getenv("LISTENING_PORT"); p != "" {
		port = p
	}

	info := map[string]any{
		"name":             name,
		"contact":          os.Getenv("CONTACT_DETAILS"),
		"coinbase":         nil,
		"node":             nil,
		"net":              nil,
		"api":              nil,
		"port":             port,
		"os":               runtime.GOOS,
		"os_v":             osRelease(),
		"client":           strings.TrimPrefix(version, "v"),
		"canUpdateHistory": true,
	}

	secret := os.Getenv("WS_SECRET")
	if secret == "" {
		secret = "eth-net-stats-has-a-secret"
	}

	var rpcURL string
	var dial reporter.Dialer
	switch chain := getenv("CHAIN", "eth"); chain {
	case "eth":
		rpcURL = "ws://" + nodeEnv("localhost", "WS_HOST", "RPC_HOST") + ":" + nodeEnv("8546", "WS_PORT", "RPC_PORT")
		dial = reporter.EthDialer(rpcURL)
	case "btc":
		rpcURL = "http://" + getenv("RPC_HOST", "localhost") + ":" + getenv("RPC_PORT", "8766") + "/"
		sub := reporter.Subsidy{Start: 5000, Halving: 2100000} // Ravencoin
		if v := os.Getenv("SUBSIDY_START"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				log.Error("bad SUBSIDY_START", "value", v)
				os.Exit(1)
			}
			sub.Start = f
		}
		if v := os.Getenv("SUBSIDY_HALVING"); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				log.Error("bad SUBSIDY_HALVING", "value", v)
				os.Exit(1)
			}
			sub.Halving = n
		}
		dial = reporter.BTCDialer(rpcURL, os.Getenv("RPC_USER"), os.Getenv("RPC_PASSWORD"), sub)
	case "monero", "ergo", "ckb", "beam", "grin", "ae", "substrate", "quai":
		rpcURL = "http://" + getenv("RPC_HOST", "localhost") + ":" + os.Getenv("RPC_PORT")
		poll := time.Second
		if v := os.Getenv("POLL_INTERVAL"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				log.Error("bad POLL_INTERVAL", "value", v)
				os.Exit(1)
			}
			poll = d
		}
		var err error
		dial, err = reporter.ChainDialer(reporter.ChainConfig{
			Kind: chain, URL: rpcURL, User: os.Getenv("RPC_USER"), Pass: os.Getenv("RPC_PASSWORD"),
			OwnerPass: os.Getenv("RPC_OWNER_PASSWORD"), Name: os.Getenv("NODE_NAME"), Poll: poll,
		})
		if err != nil {
			log.Error("chain", "err", err)
			os.Exit(1)
		}
	default:
		log.Error("unknown CHAIN, want eth, btc, monero, ergo, ckb, beam, grin, ae, substrate or quai", "chain", chain)
		os.Exit(1)
	}

	cfg := reporter.Config{
		ID:     camel.Case(name),
		Info:   info,
		Dial:   dial,
		RPCURL: rpcURL,
		Server: getenv("WS_SERVER", "ws://localhost:3000"),
		Secret: secret,
		Log:    log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("netstats client", "version", version, "id", cfg.ID, "server", cfg.Server, "rpc", cfg.RPCURL)
	reporter.New(cfg).Run(ctx)
}
