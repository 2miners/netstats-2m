'use strict';

require('./utils/logger.js');

var os = require('os');
var rpc = require('./rpc.js');
var async = require('async');
var _ = require('lodash');
var pjson = require('./../package.json');
var chalk = require('chalk').default;

var Primus = require('primus'),
	Emitter = require('primus-emit'),
	Latency = require('primus-spark-latency'),
	Socket, socket;

var INSTANCE_NAME = process.env.INSTANCE_NAME;
var WS_SECRET = process.env.WS_SECRET || "eth-net-stats-has-a-secret";

var PENDING_WORKS = true;
var MAX_BLOCKS_HISTORY = 40;
var UPDATE_INTERVAL = 5000;
var PING_INTERVAL = 3000;
var MINERS_LIMIT = 5;
var MAX_HISTORY_UPDATE = 50;
var CONNECTION_ATTEMPTS_TIMEOUT = 1000;

Socket = Primus.createSocket({
	transformer: 'websockets',
	pathname: '/api',
	strategy: 'disconnect,online,timeout',
	plugin: {emitter: Emitter, sparkLatency: Latency}
});

if(process.env.NODE_ENV === 'production' && INSTANCE_NAME === "")
{
	console.error("No instance name specified!");
	process.exit(1);
}

console.info('   ');
console.info('   ', 'NET STATS CLIENT');
console.success('   ', 'v' + pjson.version);
console.info('   ');
console.info('   ');

function Node ()
{
	this.info = {
		name: INSTANCE_NAME || (process.env.EC2_INSTANCE_ID || os.hostname()),
		contact: (process.env.CONTACT_DETAILS || ""),
		coinbase: null,
		node: null,
		net: null,
		api: null,
		port: (process.env.LISTENING_PORT || 30303),
		os: os.platform(),
		os_v: os.release(),
		client: pjson.version,
		canUpdateHistory: true,
	};

	this.id = _.camelCase(this.info.name);

	this.stats = {
		active: false,
		mining: false,
		peers: 0,
		pending: 0,
		gasPrice: 0,
		block: {
			number: 0,
			hash: '?',
			difficulty: 0,
			totalDifficulty: 0,
			transactions: [],
			uncles: []
		},
		syncing: false,
		uptime: 0
	};

	this._lastBlock = 0;
	this._lastStats = JSON.stringify(this.stats);
	this._lastFetch = 0;
	this._lastPending = 0;

	this._tries = 0;
	this._down = 0;
	this._lastSent = 0;
	this._latency = 0;

	this._web3 = false;
	this._socket = false;

	this._latestQueue = null;
	this.pendingFilter = false;
	this.chainFilter = false;
	this.syncWatcher = false;
	this._lastSyncing = false;
	this.updateInterval = false;
	this.pingInterval = false;
	this.connectionInterval = false;

	this._lastBlockSentAt = 0;
	this._lastChainLog = 0;
	this._lastPendingLog = 0;
	this._chainDebouncer = 0;
	this._chan_min_time = 50;
	this._max_chain_debouncer = 20;
	this._chain_debouncer_cnt = 0;
	this._connection_attempts = 0;
	this._timeOffset = null;

	this.startWeb3Connection();

	return this;
}

Node.prototype.startWeb3Connection = function()
{
	console.info('Starting RPC connection to', rpc.url);

	this.checkWeb3Connection();
}

Node.prototype.checkWeb3Connection = function()
{
	var self = this;

	if (this._web3)
		return;

	rpc.call('web3_clientVersion')
	.then(function () {
		if (self._web3)
			return;

		console.success('RPC connection established');
		self._web3 = true;
		self.init();
	})
	.catch(function (err) {
		console.error('RPC connection attempt', chalk.cyan('#' + self._connection_attempts++), 'failed:', err.message);
		console.error('Trying again in', chalk.cyan(CONNECTION_ATTEMPTS_TIMEOUT + ' ms'));

		setTimeout(function () {
			self.checkWeb3Connection();
		}, CONNECTION_ATTEMPTS_TIMEOUT);
	});
}

Node.prototype.reconnectWeb3 = function()
{
	console.warn("Uninstalling filters and update interval");

	this._web3 = false;
	this._connection_attempts = 0;

	this.stopWatches();

	console.info("RPC reconnect attempts started");

	this.checkWeb3Connection();
}

Node.prototype.startSocketConnection = function()
{
	if( !this._socket )
	{
		console.info('wsc', 'Starting socket connection');

		socket = new Socket( process.env.WS_SERVER || 'ws://localhost:3000', {
			reconnect: {
				retries: Infinity,
				max: 5000,
				min: 5000
			}
		});

		this.setupSockets();
	}
}

Node.prototype.setupSockets = function()
{
	var self = this;

	// Setup events
	socket.on('open', function open()
	{
		console.info('wsc', 'The socket connection has been opened.');
		console.info('   ', 'Trying to login');

		socket.emit('hello', {
			id: self.id,
			info: self.info,
			secret: WS_SECRET
		});
	})
	.on('ready', function()
	{
		self._socket = true;
		console.success('wsc', 'The socket connection has been established.');

		self.getLatestBlock();
		self.getPending();
		self.getStats(true);
	})
	.on('data', function incoming(data)
	{
		console.stats('Socket received some data', data);
	})
	.on('history', function (data)
	{
		console.stats('his', 'Got history request');

		self.getHistory( data );
	})
	.on('node-pong', function(data)
	{
		var now = _.now();
		var latency = Math.ceil( (now - data.clientTime) / 2 );

		socket.emit('latency', {
			id: self.id,
			latency: latency
		});
	})
	.on('end', function end()
	{
		self._socket = false;
		console.error('wsc', 'Socket connection end received');
	})
	.on('error', function error(err)
	{
		console.error('wsc', 'Socket error:', err);
	})
	.on('timeout', function ()
	{
		self._socket = false;
		console.error('wsc', 'Socket connection timeout');
	})
	.on('close', function ()
	{
		self._socket = false;
		console.error('wsc', 'Socket connection has been closed');
	})
	.on('offline', function ()
	{
		self._socket = false;
		console.error('wsc', 'Network connection is offline');
	})
	.on('online', function ()
	{
		self._socket = true;
		console.info('wsc', 'Network connection is online');
	})
	.on('reconnect', function ()
	{
		console.info('wsc', 'Socket reconnect attempt started');
	})
	.on('reconnect scheduled', function (opts)
	{
		self._socket = false;
		console.warn('wsc', 'Reconnecting in', opts.scheduled, 'ms');
		console.warn('wsc', 'This is attempt', opts.attempt, 'out of', opts.retries);
	})
	.on('reconnected', function (opts)
	{
		self._socket = true;
		console.success('wsc', 'Socket reconnected successfully after', opts.duration, 'ms');

		self.getLatestBlock();
		self.getPending();
		self.getStats(true);
	})
	.on('reconnect timeout', function (err, opts)
	{
		self._socket = false;
		console.error('wsc', 'Socket reconnect atempt took too long:', err.message);
	})
	.on('reconnect failed', function (err, opts)
	{
		self._socket = false;
		console.error('wsc', 'Socket reconnect failed:', err.message);
	});
}

Node.prototype.emit = function(message, payload)
{
	if(this._socket)
	{
		try {
			socket.emit(message, payload);
			console.sstats('wsc', 'Socket emited message:', chalk.reset.cyan(message));
			// console.success('wsc', payload);
		}
		catch (err) {
			console.error('wsc', 'Socket emit error:', err);
		}
	}
}

Node.prototype.getInfo = function()
{
	var self = this;

	console.info('==>', 'Getting info');
	console.time('Got info');

	function optional(method)
	{
		return rpc.call(method).catch(function (err) {
			console.warn('Couldn\'t get', method + ':', err.message);
			return null;
		});
	}

	return Promise.all([
		optional('eth_coinbase'),
		optional('web3_clientVersion'),
		optional('net_version')
	])
	.then(function (results) {
		self.info.coinbase = results[0];
		self.info.node = results[1];
		self.info.net = results[2];
		self.info.api = pjson.version;

		console.timeEnd('Got info');
		console.info(self.info);
	});
}

Node.prototype.setInactive = function()
{
	this.stats.active = false;
	this.stats.peers = 0;
	this.stats.mining = false;
	this._down++;

	this.setUptime();

	this.sendStatsUpdate(true);

	// Schedule web3 reconnect
	this.reconnectWeb3();

	return this;
}

Node.prototype.setUptime = function()
{
	this.stats.uptime = ((this._tries - this._down) / this._tries) * 100;
}

Node.prototype.formatBlock = function (block)
{
	if( !_.isNull(block) && !_.isUndefined(block) && !_.isUndefined(block.number) && block.number >= 0 && !_.isUndefined(block.difficulty) && !_.isUndefined(block.totalDifficulty) )
	{
		block.difficulty = block.difficulty.toString(10);
		block.totalDifficulty = block.totalDifficulty.toString(10);

		if( !_.isUndefined(block.logsBloom) )
		{
			delete block.logsBloom;
		}

		return block;
	}

	return false;
}

Node.prototype.getLatestBlock = function ()
{
	var self = this;

	if(this._web3)
	{
		var timeString = 'Got block in' + chalk.reset.red('');
		console.time('==>', timeString);

		rpc.getBlock('latest').then(function (result) {
			self.validateLatestBlock(null, result, timeString);
		}, function (error) {
			self.validateLatestBlock(error, null, timeString);
		});
	}
}

Node.prototype.validateLatestBlock = function (error, result, timeString)
{
	console.timeEnd('==>', timeString);

	if( error )
	{
		console.error("xx>", "getLatestBlock couldn't fetch block...");
		console.error("xx>", error);

		return false;
	}

	var block = this.formatBlock(result);

	if(block === false)
	{
		console.error("xx>", "Got bad block:", chalk.reset.cyan(result));

		return false;
	}

	if( this.stats.block.number === block.number )
	{
		console.warn("==>", "Got same block:", chalk.reset.cyan(block.number));

		if( _.isEqual(JSON.stringify(this.stats.block), JSON.stringify(block)) )
			return false;

		console.stats(this.stats.block);
		console.stats(block);
		console.warn("Blocks are different... updating block");
	}

	console.sstats("==>", "Got block:", chalk.reset.red(block.number));

	this.stats.block = block;
	this.sendBlockUpdate();

	if(this.stats.block.number - this._lastBlock > 1)
	{
		var range = _.range( Math.max(this.stats.block.number - MAX_BLOCKS_HISTORY, this._lastBlock + 1), Math.max(this.stats.block.number, 0), 1 );

		if( this._latestQueue.idle() )
			this.getHistory({ list: range });
	}

	if(this.stats.block.number > this._lastBlock)
	{
		this._lastBlock = this.stats.block.number;
	}
}

Node.prototype.getStats = function(forced)
{
	var self = this;
	var now = _.now();
	var lastFetchAgo = now - this._lastFetch;
	this._lastFetch = now;

	if (this._socket)
		this._lastStats = JSON.stringify(this.stats);

	if (this._web3 && (lastFetchAgo >= UPDATE_INTERVAL || forced === true))
	{
		console.stats('==>', 'Getting stats')
		console.stats('   ', 'last update:', chalk.reset.cyan(lastFetchAgo));
		console.stats('   ', 'forced:', chalk.reset.cyan(forced === true));

		Promise.all([
			rpc.call('net_peerCount').then(rpc.toNumber),
			// not every client supports eth_mining, don't mark the node as down for it
			rpc.call('eth_mining').catch(function () { return false; }),
			rpc.call('eth_gasPrice').then(rpc.toDecimalString),
			rpc.call('eth_syncing').then(rpc.formatSyncing)
		])
		.then(function (values) {
			return { peers: values[0], mining: values[1], gasPrice: values[2], syncing: values[3] };
		})
		.then(function (results) {
			return [null, results];
		}, function (err) {
			return [err, null];
		})
		.then(function (args)
		{
			var err = args[0];
			var results = args[1];

			self._tries++;

			if (err) {
				console.error('xx>', 'getStats error: ', err);

				self.setInactive();

				return false;
			}

			results.end = _.now();
			results.diff = results.end - self._lastFetch;

			console.sstats('==>', 'Got getStats results in', chalk.reset.cyan(results.diff, 'ms'));

			if(results.peers !== null)
			{
				self.stats.active = true;
				self.stats.peers = results.peers;
				self.stats.mining = results.mining;
				self.stats.gasPrice = results.gasPrice;

				if(results.syncing !== false) {
					var sync = results.syncing;

					var progress = sync.currentBlock - sync.startingBlock;
					var total = sync.highestBlock - sync.startingBlock;

					sync.progress = progress/total;

					self.stats.syncing = sync;
				} else {
					self.stats.syncing = false;
				}
			}
			else {
				self.setInactive();
			}

			self.setUptime();

			self.sendStatsUpdate(forced);
		});
	}
}

Node.prototype.getPending = function()
{
	var self = this;
	var now = _.now();

	if (this._web3)
	{
		console.stats('==>', 'Getting Pending')

		rpc.call('eth_getBlockTransactionCountByNumber', ['pending'])
		.then(rpc.toNumber)
		.then(function (pending)
		{

			var results = {};
			results.end = _.now();
			results.diff = results.end - now;

			console.sstats('==>', 'Got', chalk.reset.red(pending) , chalk.reset.bold.green('pending tx'+ (pending === 1 ? '' : 's') + ' in'), chalk.reset.cyan(results.diff, 'ms'));

			self.stats.pending = pending;

			if(self._lastPending !== pending)
				self.sendPendingUpdate();

			self._lastPending = pending;
		})
		.catch(function (err)
		{
			console.error('xx>', 'getPending error: ', err.message);
		});
	}
}

Node.prototype.getHistory = function (range)
{
	var self = this;

	var history = [];
	var interv = {};

	console.time('=H=', 'his', 'Got history in');

	if ( _.isUndefined(range) || range === null || _.isUndefined(range.list) )
		interv = _.range(this.stats.block.number - 1, this.stats.block.number - MAX_HISTORY_UPDATE, -1);
	else
		interv = range.list;

	console.stats('his', 'Getting history from', chalk.reset.cyan(interv[0]), 'to', chalk.reset.cyan(interv[interv.length - 1]));

	async.mapSeries(interv, function (number, callback)
	{
		rpc.getBlock(number).then(function (block) {
			callback(null, block);
		}, callback);
	},
	function (err, results)
	{
		if (err) {
			console.error('his', 'history fetch failed:', err);

			results = false;
		}
		else
		{
			for(var i=0; i < results.length; i++)
			{
				results[i] = self.formatBlock(results[i]);
			}
		}

		self.emit('history', {
			id: self.id,
			history: results ? results.reverse() : results
		});

		console.timeEnd('=H=', 'his', 'Got history in');
	});
}

Node.prototype.changed = function ()
{
	var changed = ! _.isEqual( this._lastStats, JSON.stringify(this.stats) );

	return changed;
}

Node.prototype.prepareBlock = function ()
{
	return {
		id: this.id,
		block: this.stats.block
	};
}

Node.prototype.preparePending = function ()
{
	return {
		id: this.id,
		stats: {
			pending: this.stats.pending
		}
	};
}

Node.prototype.prepareStats = function ()
{
	return {
		id: this.id,
		stats: {
			active: this.stats.active,
			syncing: this.stats.syncing,
			mining: this.stats.mining,
			peers: this.stats.peers,
			gasPrice: this.stats.gasPrice,
			uptime: this.stats.uptime
		}
	};
}

Node.prototype.sendBlockUpdate = function()
{
	this._lastBlockSentAt = _.now();
	console.stats("wsc", "Sending", chalk.reset.red("block"), chalk.bold.white("update"));
	this.emit('block', this.prepareBlock());
}

Node.prototype.sendPendingUpdate = function()
{
	console.stats("wsc", "Sending pending update");
	this.emit('pending', this.preparePending());
}

Node.prototype.sendStatsUpdate = function (force)
{
	if( this.changed() || force ) {
		console.stats("wsc", "Sending", chalk.reset.blue((force ? "forced" : "changed")), chalk.bold.white("update"));
		var stats = this.prepareStats();
		console.info(stats);
		this.emit('stats', stats);
		// this.emit('stats', this.prepareStats());
	}
}

Node.prototype.ping = function()
{
	this._latency = _.now();
	socket.emit('node-ping', {
		id: this.id,
		clientTime: _.now()
	});
};

Node.prototype.setWatches = function()
{
	var self = this;

	this.setFilters();

	if(this.updateInterval)
		clearInterval(this.updateInterval);

	this.updateInterval = setInterval( function(){
		self.getStats();
	}, UPDATE_INTERVAL);

	if( !this.pingInterval )
	{
		this.pingInterval = setInterval( function(){
			self.ping();
		}, PING_INTERVAL);
	}

	this.watchSyncing();
}

Node.prototype.watchSyncing = function()
{
	var self = this;

	if(this.syncWatcher)
		clearInterval(this.syncWatcher);

	this.syncWatcher = setInterval(function () {
		rpc.call('eth_syncing')
		.then(rpc.formatSyncing)
		.then(function (sync) {
			if(sync) {
				if(!self._lastSyncing)
					console.info("SYNC STARTED");

				sync.progress = (sync.currentBlock - sync.startingBlock) / (sync.highestBlock - sync.startingBlock);
				self.stats.syncing = sync;

				if(self._lastBlock !== sync.currentBlock && self._latestQueue) {
					self._latestQueue.push(sync.currentBlock);
				}
				console.info("SYNC UPDATE:", sync);
			} else if(self._lastSyncing) {
				console.info("SYNC STOPPED");
				self.stats.syncing = false;
			}

			self._lastSyncing = !!sync;
		})
		.catch(function (err) {
			console.error("SYNC ERROR", err.message);
		});
	}, 1000);
}

Node.prototype.stopWatches = function()
{
	if(this.updateInterval)
		clearInterval(this.updateInterval);
	this.updateInterval = false;

	if(this.syncWatcher)
		clearInterval(this.syncWatcher);
	this.syncWatcher = false;

	if(this.chainFilter)
		this.chainFilter.stop();
	this.chainFilter = false;

	if(this.pendingFilter)
		this.pendingFilter.stop();
	this.pendingFilter = false;
}

Node.prototype.setFilters = function()
{
	var self = this;

	if(this.chainFilter)
		this.chainFilter.stop();

	if(this.pendingFilter)
		this.pendingFilter.stop();

	this._latestQueue = async.queue(function (hash, callback)
	{
		var timeString = 'Got block ' + chalk.reset.red(hash) + chalk.reset.bold.white(' in') + chalk.reset.green('');

		console.time('==>', timeString);

		rpc.getBlock(hash).then(function (result) {
			self.validateLatestBlock(null, result, timeString);
		}, function (error) {
			self.validateLatestBlock(error, null, timeString);
		})
		.then(function () {
			callback();
		});
	}, 1);

	this._latestQueue.drain(function()
	{
		console.sstats("Finished processing", 'latest', 'queue');

		self.getPending();
	});

	this._debouncedChain = _.debounce(function(hash) {
		console.stats('>>>', 'Debounced');
		self._latestQueue.push(hash);
	}, 120);

	this._debouncedPending = _.debounce(function() {
		self.getPending();
	}, 5);

	this.chainFilter = new rpc.Filter('latest', function (hash)
	{
		var now = _.now();
		var time = now - self._lastChainLog;
		self._lastChainLog = now;

		console.stats('>>>', 'Chain Filter triggered: ', chalk.reset.red(hash), '- last trigger:', chalk.reset.cyan(time));

		if(time < self._chan_min_time)
		{
			self._chainDebouncer++;
			self._chain_debouncer_cnt++;

			if(self._chain_debouncer_cnt > 100)
			{
				self._chan_min_time = Math.max(self._chan_min_time + 1, 200);
				self._max_chain_debouncer = Math.max(self._max_chain_debouncer - 1, 5);
			}
		}
		else
		{
			if(time > 5000)
			{
				self._chan_min_time = 50;
				self._max_chain_debouncer = 20;
				self._chain_debouncer_cnt = 0;
			}
			// reset local chain debouncer
			self._chainDebouncer = 0;
		}

		if(self._chainDebouncer < self._max_chain_debouncer || now - self._lastBlockSentAt > 5000)
		{
			if(now - self._lastBlockSentAt > 5000)
			{
				self._lastBlockSentAt = now;
			}

			self._latestQueue.push(hash);
		}
		else
		{
			self._debouncedChain(hash);
		}
	}, function (err)
	{
		console.error("Chain filter error:", err.message);
	});

	console.success("Installed chain filter");

	this.pendingFilter = new rpc.Filter('pending', function (hash)
	{
		var now = _.now();
		var time = now - self._lastPendingLog;
		self._lastPendingLog = now;

		console.stats('>>>', 'Pending Filter triggered:', chalk.reset.red(hash), '- last trigger:', chalk.reset.cyan(time));

		if(time > 50)
		{
			self.getPending();
		}
		else
		{
			self._debouncedPending();
		}
	}, function (err)
	{
		console.error("Pending filter error:", err.message);
	});

	console.success("Installed pending filter");
}

Node.prototype.init = function()
{
	var self = this;

	// Fetch node info, then start socket connection and set filters
	this.getInfo().then(function () {
		self.startSocketConnection();
		self.setWatches();
	});
}

Node.prototype.stop = function()
{
	if(this._socket)
		socket.end();

	if(this.pingInterval)
		clearInterval(this.pingInterval);

	this.stopWatches();
}

module.exports = Node;
