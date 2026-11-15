2Miners netstats client
=======================

Node-side reporter for the 2Miners network stats pages ([ETC](https://2miners.com/etc-stats/), [ETHW](https://2miners.com/ethw-stats/)). It runs next to an Ethereum-family node (geth, core-geth, ...), reads it over **WebSocket JSON-RPC** (`newBlockHeaders` / `pendingTransactions` subscriptions plus periodic stats) and pushes the data over WebSockets (Primus) to an [eth-netstats](https://github.com/cubedro/eth-netstats)-compatible server.

## Requirements

* Node.js 22.12 or newer (developed on 26)
* A node with the WebSocket RPC enabled (`--ws`, APIs `eth,net,web3`)

## Install

```bash
git clone https://github.com/2miners/netstats-2m
cd netstats-2m
npm ci
```

## Configure

Copy [processes.json](processes.json) (for example to `ethw.json`) and set the env vars:

| Var | Meaning |
|---|---|
| `RPC_HOST`, `RPC_PORT` | node WebSocket RPC (default `localhost:8545`) |
| `WS_SERVER` | netstats server, e.g. `wss://ethw-stats.2miners.com` or `wss://etc-stats.2miners.com` |
| `WS_SECRET` | server secret (ask 2Miners) |
| `INSTANCE_NAME` | name shown on the page; required with `NODE_ENV=production` |
| `CONTACT_DETAILS` | optional contact shown on the page |
| `LISTENING_PORT` | p2p port, display only |
| `VERBOSITY` | 0 silent, 1 errors/warnings, 2 + info, 3 everything |

## Run

```bash
pm2 start ethw.json
pm2 save
```

## Behaviour when the node is down

The client keeps one RPC connection and retries every 5 s while the node is unreachable; memory stays flat. Older versions (web3 1.3–1.7 based) multiplied their reconnect attempts every 5 s and ran out of memory within minutes.
