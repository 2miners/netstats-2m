// netstats-2m reports an Ethereum-family node to a netstats server.
//
// Configuration is the environment of the former JS client:
//
//	RPC_HOST, RPC_PORT   node WebSocket RPC (default localhost:8545)
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
	"strings"
	"syscall"

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
		"client":           version,
		"canUpdateHistory": true,
	}

	secret := os.Getenv("WS_SECRET")
	if secret == "" {
		secret = "eth-net-stats-has-a-secret"
	}

	cfg := reporter.Config{
		ID:     camel.Case(name),
		Info:   info,
		RPCURL: "ws://" + getenv("RPC_HOST", "localhost") + ":" + getenv("RPC_PORT", "8545"),
		Server: getenv("WS_SERVER", "ws://localhost:3000"),
		Secret: secret,
		Log:    log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("netstats client", "version", version, "id", cfg.ID, "server", cfg.Server, "rpc", cfg.RPCURL)
	reporter.New(cfg).Run(ctx)
}
