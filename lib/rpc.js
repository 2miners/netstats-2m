'use strict';

// Minimal JSON-RPC client for the node, a replacement for the old web3 0.x API.

var RPC_URL = 'http://' + (process.env.RPC_HOST || 'localhost') + ':' + (process.env.RPC_PORT || '8545');
var RPC_TIMEOUT = parseInt(process.env.RPC_TIMEOUT, 10) || 10000;
var FILTER_POLL_INTERVAL = 500;

var requestId = 0;

function call(method, params)
{
	return fetch(RPC_URL, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify({ jsonrpc: '2.0', id: ++requestId, method: method, params: params || [] }),
		signal: AbortSignal.timeout(RPC_TIMEOUT)
	})
	.then(function (res) {
		if (!res.ok)
			throw new Error(method + ': HTTP ' + res.status);

		return res.json();
	})
	.then(function (body) {
		if (body.error) {
			var err = new Error(method + ': ' + body.error.message);
			err.code = body.error.code;
			throw err;
		}

		return body.result;
	});
}

function toNumber(hex)
{
	if (hex === null || hex === undefined)
		return 0;

	return Number(BigInt(hex));
}

function toDecimalString(hex)
{
	if (hex === null || hex === undefined)
		return '0';

	return BigInt(hex).toString(10);
}

function toHex(number)
{
	return '0x' + Number(number).toString(16);
}

function formatBlock(block)
{
	if (!block)
		return block;

	['number', 'gasLimit', 'gasUsed', 'size', 'timestamp'].forEach(function (field) {
		if (block[field] !== undefined)
			block[field] = toNumber(block[field]);
	});

	['difficulty', 'totalDifficulty'].forEach(function (field) {
		if (block[field] !== undefined && block[field] !== null)
			block[field] = toDecimalString(block[field]);
	});

	return block;
}

function formatSyncing(sync)
{
	if (!sync)
		return false;

	['startingBlock', 'currentBlock', 'highestBlock', 'knownStates', 'pulledStates'].forEach(function (field) {
		if (sync[field] !== undefined)
			sync[field] = toNumber(sync[field]);
	});

	return sync;
}

// id: block number, block hash, or a tag like 'latest'
function getBlock(id)
{
	if (typeof id === 'string' && /^0x[0-9a-fA-F]{64}$/.test(id))
		return call('eth_getBlockByHash', [id, false]).then(formatBlock);

	if (typeof id === 'number')
		id = toHex(id);

	return call('eth_getBlockByNumber', [id, false]).then(formatBlock);
}

// Polls eth_getFilterChanges and calls onChange(item) for every new item.
// type: 'latest' (new block hashes) or 'pending' (new pending tx hashes)
function Filter(type, onChange, onError)
{
	this.method = (type === 'pending') ? 'eth_newPendingTransactionFilter' : 'eth_newBlockFilter';
	this.onChange = onChange;
	this.onError = onError || function () {};
	this.id = null;
	this.timer = null;
	this.stopped = false;

	this.poll();
}

Filter.prototype.schedule = function ()
{
	var self = this;

	if (!this.stopped)
		this.timer = setTimeout(function () { self.poll(); }, FILTER_POLL_INTERVAL);
}

Filter.prototype.poll = function ()
{
	var self = this;
	var request = (this.id === null)
		? call(this.method).then(function (id) { self.id = id; return []; })
		: call('eth_getFilterChanges', [this.id]);

	request
	.then(function (items) {
		if (self.stopped)
			return;

		(items || []).forEach(function (item) { self.onChange(item); });
	})
	.catch(function (err) {
		// the filter is lost after node restart or expiry, install a new one
		self.id = null;
		self.onError(err);
	})
	.then(function () {
		self.schedule();
	});
}

Filter.prototype.stop = function ()
{
	this.stopped = true;
	clearTimeout(this.timer);

	if (this.id !== null)
		call('eth_uninstallFilter', [this.id]).catch(function () {});
}

module.exports = {
	url: RPC_URL,
	call: call,
	toNumber: toNumber,
	toDecimalString: toDecimalString,
	formatSyncing: formatSyncing,
	getBlock: getBlock,
	Filter: Filter
};
