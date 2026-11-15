2Miners Network Stats Dashboard
============

This is the backend service which runs along with your EthereumPoW (ETHW) node, fetches information from it through HTTP JSON-RPC and feeds it over WebSockets to the [2Miners ETHW network stats](https://ethw-stats.2miners.com) dashboard ([eth-netstats](https://github.com/2miners/eth-netstats)).

# How to add your node

## Prerequisite
* an ETHW node (ETHW Core / geth-compatible client) with HTTP JSON-RPC enabled
* node.js and npm
* [pm2](https://pm2.keymetrics.io/) (`npm install -g pm2`)

## Configuration

Configure the app by editing the `env` section of [processes.json](/processes.json):

```bash
"env":
	{
		"NODE_ENV"        : "production", // tell the client we're in production environment
		"RPC_HOST"        : "localhost", // node HTTP JSON-RPC host
		"RPC_PORT"        : "8545", // node HTTP JSON-RPC port
		"LISTENING_PORT"  : "30303", // p2p listening port (only used for display)
		"INSTANCE_NAME"   : "2Miners.com ETHW", // whatever you wish to name your node
		"CONTACT_DETAILS" : "ethw.2miners.com", // add your contact details here if you wish (email/telegram)
		"WS_SERVER"       : "wss://ethw-stats.2miners.com", // path to netstats WebSockets api server
		"WS_SECRET"       : "4ce279da264302069d919845eead5350", // WebSockets api server secret used for login
		"VERBOSITY"       : 0 // Set the verbosity (0 = silent, 1 = error, warn, 2 = error, warn, info, success, 3 = all logs)
	}
```

## Get and run

Get it using git:

```bash
cd ~
git clone https://github.com/2miners/netstats-2m
cd ~/netstats-2m
npm install
```

Run it using pm2:

```bash
cd ~/netstats-2m
pm2 start processes.json
pm2 save
pm2 startup
```
