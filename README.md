2Miners netstats client
=======================

Node-side reporter for the 2Miners network stats pages ([ETC](https://2miners.com/etc-stats/), [ETHW](https://2miners.com/ethw-stats/)). It runs next to an Ethereum-family node (geth, core-geth, ...), reads it over **WebSocket JSON-RPC** (`newBlockHeaders` / `pendingTransactions` subscriptions plus periodic stats) and pushes the data over WebSockets (Primus) to the 2Miners netstats server.

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
| `WS_SECRET` | server secret: the value in [processes.json](processes.json) (public on purpose, any pool may report its node) |
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

The client keeps one RPC connection and retries every 5 s while the node is unreachable; memory stays flat. Requests time out after 15 s, so a hung node is shown as down instead of piling up requests.

The client has no web3 dependency: [lib/rpc.js](lib/rpc.js) is a small JSON-RPC client on top of `ws` (requests, `eth_subscribe` for `newHeads` and `newPendingTransactions`).
