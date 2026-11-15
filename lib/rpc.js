'use strict';

// Minimal Ethereum JSON-RPC client over one WebSocket (replaces web3 1.x).
// One instance = one connection; the caller creates a new instance to reconnect.

var EventEmitter = require('events');
var WebSocket = require('ws');

var REQUEST_TIMEOUT = 15000;

function RpcClient(url, options)
{
	EventEmitter.call(this);

	options = options || {};

	this.url = url;
	this.timeout = options.timeout || REQUEST_TIMEOUT;
	this._id = 0;
	this._pending = new Map();      // id -> { resolve, reject, timer, method }
	this._subscriptions = new Map(); // subscription id -> callback
	this._closed = false;

	var self = this;

	this.ws = new WebSocket(url, { handshakeTimeout: this.timeout, perMessageDeflate: false });

	this.ws.on('open', function () {
		self.emit('connect');
	});

	this.ws.on('message', function (data) {
		self._onMessage(data);
	});

	this.ws.on('error', function (err) {
		// 'close' always follows; keep the error for the log
		self._lastError = err;
	});

	this.ws.on('close', function () {
		self._onClose();
	});
}

RpcClient.prototype = Object.create(EventEmitter.prototype);
RpcClient.prototype.constructor = RpcClient;

RpcClient.prototype.isConnected = function ()
{
	return !this._closed && this.ws.readyState === WebSocket.OPEN;
};

RpcClient.prototype.request = function (method, params)
{
	var self = this;

	return new Promise(function (resolve, reject) {
		if (!self.isConnected())
			return reject(new Error('RPC connection not open (' + method + ')'));

		var id = ++self._id;
		var timer = setTimeout(function () {
			if (self._pending.delete(id))
				reject(new Error('RPC timeout after ' + self.timeout + ' ms (' + method + ')'));
		}, self.timeout);

		self._pending.set(id, { resolve: resolve, reject: reject, timer: timer, method: method });

		self.ws.send(JSON.stringify({ jsonrpc: '2.0', id: id, method: method, params: params || [] }), function (err) {
			if (err && self._pending.has(id)) {
				clearTimeout(timer);
				self._pending.delete(id);
				reject(err);
			}
		});
	});
};

// callback style helper: call(method, params, cb(err, result))
RpcClient.prototype.call = function (method, params, callback)
{
	this.request(method, params).then(function (result) {
		callback(null, result);
	}, function (err) {
		callback(err);
	});
};

// eth_subscribe; callback(err, result) for every notification
RpcClient.prototype.subscribe = function (type, callback)
{
	var self = this;

	return this.request('eth_subscribe', [type]).then(function (id) {
		self._subscriptions.set(id, callback);
		return id;
	});
};

RpcClient.prototype.close = function ()
{
	if (this._closed) return;

	try {
		this.ws.removeAllListeners('open');
		this.ws.removeAllListeners('message');
		this.ws.on('error', function () {});
		this.ws.terminate();
	}
	catch (err) {}

	this._onClose();
};

RpcClient.prototype._onMessage = function (data)
{
	var msg;

	try {
		msg = JSON.parse(data.toString());
	}
	catch (err) {
		return;
	}

	var list = Array.isArray(msg) ? msg : [msg];

	for (var i = 0; i < list.length; i++)
	{
		var m = list[i];

		if (m && m.method === 'eth_subscription' && m.params)
		{
			var cb = this._subscriptions.get(m.params.subscription);

			if (cb) {
				try { cb(null, m.params.result); }
				catch (err) { console.error('rpc', 'subscription callback error:', err); }
			}

			continue;
		}

		if (!m || m.id === undefined || !this._pending.has(m.id))
			continue;

		var p = this._pending.get(m.id);

		this._pending.delete(m.id);
		clearTimeout(p.timer);

		if (m.error)
			p.reject(new Error('RPC error ' + m.error.code + ': ' + m.error.message + ' (' + p.method + ')'));
		else
			p.resolve(m.result);
	}
};

RpcClient.prototype._onClose = function ()
{
	if (this._closed) return;

	this._closed = true;

	var err = new Error('RPC connection closed' + (this._lastError ? ': ' + this._lastError.message : ''));

	this._pending.forEach(function (p) {
		clearTimeout(p.timer);
		p.reject(err);
	});

	this._pending.clear();
	this._subscriptions.clear();

	this.emit('close', this._lastError);
	this.removeAllListeners();
};

// ---- formatters (same shapes web3 1.x returned) ----

function toNumber(hex)
{
	if (hex === null || hex === undefined) return hex;
	if (typeof hex === 'number') return hex;

	return Number(BigInt(hex));
}

function toDecimalString(hex)
{
	if (hex === null || hex === undefined) return hex;

	return BigInt(hex).toString(10);
}

function toBlockTag(block)
{
	if (typeof block === 'number') return '0x' + block.toString(16);

	return block; // 'latest', 'pending', hex
}

function formatBlock(b)
{
	if (!b) return b;

	var out = Object.assign({}, b);

	['number', 'gasLimit', 'gasUsed', 'size', 'timestamp', 'baseFeePerGas'].forEach(function (k) {
		if (out[k] !== undefined && out[k] !== null) out[k] = toNumber(out[k]);
	});

	['difficulty', 'totalDifficulty'].forEach(function (k) {
		if (out[k] !== undefined && out[k] !== null) out[k] = toDecimalString(out[k]);
	});

	return out;
}

function formatSyncing(s)
{
	if (!s || s === true) return s;

	var out = Object.assign({}, s);

	['startingBlock', 'currentBlock', 'highestBlock', 'knownStates', 'pulledStates'].forEach(function (k) {
		if (out[k] !== undefined && out[k] !== null) out[k] = toNumber(out[k]);
	});

	return out;
}

module.exports = {
	RpcClient: RpcClient,
	toNumber: toNumber,
	toDecimalString: toDecimalString,
	toBlockTag: toBlockTag,
	formatBlock: formatBlock,
	formatSyncing: formatSyncing
};
