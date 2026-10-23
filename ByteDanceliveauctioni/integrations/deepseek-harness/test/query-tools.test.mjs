import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import test from 'node:test';
import { dashboardParameters, lotPriceParameters } from '../src/schemas.mjs';
import { getDashboard, getLotPrice } from '../src/query-tools.mjs';
import * as queries from '../src/query-tools.mjs';
import { BackendUnavailableError } from '../src/auth.mjs';
import { apply } from '../src/plugin.mjs';

const sections = ['summary', 'funnel', 'trend', 'topLots', 'categoryPerformance', 'orderRisks', 'lotPerformance'];

function snapshot() {
  return {
    currency: 'CNY', snapshotAtUnixMs: 1710000000000,
    range: { range: 'today', timezone: 'Asia/Shanghai', startUnixMs: 1709913600000, endUnixMs: 1710000000000 },
    summary: { gmvAmount: 128800, paidAmount: 100000, pendingAmount: 28800, orderCount: 5, cancelledOrderCount: 1, participantCount: 3, averageDealAmount: 25760, paymentRate: { numerator: 4, denominator: 5, rate: 0.8 }, dealRate: { numerator: 3, denominator: 5, rate: 0.6 } },
    funnel: { createdLotCount: 5, listedLotCount: 4, startedLotCount: 3, bidLotCount: 3, settledLotCount: 3, paidLotCount: 2 },
    trend: [{ startUnixMs: 1709913600000, endUnixMs: 1710000000000, paidAmount: 100000, paidOrders: 4, bidders: 3 }],
    topLots: [{ lotId: 'lot-1', title: '玉器', category: '玉器', paidAmount: 50000, finalAmount: 55000, bidCount: 4, participantCount: 3 }],
    categoryPerformance: [{ category: '玉器', paidAmount: 50000, startedLotCount: 1, settledLotCount: 1, dealRate: { numerator: 1, denominator: 1, rate: 1 } }],
    orderRisks: { pendingPaymentCount: 1, expiringSoonCount: 1, expiredCount: 0, paymentFailedCount: 0 },
    lotPerformance: [{ lotId: 'lot-1', title: '玉器', category: '玉器', status: 'LOT_STATUS_SETTLED', bidCount: 4, participantCount: 3, finalAmount: 55000, paidAmount: 50000, conversionRate: { numerator: 1, denominator: 1, rate: 1 } }],
    partialErrors: [{ section: 'trend', code: 'DATA_INCOMPLETE' }],
  };
}

function priceLot(overrides = {}) {
  return {
    id: 'lot-maotai', roomId: 'room-1', title: '茅台', status: 'LOT_STATUS_BIDDING',
    startPrice: { amount: 10000, currency: 'CNY' },
    currentPrice: { amount: 12000, currency: 'CNY' },
    minIncrement: { amount: 100, currency: 'CNY' },
    capPrice: { amount: 20000, currency: 'CNY' },
    finalPrice: { amount: 0, currency: 'CNY' },
    leadingNickname: '竞拍人甲', bidCount: 3, updatedAtUnixMs: 1710000000000,
    leadingUserId: 'private-leader-id', winnerUserId: 'private-winner-id',
    buyer: { mainAccountId: 'private-account' }, accessToken: 'private-token',
    ...overrides,
  };
}

function priceApi({ lots = [], details = {} } = {}) {
  const searchLotsCalls = []; const getLotCalls = [];
  return {
    searchLotsCalls, getLotCalls,
    async searchLots(args, signal) { searchLotsCalls.push([args, signal]); return { lots, total: lots.length, page: 1, pageSize: 5 }; },
    async getLot(lotId, signal) { getLotCalls.push([lotId, signal]); return details[lotId]; },
  };
}

test('lot price resolves a direct lot ID and returns only public price details', async () => {
  const signal = new AbortController().signal;
  const api = priceApi({ details: { 'lot-direct': priceLot({ id: 'lot-direct' }) } });
  const result = await getLotPrice({ lotId: 'lot-direct' }, { api }, signal);
  assert.deepEqual(api.searchLotsCalls, []);
  assert.deepEqual(api.getLotCalls, [['lot-direct', signal]]);
  assert.deepEqual(result, {
    lotId: 'lot-direct', roomId: 'room-1', title: '茅台', status: 'LOT_STATUS_BIDDING',
    startPrice: { amount: 10000, currency: 'CNY' }, currentPrice: { amount: 12000, currency: 'CNY' },
    minIncrement: { amount: 100, currency: 'CNY' }, capPrice: { amount: 20000, currency: 'CNY' },
    finalPrice: { amount: 0, currency: 'CNY' }, leadingNickname: '竞拍人甲', bidCount: 3,
    updatedAtUnixMs: 1710000000000,
  });
  assert.doesNotMatch(JSON.stringify(result), /private-(leader-id|winner-id|account|token)/);
});

test('lot price resolves one case-insensitive exact Chinese title before other results', async () => {
  const api = priceApi({
    lots: [priceLot({ id: 'lot-exact', title: 'MAOTAI' }), priceLot({ id: 'lot-other', title: '茅台酒' })],
    details: { 'lot-exact': priceLot({ id: 'lot-exact', title: 'MAOTAI' }) },
  });
  const result = await getLotPrice({ keyword: 'maotai' }, { api });
  assert.equal(result.lotId, 'lot-exact');
  assert.deepEqual(api.searchLotsCalls[0][0], { keyword: 'maotai', roomId: '', pageSize: 5 });
  assert.deepEqual(api.getLotCalls.map(([id]) => id), ['lot-exact']);
});

test('lot price resolves the only fuzzy result', async () => {
  const api = priceApi({ lots: [priceLot({ id: 'lot-only', title: '飞天茅台' })], details: { 'lot-only': priceLot({ id: 'lot-only', title: '飞天茅台' }) } });
  const result = await getLotPrice({ keyword: '茅台' }, { api });
  assert.equal(result.lotId, 'lot-only');
  assert.deepEqual(api.getLotCalls.map(([id]) => id), ['lot-only']);
});

test('lot price returns bounded public candidates for ambiguous keywords without guessing', async () => {
  const api = priceApi({ lots: [
    priceLot({ id: 'lot-a', title: '茅台 A' }), priceLot({ id: 'lot-b', title: '茅台 B' }),
    priceLot({ id: 'lot-c', title: '茅台 C' }), priceLot({ id: 'lot-d', title: '茅台 D' }),
    priceLot({ id: 'lot-e', title: '茅台 E' }), priceLot({ id: 'lot-f', title: '茅台 F' }),
  ] });
  const result = await getLotPrice({ keyword: '茅台' }, { api });
  assert.equal(result.status, 'LOT_AMBIGUOUS');
  assert.equal(result.candidates.length, 5);
  assert.deepEqual(Object.keys(result.candidates[0]).sort(), ['lotId', 'roomId', 'status', 'title']);
  assert.equal(api.getLotCalls.length, 0);
  assert.doesNotMatch(JSON.stringify(result), /private-/);
});

test('lot price returns LOT_NOT_FOUND when keyword search has no candidates', async () => {
  const api = priceApi();
  const result = await getLotPrice({ keyword: '不存在的拍品' }, { api });
  assert.deepEqual(result, { status: 'LOT_NOT_FOUND', keyword: '不存在的拍品' });
  assert.equal(api.getLotCalls.length, 0);
});

test('lot price narrows keyword queries to the requested room', async () => {
  const api = priceApi({ lots: [priceLot({ id: 'lot-room', roomId: 'room-2' })], details: { 'lot-room': priceLot({ id: 'lot-room', roomId: 'room-2' }) } });
  const result = await getLotPrice({ keyword: '茅台', roomId: 'room-2' }, { api });
  assert.equal(result.roomId, 'room-2');
  assert.deepEqual(api.searchLotsCalls[0][0], { keyword: '茅台', roomId: 'room-2', pageSize: 5 });
});

test('lot price rejects unknown fields and empty locators before network access', async () => {
  const api = priceApi();
  for (const args of [{}, { keyword: '' }, { lotId: '  ' }, { roomId: 'room-1' }, { keyword: '茅台', unexpected: true }]) {
    await assert.rejects(getLotPrice(args, { api }), /参数|lotId|keyword/);
  }
  assert.equal(api.searchLotsCalls.length, 0);
  assert.equal(api.getLotCalls.length, 0);
});

function effectScope() {
  const cleanups = new Set();
  return {
    effect(setup) {
      const cleanup = setup();
      cleanups.add(cleanup);
      return () => { cleanups.delete(cleanup); cleanup(); };
    },
    dispose() {
      const current = [...cleanups];
      cleanups.clear();
      for (const cleanup of current) cleanup();
    },
    get effectCount() { return cleanups.size; },
  };
}

test('dashboard tool normalizes dashboard arguments, preserves integers, and exposes partial errors', async () => {
  const calls = [];
  const signal = new AbortController().signal;
  const api = { getDashboard: async (...args) => { calls.push(args); return snapshot(); } };
  const defaultValue = await getDashboard({}, { api }, signal);
  const value = await getDashboard({ range: '7d', sections: ['topLots', 'trend', 'topLots'] }, { api }, signal);
  assert.deepEqual(calls[0], [{ range: 'today', sections: [] }, signal]);
  assert.deepEqual(calls[1], [{ range: '7d', sections: ['topLots', 'trend'] }, signal]);
  assert.equal(defaultValue.summary.gmvAmount, 128800);
  assert.equal(value.summary.gmvAmount, 128800);
  assert.equal(value.topLots[0].paidAmount, 50000);
  assert.deepEqual(value.partialErrors, [{ section: 'trend', code: 'DATA_INCOMPLETE' }]);
  assert.match(value.display.summary.gmvAmount, /1,288\.00/);
  assert.match(value.display.topLots[0].paidAmount, /500\.00/);
  await assert.rejects(getDashboard({ range: 'tomorrow' }, { api: {} }), /range/);
  await assert.rejects(getDashboard({ sections: ['summary'], account: 'other' }, { api: {} }), /参数/);
  await assert.rejects(getDashboard({ sections: ['unknown'] }, { api: {} }), /sections/);
});

test('dashboard tool schema is closed and lists the backend ranges and panels', () => {
  assert.deepEqual(dashboardParameters.properties.range.enum, ['today', 'live', '7d', '30d']);
  assert.deepEqual(dashboardParameters.properties.sections.items.enum, sections);
  assert.equal(dashboardParameters.additionalProperties, false);
  assert.deepEqual(Object.keys(dashboardParameters.properties).sort(), ['range', 'sections']);
});

test('dashboard tool registers a credential-free dashboard query backed by the session api', async t => {
  const user = { id: 'merchant-1', username: 'merchant', status: 'USER_STATUS_ACTIVE', permissionCodes: ['lot.view_admin'], roleCodes: ['merchant_owner'], mainAccountId: 'tenant-1' };
  const server = createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    if (req.url === '/api/users/login') return res.end(JSON.stringify({ result: { code: 0 }, user, tokens: { accessToken: 'private-token', accessExpiresAtUnixMs: String(Date.now() + 60000) } }));
    assert.equal(req.headers.authorization, 'Bearer private-token');
    if (req.url === '/api/admin/lots/lot-1') return res.end(JSON.stringify({ result: { code: 0 }, lot: {
      id: 'lot-1', roomId: 'room-1', title: '玉器', status: 'LOT_STATUS_BIDDING', category: '玉器',
      currentPrice: { amount: '12000', currency: 'CNY' }, rule: { startPrice: { amount: '10000', currency: 'CNY' }, minIncrement: { amount: '100', currency: 'CNY' }, capPrice: { amount: '20000', currency: 'CNY' } },
      finalPrice: { amount: '12000', currency: 'CNY' }, leadingNickname: '竞拍人甲', updatedAtUnixMs: '1710000000123',
      startedAtUnixMs: '1710000000000', endsAtUnixMs: '1710003600000', queuePosition: 1,
      stats: { bidCount: 3, participantCount: 2 }, leadingUserId: 'private-leader-id',
    } }));
    assert.equal(req.url, '/api/admin/dashboard?range=today');
    const reply = snapshot();
    reply.categories = reply.categoryPerformance;
    delete reply.categoryPerformance;
    res.end(JSON.stringify({ result: { code: 0 }, ...reply }));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => { server.close(resolve); server.closeAllConnections(); }));
  const tools = new Map(); const cleanup = [];
  const ctx = { tools: { register: tool => tools.set(tool.name, tool) }, skills: { register: () => () => {} }, effect: setup => cleanup.push(setup()) };
  apply(ctx, { apiBaseUrl: `http://127.0.0.1:${server.address().port}` });
  const dashboard = tools.get('auction_get_dashboard');
  const lotPrice = tools.get('auction_get_lot_price');
  assert.equal(tools.size, 7);
  assert.deepEqual(dashboard.parameters, dashboardParameters);
  assert.doesNotMatch(JSON.stringify(dashboard.parameters), /password|username|account/i);
  for (const term of ['今日工作台', '今天咋样', '经营数据', 'GMV', '成交额', '付款/支付率', '成交率', '参拍人数', '平均成交价', '漏斗', '趋势', 'Top 10', '品类', '订单风险', '拍品表现']) assert.match(dashboard.description, new RegExp(term));
  assert.match(dashboard.description, /登录|切换/);
  assert.match(dashboard.description, /不接受.*凭证/);
  assert.ok(lotPrice);
  assert.deepEqual(lotPrice.parameters, lotPriceParameters);
  assert.deepEqual(Object.keys(lotPrice.parameters.properties).sort(), ['keyword', 'lotId', 'roomId']);
  assert.equal(lotPrice.parameters.additionalProperties, false);
  assert.equal(lotPrice.output.schema.oneOf.length, 3);
  const successSchema = lotPrice.output.schema.oneOf.find(branch => branch.required.includes('lotId'));
  const ambiguousSchema = lotPrice.output.schema.oneOf.find(branch => branch.required.includes('candidates'));
  const notFoundSchema = lotPrice.output.schema.oneOf.find(branch => branch.required.includes('keyword'));
  assert.equal(successSchema.additionalProperties, false);
  assert.deepEqual(successSchema.required, ['lotId', 'roomId', 'title', 'status', 'startPrice', 'currentPrice', 'minIncrement', 'capPrice', 'finalPrice', 'leadingNickname', 'bidCount', 'updatedAtUnixMs']);
  assert.equal(ambiguousSchema.additionalProperties, false);
  assert.equal(ambiguousSchema.properties.candidates.items.additionalProperties, false);
  assert.deepEqual(ambiguousSchema.properties.candidates.items.required, ['lotId', 'roomId', 'title', 'status']);
  assert.equal(notFoundSchema.additionalProperties, false);
  for (const term of ['多少钱', '当前价', '起拍价', '最低加价', '封顶价', '成交价', '谁领先', '有没有成交']) assert.match(lotPrice.description, new RegExp(term));
  assert.match(lotPrice.description, /LOT_AMBIGUOUS.*追问/);
  const exec = { agent: { session: {}, ctx }, signal: new AbortController().signal };
  await tools.get('auction_login').execute({ username: 'merchant', password: 'fixture-password' }, exec);
  const value = await dashboard.execute({}, exec);
  assert.equal(value.summary.gmvAmount, 128800);
  assert.match(value.display.summary.gmvAmount, /1,288\.00/);
  const lotValue = await lotPrice.execute({ lotId: 'lot-1' }, exec);
  assert.deepEqual(lotValue, {
    lotId: 'lot-1', roomId: 'room-1', title: '玉器', status: 'LOT_STATUS_BIDDING',
    startPrice: { amount: 10000, currency: 'CNY' }, currentPrice: { amount: 12000, currency: 'CNY' },
    minIncrement: { amount: 100, currency: 'CNY' }, capPrice: { amount: 20000, currency: 'CNY' },
    finalPrice: { amount: 12000, currency: 'CNY' }, leadingNickname: '竞拍人甲', bidCount: 3,
    updatedAtUnixMs: 1710000000123,
  });
  assert.doesNotMatch(JSON.stringify(lotPrice.output.render({}, lotValue)), /private-(leader-id|token)/);
  await assert.rejects(lotPrice.execute({ unknown: true }, exec), /参数/);
  for (const dispose of cleanup) dispose();
});

function liveApi(rooms = [{ id: 'room-b', name: 'B', status: 'ACTIVE' }, { id: 'room-a', name: 'A', status: 'ACTIVE' }]) {
  return {
    async listRooms() { return rooms; },
    async getRoomSnapshot(roomId) { return { roomId, serverTimeUnixMs: 1000, currentLot: priceLot({ roomId, status: 'LOT_STATUS_LIVE', startedAtUnixMs: 500, endsAtUnixMs: 5000, participantCount: 2 }), recentBids: [] }; },
    async getNextQueuedLot(roomId) { return priceLot({ id: 'next', roomId, queuePosition: 2, status: 'LOT_STATUS_QUEUED' }); },
  };
}

async function liveLots(args, api, signal) {
  assert.equal(typeof queries.getLiveLots, 'function');
  return queries.getLiveLots(args, { api }, signal);
}

test('live lots queries all active rooms in deterministic ID order with safe current and queued projections', async () => {
  const api = liveApi([{ id: 'room-b', name: 'B', status: 'ACTIVE' }, { id: 'disabled', name: 'X', status: 'DISABLED' }, { id: 'room-a', name: 'A', status: 'ACTIVE' }]);
  const value = await liveLots({}, api);
  assert.equal(value.status, 'OK');
  assert.deepEqual(value.rooms.map(room => room.roomId), ['room-a', 'room-b']);
  assert.deepEqual(value.rooms[0], {
    roomId: 'room-a', roomName: 'A', snapshotAtUnixMs: 1000,
    currentLot: { lotId: 'lot-maotai', title: '茅台', status: 'LOT_STATUS_LIVE', startPrice: { amount: 10000, currency: 'CNY' }, currentPrice: { amount: 12000, currency: 'CNY' }, minIncrement: { amount: 100, currency: 'CNY' }, leadingNickname: '竞拍人甲', participantCount: 2, bidCount: 3, startedAtUnixMs: 500, endsAtUnixMs: 5000, remainingMs: 4000 },
    nextLot: { lotId: 'next', title: '茅台', queuePosition: 2, startPrice: { amount: 10000, currency: 'CNY' } },
  });
  assert.doesNotMatch(JSON.stringify(value), /private-|leadingUserId|winnerUserId/);
});

test('live lots verifies IDs and resolves unique exact names case-insensitively', async () => {
  const api = liveApi([{ id: 'exact', name: 'Studio', status: 'ACTIVE' }, { id: 'other', name: 'Studio Two', status: 'ACTIVE' }]);
  assert.deepEqual((await liveLots({ roomId: 'other' }, api)).rooms.map(room => room.roomId), ['other']);
  assert.deepEqual((await liveLots({ roomName: 'studio' }, api)).rooms.map(room => room.roomId), ['exact']);
  api.getRoomSnapshot = async () => assert.fail('unverified rooms must not be fetched');
  assert.deepEqual(await liveLots({ roomId: 'unknown' }, api), { status: 'ROOM_NOT_FOUND' });
  assert.deepEqual(await liveLots({ roomName: 'unknown' }, api), { status: 'ROOM_NOT_FOUND' });
});

test('live lots reports at most five safe candidates for duplicate names', async () => {
  const api = liveApi(Array.from({ length: 7 }, (_, i) => ({ id: `room-${i}`, name: 'Main', status: 'ACTIVE', mainAccountId: 'private-account' })));
  api.getRoomSnapshot = async () => assert.fail('ambiguous rooms must not be fetched');
  const value = await liveLots({ roomName: 'main' }, api);
  assert.equal(value.status, 'ROOM_AMBIGUOUS');
  assert.equal(value.candidates.length, 5);
  assert.deepEqual(value.candidates[0], { roomId: 'room-0', name: 'Main', status: 'ACTIVE' });
});

test('live lots preserves successful rooms and safe errors on snapshot or queue failure', async () => {
  const api = liveApi();
  const original = api.getRoomSnapshot;
  api.getRoomSnapshot = async id => { if (id === 'room-b') throw new BackendUnavailableError(); return original(id); };
  let value = await liveLots({}, api);
  assert.equal(value.status, 'OK');
  assert.deepEqual(value.rooms.map(room => room.roomId), ['room-a']);
  assert.deepEqual(value.partialErrors, [{ roomId: 'room-b', code: 'BACKEND_UNAVAILABLE' }]);
  api.getRoomSnapshot = original;
  api.getNextQueuedLot = async () => { throw new BackendUnavailableError(); };
  value = await liveLots({}, api);
  assert.equal(value.status, 'OK');
  assert.equal(value.rooms.length, 2);
  assert.equal(value.rooms[0].nextLot, null);
  assert.equal(value.partialErrors.length, 2);
  assert.doesNotMatch(JSON.stringify(value), /private/);
});

test('live lots returns NO_LIVE_LOT for empty snapshots and clamps expired remaining time', async () => {
  const api = liveApi();
  api.getRoomSnapshot = async roomId => ({ roomId, serverTimeUnixMs: 1000, recentBids: [] });
  api.getNextQueuedLot = async () => null;
  const value = await liveLots({}, api);
  assert.equal(value.status, 'NO_LIVE_LOT');
  assert.equal(value.rooms.length, 2);
  assert.equal(value.rooms[0].currentLot, null);
  api.getRoomSnapshot = async roomId => ({ roomId, serverTimeUnixMs: 6000, currentLot: priceLot({ roomId, endsAtUnixMs: 5000 }), recentBids: [] });
  assert.equal((await liveLots({}, api)).rooms[0].currentLot.remainingMs, 0);
  assert.deepEqual(await liveLots({}, liveApi([])), { status: 'NO_LIVE_LOT', rooms: [], partialErrors: [] });
});

test('live lots recent bids are opt-in, capped at ten, and omit all IDs', async () => {
  const api = liveApi();
  api.getRoomSnapshot = async roomId => ({ roomId, serverTimeUnixMs: 1000, recentBids: Array.from({ length: 20 }, (_, i) => ({ id: 'private-bid', userId: 'private-user', nickname: `Alice ${i}`, amount: { amount: 500, currency: 'CNY' }, createdAtUnixMs: 900 + i })) });
  assert.equal((await liveLots({}, api)).rooms[0].recentBids, undefined);
  const value = await liveLots({ includeRecentBids: true }, api);
  assert.equal(value.rooms[0].recentBids.length, 10);
  assert.deepEqual(value.rooms[0].recentBids[0], { nickname: 'Alice 10', amount: { amount: 500, currency: 'CNY' }, createdAtUnixMs: 910 });
  assert.equal(value.rooms[0].recentBids[9].createdAtUnixMs, 919);
  assert.doesNotMatch(JSON.stringify(value), /private/);
});

test('live lots limits all HTTP fan-out to four and forwards cancellation', async () => {
  const api = liveApi(Array.from({ length: 9 }, (_, i) => ({ id: `room-${i}`, name: 'Main', status: 'ACTIVE' })));
  const controller = new AbortController();
  let active = 0; let maximum = 0;
  async function request(signal) {
    assert.equal(signal, controller.signal);
    maximum = Math.max(maximum, ++active);
    await new Promise(resolve => setTimeout(resolve, 5));
    active--;
  }
  api.getRoomSnapshot = async (roomId, signal) => { await request(signal); return { roomId, serverTimeUnixMs: 1000, recentBids: [] }; };
  api.getNextQueuedLot = async (_id, signal) => { await request(signal); return null; };
  const value = await liveLots({}, api, controller.signal);
  assert.equal(value.rooms.length, 9);
  assert.equal(maximum, 4);
  let calls = 0;
  api.getRoomSnapshot = async (_id, signal) => { calls++; controller.abort(); signal.throwIfAborted(); };
  await assert.rejects(liveLots({}, api, controller.signal), { name: 'AbortError' });
  assert.ok(calls <= 4);
  api.listRooms = async () => assert.fail('pre-aborted queries must not access the backend');
  await assert.rejects(liveLots({}, api, controller.signal), { name: 'AbortError' });
});

test('live lots rejects malformed selectors before backend access and cannot accept credentials', async () => {
  const api = liveApi();
  api.listRooms = async () => assert.fail('invalid args must not access backend');
  for (const args of [null, [], { roomId: '' }, { roomName: ' ' }, { roomId: 'x', roomName: 'y' }, { includeRecentBids: 'true' }, { password: 'private' }]) await assert.rejects(liveLots(args, api), /参数|room|includeRecentBids/);
});

test('dashboard tool keeps session cleanup in each agent scope without accumulating plugin effects', async t => {
  const user = { id: 'merchant-1', username: 'merchant', status: 'USER_STATUS_ACTIVE', permissionCodes: ['lot.view_admin'], roleCodes: ['merchant_owner'], mainAccountId: 'tenant-1' };
  const server = createServer((req, res) => {
    assert.equal(req.url, '/api/users/login');
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ result: { code: 0 }, user, tokens: { accessToken: 'private-token', accessExpiresAtUnixMs: String(Date.now() + 60000) } }));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => { server.close(resolve); server.closeAllConnections(); }));
  const pluginScope = effectScope(); const tools = new Map();
  const pluginCtx = { tools: { register: tool => tools.set(tool.name, tool) }, skills: { register: () => () => {} }, effect: pluginScope.effect };
  apply(pluginCtx, { apiBaseUrl: `http://127.0.0.1:${server.address().port}` });
  const agents = Array.from({ length: 3 }, () => ({ scope: effectScope(), session: {} }));
  const execs = agents.map(({ scope, session }) => ({ agent: { session, ctx: { effect: scope.effect } }, signal: new AbortController().signal }));
  for (const exec of execs) await tools.get('auction_login').execute({ username: 'merchant', password: 'fixture-password' }, exec);
  assert.equal(pluginScope.effectCount, 2);
  assert.deepEqual(agents.map(({ scope }) => scope.effectCount), [1, 1, 1]);
  agents[0].scope.dispose();
  assert.equal(pluginScope.effectCount, 2);
  assert.equal((await tools.get('auction_login_status').execute({}, execs[0])).authenticated, false);
  agents[0].scope.dispose();
  agents[1].scope.dispose();
  assert.equal(pluginScope.effectCount, 2);
  pluginScope.dispose();
  assert.equal(pluginScope.effectCount, 0);
  assert.equal((await tools.get('auction_login_status').execute({}, execs[2])).authenticated, false);
  agents[2].scope.dispose();
});
