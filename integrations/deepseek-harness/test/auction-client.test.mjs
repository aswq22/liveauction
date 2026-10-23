import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { AuctionAuth } from '../src/auth.mjs';
import { AuctionApi } from '../src/auction-client.mjs';
import { getLiveLots, getLotPrice } from '../src/query-tools.mjs';

const user = { id: 'merchant-1', username: 'merchant', status: 'USER_STATUS_ACTIVE', permissionCodes: ['lot.view_admin'], roleCodes: ['merchant_owner'], mainAccountId: 'tenant-1' };
const money = amount => ({ amount: String(amount), currency: 'CNY' });
const lot = (id = 'lot/1', category = 'jade') => ({ id, roomId: 'room/1', title: 'Jade', status: 'LOT_STATUS_LIVE', category, currentPrice: money(1200), rule: { startPrice: money(1000), minIncrement: money(100), capPrice: money(2000) }, finalPrice: money(1200), leadingNickname: 'Leader', updatedAtUnixMs: '1710000000123', startedAtUnixMs: '1710000000000', endsAtUnixMs: '1710000060000', queuePosition: 1, stats: { bidCount: '2', participantCount: '2' }, mainAccountId: 'tenant-1', leadingUserId: 'buyer-1', winnerUserId: 'buyer-2' });
function loginReply() { return { result: { code: 0 }, user, tokens: { accessToken: 'private-fixture-token', accessExpiresAtUnixMs: String(Date.now() + 60000) } }; }
async function fixture(t, handler) {
  const server = createServer(async (req, res) => { res.setHeader('Content-Type', 'application/json'); await handler(req, res); });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => { server.close(resolve); server.closeAllConnections(); }));
  return `http://127.0.0.1:${server.address().port}`;
}

test('auction api consumes protojson replies with safe money, int64 and repeated sections', async t => {
  const origin = await fixture(t, (req, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(loginReply()));
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    if (req.url === '/api/admin/dashboard?range=today&sections=topLots&sections=categoryPerformance') return res.end(JSON.stringify({ result: { code: 0, message: 'untrusted success message' }, currency: 'CNY', snapshotAtUnixMs: '1710000000000', range: { range: 'today', timezone: 'Asia/Shanghai', startUnixMs: '1709913600000', endUnixMs: '1710000000000' }, summary: { gmvAmount: '1200', paidAmount: '1000', pendingAmount: '200', orderCount: '2', cancelledOrderCount: '1', participantCount: '2', averageDealAmount: '500', paymentRate: { numerator: '1', denominator: '2', rate: 0.5 }, dealRate: { numerator: '1', denominator: '1', rate: 1 } }, funnel: { createdLotCount: '2', listedLotCount: '2', startedLotCount: '1', bidLotCount: '1', settledLotCount: '1', paidLotCount: '1' }, trend: [{ startUnixMs: '1709913600000', endUnixMs: '1710000000000', paidAmount: '1000', paidOrders: '1', bidders: '2' }], topLots: [{ lotId: 'lot/1', title: 'Jade', category: 'jade', paidAmount: '1000', finalAmount: '1200', bidCount: '2', participantCount: '2' }], categories: [{ category: 'jade', paidAmount: '1000', startedLotCount: '1', settledLotCount: '1', dealRate: { numerator: '1', denominator: '1', rate: 1 } }], orderRisks: { pendingPaymentCount: '1', expiringSoonCount: '0', expiredCount: '0', paymentFailedCount: '0' }, lotPerformance: [{ lotId: 'lot/1', title: 'Jade', category: 'jade', status: 'LOT_STATUS_SETTLED', bidCount: '2', participantCount: '2', finalAmount: '1200', paidAmount: '1000', conversionRate: { numerator: '1', denominator: '1', rate: 1 } }], partialErrors: [{ section: 'trend', code: 'timeout', message: 'private backend detail' }] }));
    if (req.url === '/api/admin/lots/lot%2F1') return res.end(JSON.stringify({ result: { code: 0 }, lot: lot('lot/1', '') }));
    if (req.url === '/api/admin/lots?keyword=jade+%26+gold&roomId=room%2F1&pageSize=5') return res.end(JSON.stringify({ result: { code: 0 }, lots: [lot('lot/1', ''), lot('lot-2')], total: '2', page: 1, pageSize: 5 }));
    if (req.url === '/api/rooms/room%2F1/snapshot') return res.end(JSON.stringify({ result: { code: 0 }, snapshot: { roomId: 'room/1', serverTimeUnixMs: '1710000000000', currentLot: lot('lot/1', ''), recentBids: Array.from({ length: 12 }, (_, i) => ({ id: `bid-${i}`, amount: money(1000 + i), createdAtUnixMs: String(1710000000000 + i), userId: `buyer-${i}`, phone: '13800000000' })) } }));
    if (req.url === '/api/admin/rooms') return res.end(JSON.stringify({ result: { code: 0 }, rooms: [{ id: 'room/1', name: 'Main', platform: 'douyin', platformRoomId: 'private', status: 'ROOM_STATUS_LIVE', liveStartedAtUnixMs: '1710000000000', mainAccountId: 'tenant-1', createdByUserId: 'merchant-1' }] }));
    throw new Error(`unexpected ${req.url}`);
  });
  const auth = new AuctionAuth(origin); await auth.login({ username: 'merchant', password: 'fixture-password' }); const api = new AuctionApi(auth.authorizedRequest.bind(auth));
  const dashboard = await api.getDashboard({ sections: ['topLots', 'categoryPerformance'] });
  assert.deepEqual(dashboard.partialErrors, [{ section: 'trend', code: 'timeout' }]); assert.equal(dashboard.summary.gmvAmount, 1200); assert.equal(dashboard.categoryPerformance[0].paidAmount, 1000); assert.doesNotMatch(JSON.stringify(dashboard), /message|private/);
  assert.deepEqual((await api.searchLots({ lotId: 'lot/1' })).lots, [{ id: 'lot/1', roomId: 'room/1', title: 'Jade', status: 'LOT_STATUS_LIVE', category: '', currentPrice: { amount: 1200, currency: 'CNY' }, startPrice: { amount: 1000, currency: 'CNY' }, minIncrement: { amount: 100, currency: 'CNY' }, capPrice: { amount: 2000, currency: 'CNY' }, finalPrice: { amount: 1200, currency: 'CNY' }, leadingNickname: 'Leader', updatedAtUnixMs: 1710000000123, startedAtUnixMs: 1710000000000, endsAtUnixMs: 1710000060000, queuePosition: 1, bidCount: 2, participantCount: 2 }]);
  assert.equal((await api.searchLots({ keyword: 'jade & gold', roomId: 'room/1', pageSize: 99 })).lots[0].category, ''); assert.equal((await api.getRoomSnapshot('room/1')).currentLot.category, ''); assert.equal((await api.getRoomSnapshot('room/1')).recentBids.length, 10);
  assert.deepEqual(await api.listRooms(), [{ id: 'room/1', name: 'Main', platform: 'douyin', status: 'ROOM_STATUS_LIVE', liveStartedAtUnixMs: 1710000000000 }]);
});

test('empty sections are omitted and malformed protojson fails closed', async () => {
  const seen = []; const api = new AuctionApi(async path => { seen.push(path); return { result: { code: 0 }, currency: 'CNY', snapshotAtUnixMs: 'not-an-int', range: {}, summary: {}, funnel: {}, trend: [], topLots: [], categories: [], orderRisks: {}, lotPerformance: [], partialErrors: [] }; });
  await assert.rejects(api.getDashboard(), /无效/); assert.deepEqual(seen, ['/api/admin/dashboard?range=today']);
});

test('auction api represents absent optional lot price fields explicitly without exposing identities', async () => {
  const api = new AuctionApi(async () => ({ result: { code: 0 }, lot: {
    id: 'lot-optional', roomId: 'room-1', title: 'Optional', status: 'LOT_STATUS_QUEUED', category: '',
    currentPrice: money(1000), rule: { startPrice: money(1000), minIncrement: money(100) },
    updatedAtUnixMs: '1710000000999', startedAtUnixMs: '0', endsAtUnixMs: '0', queuePosition: 1,
    stats: { bidCount: '0', participantCount: '0' }, leadingUserId: 'private-leader', winnerUserId: 'private-winner',
  } }));
  const value = await api.getLot('lot-optional');
  assert.equal(value.capPrice, null);
  assert.equal(value.finalPrice, null);
  assert.equal(value.leadingNickname, '');
  assert.equal(value.updatedAtUnixMs, 1710000000999);
  assert.doesNotMatch(JSON.stringify(value), /private-/);
  const price = await getLotPrice({ lotId: 'lot-optional' }, { api });
  assert.equal(JSON.parse(JSON.stringify(price)).capPrice, null);
  assert.equal(price.finalPrice, null);
});

test('lot price never resolves an exact or fuzzy singleton when pagination hides matches', async () => {
  for (const [count, title] of [[5, 'Jade'], [1, 'Jade'], [1, 'Jade bracelet']]) {
    let detailCalls = 0;
    const api = new AuctionApi(async path => {
      if (path.startsWith('/api/admin/lots?')) return { result: { code: 0 }, lots: Array.from({ length: count }, (_, i) => ({ ...lot(`lot-${i}`), title: i === 0 ? title : `Other ${i}` })), total: '6', page: 1, pageSize: 5 };
      detailCalls++;
      return { result: { code: 0 }, lot: lot('lot-0') };
    });
    const value = await getLotPrice({ keyword: 'Jade' }, { api });
    assert.equal(value.status, 'LOT_AMBIGUOUS');
    assert.equal(value.candidates.length, count);
    assert.equal(detailCalls, 0);
  }
});

test('direct ID price requires the authenticated admin detail route and an exact result ID', async () => {
  let wrongId = false;
  const api = new AuctionApi(async path => {
    assert.equal(path, '/api/admin/lots/lot%2F1');
    return { result: { code: 0 }, lot: lot(wrongId ? 'different-lot' : 'lot/1') };
  });
  assert.equal((await getLotPrice({ lotId: 'lot/1' }, { api })).lotId, 'lot/1');
  wrongId = true;
  await assert.rejects(getLotPrice({ lotId: 'lot/1' }, { api }), /无效/);
});

test('live lots client preserves up to 100 rooms and rejects overflow instead of truncating', async () => {
  let count = 100;
  const api = new AuctionApi(async () => ({ result: { code: 0 }, rooms: Array.from({ length: count }, (_, i) => ({ id: `room-${i}`, name: 'Main', platform: 'douyin', status: 'ACTIVE', liveStartedAtUnixMs: '0' })) }));
  assert.equal((await api.listRooms()).length, 100);
  count = 101;
  await assert.rejects(api.listRooms(), /无效|安全/);
});

test('live lots client queries the first queued lot with the HTTP enum and projects private bids away', async () => {
  const signal = new AbortController().signal;
  const api = new AuctionApi(async (path, options) => {
    assert.equal(options.signal, signal);
    if (path.includes('/snapshot')) return { result: { code: 0 }, snapshot: { roomId: 'room/1', serverTimeUnixMs: '1710000000000', recentBids: [{ id: 'private-bid', userId: 'private-user', nickname: 'Alice', amount: money(1250), createdAtUnixMs: '1710000000001' }] } };
    assert.equal(path, '/api/admin/lots?roomId=room%2F1&status=LOT_STATUS_QUEUED&page=1&pageSize=1');
    return { result: { code: 0 }, lots: [{ ...lot('next'), status: 'LOT_STATUS_QUEUED' }], total: '1', page: 1, pageSize: 1 };
  });
  const snapshot = await api.getRoomSnapshot('room/1', signal);
  assert.deepEqual(snapshot.recentBids, [{ nickname: 'Alice', amount: { amount: 1250, currency: 'CNY' }, createdAtUnixMs: 1710000000001 }]);
  assert.equal(typeof api.getNextQueuedLot, 'function');
  assert.equal((await api.getNextQueuedLot('room/1', signal)).id, 'next');
});

test('live lots client returns null for an empty queue and rejects an unscoped queue result', async () => {
  let lots = [];
  const api = new AuctionApi(async () => ({ result: { code: 0 }, lots, total: '0', page: 1, pageSize: 1 }));
  assert.equal(typeof api.getNextQueuedLot, 'function');
  assert.equal(await api.getNextQueuedLot('room/1'), null);
  lots = [{ ...lot(), roomId: 'other-room', status: 'LOT_STATUS_QUEUED' }];
  await assert.rejects(api.getNextQueuedLot('room/1'), /无效/);
});

async function liveHttpApi(t, { snapshotStatus = 200, queueStatus = 200, queueCode = 403001 } = {}) {
  const origin = await fixture(t, (req, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(loginReply()));
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    if (req.url === '/api/admin/rooms') return res.end(JSON.stringify({ result: { code: 0 }, rooms: [{ id: 'room/1', name: 'Main', platform: 'douyin', status: 'ACTIVE', liveStartedAtUnixMs: '0' }] }));
    const isSnapshot = req.url === '/api/rooms/room%2F1/snapshot';
    if (!isSnapshot) assert.equal(req.url, '/api/admin/lots?roomId=room%2F1&status=LOT_STATUS_QUEUED&page=1&pageSize=1');
    res.statusCode = isSnapshot ? snapshotStatus : queueStatus;
    if (res.statusCode !== 200) return res.end(JSON.stringify({ result: { code: res.statusCode === 403 ? queueCode : 500001, message: 'private-backend-message' } }));
    res.end(JSON.stringify(isSnapshot ? { result: { code: 0 }, snapshot: { roomId: 'room/1', serverTimeUnixMs: '1710000000050', currentLot: lot(), recentBids: Array.from({ length: 20 }, (_, i) => ({ id: `private-bid-${i}`, userId: `private-user-${i}`, nickname: `Bidder ${i}`, amount: money(1000 + i), createdAtUnixMs: String(1710000000000 + i) })) } } : { result: { code: 0 }, lots: [], total: '0', page: 1, pageSize: 1 }));
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'fixture-password' });
  return { auth, api: new AuctionApi(auth.authorizedRequest.bind(auth)) };
}

test('live lots keeps the latest ten of twenty ascending bids through HTTP auth client and handler', async t => {
  const { api } = await liveHttpApi(t);
  const value = await getLiveLots({ includeRecentBids: true }, { api });
  const bids = value.rooms[0].recentBids;
  assert.equal(bids.length, 10);
  assert.deepEqual(bids.map(bid => bid.nickname), ['Bidder 10', 'Bidder 11', 'Bidder 12', 'Bidder 13', 'Bidder 14', 'Bidder 15', 'Bidder 16', 'Bidder 17', 'Bidder 18', 'Bidder 19']);
  assert.deepEqual(bids[0], { nickname: 'Bidder 10', amount: { amount: 1010, currency: 'CNY' }, createdAtUnixMs: 1710000000010 });
  assert.doesNotMatch(JSON.stringify(value), /private-/);
});

test('live lots snapshot HTTP 401 preserves re-login semantics and clears the session', async t => {
  const { api, auth } = await liveHttpApi(t, { snapshotStatus: 401 });
  await assert.rejects(getLiveLots({}, { api }), error => {
    assert.match(error.message, /重新登录/);
    assert.doesNotMatch(error.message, /private-/);
    return true;
  });
  assert.equal((await auth.status()).authenticated, false);
});

test('live lots queued HTTP 403 preserves safe permission denial', async t => {
  const { api } = await liveHttpApi(t, { queueStatus: 403 });
  await assert.rejects(getLiveLots({}, { api }), error => {
    assert.match(error.message, /权限/);
    assert.doesNotMatch(error.message, /private-/);
    return true;
  });
});

test('live lots HTTP 503 remains a partial backend failure while preserving current lot', async t => {
  const { api } = await liveHttpApi(t, { queueStatus: 503 });
  const value = await getLiveLots({}, { api });
  assert.equal(value.rooms[0].currentLot.lotId, 'lot/1');
  assert.deepEqual(value.partialErrors, [{ roomId: 'room/1', code: 'BACKEND_UNAVAILABLE' }]);
  assert.doesNotMatch(JSON.stringify(value), /private-/);
});

test('live lots queued HTTP 403 without a business code still requires permission', async t => {
  const { api } = await liveHttpApi(t, { queueStatus: 403, queueCode: 0 });
  await assert.rejects(getLiveLots({}, { api }), /权限/);
});

async function malformedLiveHttpApi(t, malformed) {
  const origin = await fixture(t, (req, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(loginReply()));
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    if (req.url === '/api/admin/rooms') return res.end(JSON.stringify({ result: { code: 0 }, rooms: ['room-a', 'room-b'].map(id => ({ id, name: id, platform: 'douyin', status: 'ACTIVE', liveStartedAtUnixMs: '0' })) }));
    const url = new URL(req.url, 'http://localhost');
    const isSnapshot = url.pathname.endsWith('/snapshot');
    const roomId = isSnapshot ? url.pathname.split('/')[3] : url.searchParams.get('roomId');
    assert.ok(['room-a', 'room-b'].includes(roomId));
    if (roomId === 'room-b' && isSnapshot && malformed === 'json') return res.end('{"private-parse-details":');
    if (roomId === 'room-b' && !isSnapshot && malformed === 'lots') return res.end(JSON.stringify({ result: { code: 0, message: 'private-backend-message' }, lots: { invalid: 'private-payload' } }));
    const snapshot = { roomId, serverTimeUnixMs: '1710000000050', currentLot: { ...lot(`lot-${roomId}`), roomId }, recentBids: [] };
    if (roomId === 'room-b' && malformed === 'tenant') snapshot.currentLot.mainAccountId = 'other-tenant';
    res.end(JSON.stringify(isSnapshot ? { result: { code: 0 }, snapshot } : { result: { code: 0 }, lots: [], total: '0', page: 1, pageSize: 1 }));
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'fixture-password' });
  return new AuctionApi(auth.authorizedRequest.bind(auth));
}

test('live lots isolates malformed snapshot JSON and preserves other successful rooms through HTTP', async t => {
  const api = await malformedLiveHttpApi(t, 'json');
  const value = await getLiveLots({}, { api });
  assert.equal(value.status, 'OK');
  assert.deepEqual(value.rooms.map(room => room.roomId), ['room-a']);
  assert.equal(value.rooms[0].currentLot.lotId, 'lot-room-a');
  assert.deepEqual(value.partialErrors, [{ roomId: 'room-b', code: 'BACKEND_UNAVAILABLE' }]);
  assert.doesNotMatch(JSON.stringify(value), /private-|SyntaxError|JSON|parse/i);
});

test('live lots isolates malformed queue lots and retains both current snapshots through HTTP', async t => {
  const api = await malformedLiveHttpApi(t, 'lots');
  const value = await getLiveLots({}, { api });
  assert.equal(value.status, 'OK');
  assert.deepEqual(value.rooms.map(room => room.roomId), ['room-a', 'room-b']);
  assert.equal(value.rooms[1].currentLot.lotId, 'lot-room-b');
  assert.equal(value.rooms[1].nextLot, null);
  assert.deepEqual(value.partialErrors, [{ roomId: 'room-b', code: 'BACKEND_UNAVAILABLE' }]);
  assert.doesNotMatch(JSON.stringify(value), /private-|SyntaxError|JSON|parse/i);
});

test('live lots still propagates tenant safety errors instead of treating them as malformed data', async t => {
  const api = await malformedLiveHttpApi(t, 'tenant');
  await assert.rejects(getLiveLots({}, { api }), /其他商家空间/);
});
