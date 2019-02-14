2Miners Network Stats Dashboard
============

This is the backend service which runs along with node and tracks the network status, fetches information through WS-RPC and connects through WebSockets to feed information. 

# How to add your node

## Prerequisite
* parity or geth
* node
* npm


## Configuration

Configure the app modifying [processes.json](/processes.json). Note that you have to modify the backup processes.json file located in `./processes.json` (to allow you to set your env vars without being rewritten when updating).

```bash
"env":
	{
		"NODE_ENV"        : "production", // tell the client we're in production environment
		"RPC_HOST"        : "localhost", // geth or parity JSON-RPC host
		"RPC_PORT"        : "8545", // geth or parity JSON-RPC port
		"LISTENING_PORT"  : "30303", // eth listening port (only used for display)
		"INSTANCE_NAME"   : "2Miners.com Ethereum", // whatever you wish to name your node
		"CONTACT_DETAILS" : "eth.2miners.com", // add your contact details here if you wish (email/skype)
		"WS_SERVER"       : "wss://eth-stats.2miners.com", // path to netstats WebSockets api server
		"WS_SECRET"       : "4ce279da264302069d919845eead5350", // WebSockets api server secret used for login
		"VERBOSITY"       : 0 // Set the verbosity (0 = silent, 1 = error, warn, 2 = error, warn, info, success, 3 = all logs)
	}
```

## GET and Run

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
pm2 startup
```
