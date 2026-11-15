2Miners netstats client
=======================

Node-side reporter for the 2Miners network stats pages ([ETC](https://2miners.com/etc-stats/), [ETHW](https://2miners.com/ethw-stats/)). It runs next to an Ethereum-family node (core-geth, geth, ...), reads it over **WebSocket JSON-RPC** (`newHeads` / `newPendingTransactions` subscriptions plus periodic stats) and pushes the data over WebSockets (Primus protocol) to the 2Miners netstats server.

A single static Go binary: no Node.js, npm or PM2.

## Requirements

* A node with the WebSocket RPC enabled (`--ws`, APIs `eth,net,web3`)
* Go 1.26+ to build, or a prebuilt linux amd64/arm64 binary

## Download

Prebuilt static binaries for linux amd64 and arm64 are on the [releases page](https://github.com/2miners/netstats-2m/releases):

```bash
curl -LO https://github.com/2miners/netstats-2m/releases/latest/download/SHA256SUMS
curl -LO https://github.com/2miners/netstats-2m/releases/download/v1.0.1/netstats-2m-v1.0.1-linux-amd64.tar.gz   # or -linux-arm64
sha256sum -c --ignore-missing SHA256SUMS
tar xzf netstats-2m-v1.0.1-linux-amd64.tar.gz
```

Each archive has the binary, `example.env`, the systemd unit, README and LICENSE.

## Build

```bash
git clone https://github.com/2miners/netstats-2m
cd netstats-2m
make build            # build/bin/netstats-2m (linux/amd64, static)
make release          # build/release: linux amd64 + arm64 archives and SHA256SUMS
```

## Configure

One env file per node, see [example.env](example.env):

| Var | Meaning |
|---|---|
| `RPC_HOST`, `RPC_PORT` | node WebSocket RPC (default `localhost:8545`) |
| `WS_SERVER` | netstats server: `wss://ethw-stats.2miners.com` or `wss://etc-stats.2miners.com` |
| `WS_SECRET` | server secret: the value in [example.env](example.env) (public on purpose, any pool may report its node) |
| `INSTANCE_NAME` | name shown on the page; required with `NODE_ENV=production`. The node id on the server is its camelCase form |
| `CONTACT_DETAILS` | optional contact shown on the page |
| `LISTENING_PORT` | p2p port, display only |
| `VERBOSITY` | 0 errors, 1 + warnings, 2 + info, 3 everything |

Real environment variables override the file.

## Run with systemd

```bash
useradd --system --home-dir /opt/netstats-2m --shell /usr/sbin/nologin netstats-2m
install -d -o netstats-2m -g netstats-2m -m 0750 /opt/netstats-2m
cp build/bin/netstats-2m /opt/netstats-2m/
cp example.env /opt/netstats-2m/ethw.env      # edit it
cp deploy/netstats-2m@.service /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now netstats-2m@ethw
journalctl -u netstats-2m@ethw -f
```

Or by hand: `./netstats-2m -env ethw.env`.

## Behaviour

* One RPC connection to the node; while it is unreachable the client retries every 5 s and memory stays flat. Requests time out after 15 s, so a hung node is reported as down instead of piling up requests.
* The server connection starts after the first successful node connection and reconnects every 5 s (every minute after the server refused the secret).
* Wire-compatible with the old JS client: the same messages, node id and block format, so it can replace it without changes on the server.
