import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import * as auctionPlugin from '../src/plugin.mjs';
import { dashboardReply } from '../test/fixtures/dashboard.mjs';

// Run from the Harness root with its tsx loader and tsconfig source aliases.
const root = new URL('../../../../deepseek-harness/', import.meta.url);
const { Context } = await import(new URL('vendor/cordis/src/index.ts', root));
const { default: SystemPrompt } = await import(new URL('packages/core/system-prompt/src/index.ts', root));
const { default: ToolRuntime } = await import(new URL('packages/core/tools/src/index.ts', root));
const { default: SkillRegistry } = await import(new URL('packages/skill/skill/src/index.ts', root));
const user = { id: 'smoke-user', username: 'smoke-merchant', mainAccountId: 'smoke-user', status: 'USER_STATUS_ACTIVE', permissionCodes: ['lot.view_admin', 'team.user.create'], roleCodes: ['merchant_owner'] };
const member = { id: 'smoke-member', username: 'smoke-operator', nickname: '冒烟运营', mainAccountId: 'smoke-user', createdByUserId: 'smoke-user', status: 'USER_STATUS_ACTIVE', permissionCodes: ['lot.view_admin'], roleCodes: ['operator'], createdAtUnixMs: Date.now(), updatedAtUnixMs: Date.now() };
let loginCalls = 0;
let meCalls = 0;
let createCalls = 0;
let dashboardCalls = 0;
let lotPriceCalls = 0;
let liveLotsCalls = 0;
let searchCalls = 0;
let requestCount = 0;
let duplicateRoomNames = false;
const queryPaths = new Set();
const privateFields = { buyerId: 'private-buyer-id', phone: 'private-phone', address: 'private-address', password: 'fixture-password', accessToken: 'private-fixture-token' };
let liveErrorMode;
let uncappedPrice = false;
let hiddenPriceMatches = false;
const liveLot = { id: 'live-smoke', roomId: 'room-smoke', title: '直播拍品', status: 'LOT_STATUS_LIVE', category: '', currentPrice: { amount: '12800', currency: 'CNY' }, rule: { startPrice: { amount: '10000', currency: 'CNY' }, minIncrement: { amount: '100', currency: 'CNY' } }, leadingNickname: '领先者', updatedAtUnixMs: '1710000000000', startedAtUnixMs: '1709999999000', endsAtUnixMs: '1710000006000', queuePosition: 1, stats: { bidCount: '3', participantCount: '2' }, leadingUserId: 'private-leader-id' };
const server = createServer(async (req, res) => {
  requestCount++;
  let body = '';
  for await (const chunk of req) body += chunk;
  res.setHeader('Content-Type', 'application/json');
  if (req.url !== '/api/users/login') {
    assert.match(req.headers.authorization ?? '', /^Bearer (private-fixture-token|portfolio-token-[ab])$/, 'every authenticated backend request must carry an expected isolated bearer token');
    queryPaths.add(req.url.split('?')[0]);
  }
  // Poison all response envelopes to verify the plugin's public projection.
  const end = res.end.bind(res);
  res.end = payload => {
    const value = JSON.parse(payload);
    if (uncappedPrice && value.lot) { delete value.lot.rule.capPrice; delete value.lot.finalPrice; }
    if (hiddenPriceMatches && value.lots) { value.lots = value.lots.slice(0, 1); value.total = '6'; }
    return end(JSON.stringify({ ...value, ...privateFields }));
  };
  if (req.url === '/api/users/login') {
    loginCalls++;
    const credentials = JSON.parse(body);
    if (credentials.username === 'smoke-merchant') {
      assert.deepEqual(credentials, { username: 'smoke-merchant', password: 'fixture-password' });
      res.end(JSON.stringify({ user, tokens: { accessToken: 'private-fixture-token', accessExpiresAtUnixMs: Date.now() + 60000 } }));
    } else {
      const suffix = credentials.username === 'portfolio-a' ? 'a' : credentials.username === 'portfolio-b' ? 'b' : '';
      assert.ok(suffix, 'only configured portfolio fixture accounts may log in');
      assert.equal(credentials.password, `portfolio-password-${suffix}`);
      res.end(JSON.stringify({
        user: {
          ...user, id: `portfolio-owner-${suffix}`, username: credentials.username,
          mainAccountId: `portfolio-tenant-${suffix}`,
          permissionCodes: ['lot.view_admin', 'order.manage', 'realtime.view'],
        },
        tokens: { accessToken: `portfolio-token-${suffix}`, accessExpiresAtUnixMs: Date.now() + 60000 },
      }));
    }
  } else if (req.url === '/api/users/me') {
    meCalls++;
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    res.end(JSON.stringify({ user }));
  } else if (req.url === '/api/admin/dashboard?range=live') {
    dashboardCalls++;
    res.end(JSON.stringify(dashboardReply()));
  } else if (req.url?.startsWith('/api/admin/dashboard?range=today')) {
    dashboardCalls++;
    if (req.url === '/api/admin/dashboard?range=today') assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    else assert.match(req.headers.authorization, /^Bearer portfolio-token-[ab]$/);
    res.end(JSON.stringify({ result: { code: 0 }, currency: 'CNY', snapshotAtUnixMs: '1710000000000', range: { range: 'today', timezone: 'Asia/Shanghai', startUnixMs: '1709913600000', endUnixMs: '1710000000000' }, summary: { gmvAmount: '1200', paidAmount: '1000', pendingAmount: '200', orderCount: '2', cancelledOrderCount: '1', participantCount: '2', averageDealAmount: '500', paymentRate: { numerator: '1', denominator: '2', rate: 0.5 }, dealRate: { numerator: '1', denominator: '1', rate: 1 } }, funnel: { createdLotCount: '2', listedLotCount: '2', startedLotCount: '1', bidLotCount: '1', settledLotCount: '1', paidLotCount: '1' }, trend: [], topLots: [], categories: [], orderRisks: { pendingPaymentCount: '0', expiringSoonCount: '0', expiredCount: '0', paymentFailedCount: '0' }, lotPerformance: [], partialErrors: [] }));
  } else if (req.url === '/api/admin/lots?keyword=%E8%8C%85%E5%8F%B0&roomId=&pageSize=5') {
    searchCalls++;
    res.end(JSON.stringify({ result: { code: 0 }, lots: [
      { ...liveLot, ...privateFields, id: 'search-one', title: '茅台' },
      { ...liveLot, ...privateFields, id: 'search-two', title: '茅台' },
    ], total: '2', page: 1, pageSize: 5 }));
  } else if (req.url === '/api/admin/lots/lot-smoke') {
    lotPriceCalls++;
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    res.end(JSON.stringify({ result: { code: 0 }, lot: { id: 'lot-smoke', roomId: 'room-smoke', title: '冒烟拍品', status: 'LOT_STATUS_BIDDING', category: 'test', currentPrice: { amount: '12000', currency: 'CNY' }, rule: { startPrice: { amount: '10000', currency: 'CNY' }, minIncrement: { amount: '100', currency: 'CNY' }, capPrice: { amount: '20000', currency: 'CNY' } }, finalPrice: { amount: '12000', currency: 'CNY' }, leadingNickname: '领先者', updatedAtUnixMs: '1710000000123', startedAtUnixMs: '1710000000000', endsAtUnixMs: '1710003600000', queuePosition: 1, stats: { bidCount: '3', participantCount: '2' }, leadingUserId: 'private-leader-id', winnerUserId: 'private-winner-id' } }));
  } else if (req.url === '/api/admin/rooms') {
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    const room = { id: 'room-smoke', name: duplicateRoomNames ? '一号直播间' : '冒烟直播间', status: 'ACTIVE', platform: 'douyin', liveStartedAtUnixMs: '1709999999000' };
    res.end(JSON.stringify({ result: { code: 0 }, rooms: duplicateRoomNames ? [room, { ...room, id: 'room-other', ...privateFields }] : [room] }));
  } else if (req.url === '/api/rooms/room-smoke/snapshot') {
    liveLotsCalls++;
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    if (liveErrorMode === 'snapshot-401') {
      res.statusCode = 401;
      res.end(JSON.stringify({ result: { code: 401001, message: 'private-backend-message' } }));
      return;
    }
    res.end(JSON.stringify({ result: { code: 0 }, snapshot: { roomId: 'room-smoke', serverTimeUnixMs: '1710000000000', currentLot: liveLot, recentBids: [{ id: 'private-bid-id', userId: 'private-user-id', nickname: '领先者', amount: { amount: '12800', currency: 'CNY' }, createdAtUnixMs: '1709999999999' }] } }));
  } else if (req.url === '/api/admin/lots?roomId=room-smoke&status=LOT_STATUS_QUEUED&page=1&pageSize=1') {
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    if (liveErrorMode === 'queue-403') {
      res.statusCode = 403;
      res.end(JSON.stringify({ result: { code: 403001, message: 'private-backend-message' } }));
      return;
    }
    res.end(JSON.stringify({ result: { code: 0 }, lots: [{ ...liveLot, id: 'next-smoke', title: '下一件', status: 'LOT_STATUS_QUEUED', queuePosition: 2 }], total: '1', page: 1, pageSize: 1 }));
  } else {
    createCalls++;
    assert.equal(req.url, '/api/admin/users');
    assert.equal(req.headers.authorization, 'Bearer private-fixture-token');
    assert.deepEqual(JSON.parse(body), { username: 'smoke-operator', nickname: '冒烟运营', password: 'fixture-member-password', roleCode: 'operator' });
    res.end(JSON.stringify({ user: member }));
  }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const ctx = new Context();
try {
  await ctx.plugin(SystemPrompt);
  await ctx.plugin(ToolRuntime);
  await ctx.plugin(SkillRegistry);
  const fiber = await ctx.plugin(auctionPlugin, { apiBaseUrl: `http://127.0.0.1:${server.address().port}` });
  assert.deepEqual(ctx.tools.schemas().map(tool => tool.name).sort(), ['auction_create_team_member', 'auction_get_dashboard', 'auction_get_live_lots', 'auction_get_lot_price', 'auction_get_multi_account_report', 'auction_login', 'auction_login_status']);
  const skills = await ctx.skills.list();
  assert.deepEqual(skills.map(skill => skill.name), ['analyzing-auction-portfolio']);
  assert.equal(skills[0].source, 'runtime');
  assert.equal(skills[0].invocation.modelInvocable, true);
  const portfolioSkill = await ctx.skills.get('analyzing-auction-portfolio');
  assert.match(portfolioSkill.content, /auction_get_multi_account_report/);
  const agent = { id: 'smoke-a', session: {}, ctx };
  const other = { id: 'smoke-b', session: {}, ctx };
  let id = 0;
  async function call(name, args, owner = agent) {
    const result = await ctx.tools.execute({ name, arguments: args, agent: owner, callId: `smoke-${++id}`, signal: new AbortController().signal });
    const rendered = result.isError ? [] : ctx.tools.get(name).output.render(args, result.value);
    assert.equal(/private-|fixture-password|fixture-member-password/.test(JSON.stringify({ result, rendered })), false, `${name} must not expose fixture secrets or private customer fields`);
    return result;
  }
  const login = await call('auction_login', { username: 'smoke-merchant', password: 'fixture-password' });
  assert.equal(login.isError, false, JSON.stringify(login));
  assert.equal(login.value.authenticated, true);
  assert.doesNotMatch(JSON.stringify(login), /private-fixture-token|fixture-password/);
  assert.equal((await call('auction_login_status', {})).value.user.username, 'smoke-merchant');
  assert.equal((await call('auction_login_status', {}, other)).value.authenticated, false);
  const dashboard = await call('auction_get_dashboard', {});
  assert.equal(dashboard.isError, false, JSON.stringify(dashboard));
  assert.equal(dashboard.value.summary.gmvAmount, 1200);
  assert.equal(dashboard.value.range.timezone, 'Asia/Shanghai');
  assert.equal(dashboard.value.display.summary.gmvAmount, '¥12.00');
  const maxDashboard = await call('auction_get_dashboard', { range: 'live' });
  assert.equal(maxDashboard.isError, false, JSON.stringify(maxDashboard));
  assert.equal(maxDashboard.value.trend.length, 720);
  assert.equal(maxDashboard.value.topLots[9].lotId, 'top-9');
  assert.equal(maxDashboard.value.categoryPerformance.length, 1000);
  assert.equal(maxDashboard.value.lotPerformance[99].lotId, 'lot-99');
  const portfolio = await call('auction_get_multi_account_report', {
    accounts: [
      { username: 'portfolio-a', password: 'portfolio-password-a', alias: '组合A' },
      { username: 'portfolio-b', password: 'portfolio-password-b', alias: '组合B' },
    ],
    range: 'today', sections: ['summary'],
  });
  assert.equal(portfolio.isError, false, JSON.stringify(portfolio));
  assert.equal(portfolio.value.status, 'OK');
  assert.deepEqual(portfolio.value.coverage, { requested: 2, authenticated: 2, included: 2, failed: 0 });
  assert.equal(portfolio.value.summary.paidAmount, 2000);
  assert.doesNotMatch(JSON.stringify(portfolio), /portfolio-password|portfolio-token/);
  assert.equal((await call('auction_login_status', {})).value.user.username, 'smoke-merchant');
  const ambiguousPrice = await call('auction_get_lot_price', { keyword: '茅台' });
  assert.equal(ambiguousPrice.isError, false, 'keyword search must execute through the authenticated backend');
  assert.deepEqual(ambiguousPrice.value, { status: 'LOT_AMBIGUOUS', candidates: [
    { lotId: 'search-one', roomId: 'room-smoke', title: '茅台', status: 'LOT_STATUS_LIVE' },
    { lotId: 'search-two', roomId: 'room-smoke', title: '茅台', status: 'LOT_STATUS_LIVE' },
  ] });
  hiddenPriceMatches = true;
  const hiddenMatchPrice = await call('auction_get_lot_price', { keyword: '茅台' });
  assert.equal(hiddenMatchPrice.isError, false, JSON.stringify(hiddenMatchPrice));
  assert.equal(hiddenMatchPrice.value.status, 'LOT_AMBIGUOUS');
  assert.equal(hiddenMatchPrice.value.candidates.length, 1);
  hiddenPriceMatches = false;
  const lotPrice = await call('auction_get_lot_price', { lotId: 'lot-smoke' });
  assert.equal(lotPrice.isError, false, JSON.stringify(lotPrice));
  assert.deepEqual(lotPrice.value, {
    lotId: 'lot-smoke', roomId: 'room-smoke', title: '冒烟拍品', status: 'LOT_STATUS_BIDDING',
    startPrice: { amount: 10000, currency: 'CNY' }, currentPrice: { amount: 12000, currency: 'CNY' },
    minIncrement: { amount: 100, currency: 'CNY' }, capPrice: { amount: 20000, currency: 'CNY' },
    finalPrice: { amount: 12000, currency: 'CNY' }, leadingNickname: '领先者', bidCount: 3,
    updatedAtUnixMs: 1710000000123,
  });
  const priceDefinition = ctx.tools.get('auction_get_lot_price');
  assert.deepEqual(priceDefinition.output.render({}, lotPrice.value), [{ type: 'text', text: JSON.stringify(lotPrice.value) }]);
  assert.doesNotMatch(JSON.stringify(lotPrice), /private-(leader-id|winner-id)|private-fixture-token|fixture-password/);
  uncappedPrice = true;
  const uncapped = await call('auction_get_lot_price', { lotId: 'lot-smoke' });
  assert.equal(uncapped.isError, false, JSON.stringify(uncapped));
  assert.equal(uncapped.value.capPrice, null);
  assert.equal(uncapped.value.finalPrice, null);
  uncappedPrice = false;
  const liveLots = await call('auction_get_live_lots', { includeRecentBids: true });
  assert.equal(liveLots.isError, false, JSON.stringify(liveLots));
  assert.equal(liveLots.value.status, 'OK');
  assert.equal(liveLots.value.rooms[0].currentLot.currentPrice.amount, 12800);
  assert.equal(liveLots.value.rooms[0].currentLot.remainingMs, 6000);
  assert.equal(liveLots.value.rooms[0].nextLot.lotId, 'next-smoke');
  assert.deepEqual(liveLots.value.rooms[0].recentBids, [{ nickname: '领先者', amount: { amount: 12800, currency: 'CNY' }, createdAtUnixMs: 1709999999999 }]);
  assert.doesNotMatch(JSON.stringify(liveLots), /private-/);
  assert.equal((await call('auction_get_live_lots', { password: 'private-password' })).isError, true);
  const requestsBeforeIsolation = requestCount;
  assert.equal((await call('auction_get_live_lots', {}, other)).isError, true);
  for (const [name, args] of [
    ['auction_get_dashboard', {}], ['auction_get_lot_price', { lotId: 'lot-smoke' }],
    ['auction_create_team_member', { mainAccountUsername: 'smoke-merchant', username: 'smoke-operator', nickname: '冒烟运营', password: 'fixture-member-password', roleCode: 'operator' }],
  ]) assert.equal((await call(name, args, other)).isError, true, `${name} must not reuse another session's login`);
  assert.equal(requestCount, requestsBeforeIsolation, 'unauthenticated sessions must not reach the backend');
  const created = await call('auction_create_team_member', { mainAccountUsername: 'smoke-merchant', username: 'smoke-operator', nickname: '冒烟运营', password: 'fixture-member-password', roleCode: 'operator' });
  assert.equal(created.isError, false, JSON.stringify(created));
  assert.equal(created.value.user.username, 'smoke-operator');
  assert.doesNotMatch(JSON.stringify(created), /fixture-member-password|private-fixture-token/);
  assert.equal((await call('auction_create_team_member', { mainAccountUsername: 'other-owner', username: 'smoke-operator', nickname: '冒烟运营', password: 'fixture-member-password', roleCode: 'operator' })).isError, true);
  assert.equal((await call('auction_login', { username: 'missing-password' })).isError, true);
  assert.equal(loginCalls, 3);
  assert.equal(meCalls, 2);
  assert.equal(createCalls, 1);
  assert.equal(dashboardCalls, 4);
  assert.equal(lotPriceCalls, 2);
  assert.equal(liveLotsCalls, 1);
  assert.equal(searchCalls, 2);
  assert.deepEqual([...queryPaths].sort(), ['/api/admin/dashboard', '/api/admin/lots', '/api/admin/lots/lot-smoke', '/api/admin/rooms', '/api/admin/users', '/api/rooms/room-smoke/snapshot', '/api/users/me']);
  duplicateRoomNames = true;
  const ambiguousRoom = await call('auction_get_live_lots', { roomName: '一号直播间' });
  assert.equal(ambiguousRoom.isError, false);
  assert.equal(ambiguousRoom.value.status, 'ROOM_AMBIGUOUS');
  assert.deepEqual(ambiguousRoom.value.candidates.map(room => room.roomId), ['room-other', 'room-smoke']);
  assert.equal(liveLotsCalls, 1, 'ambiguous rooms must not be guessed or queried');
  duplicateRoomNames = false;
  liveErrorMode = 'queue-403';
  const forbiddenLiveLots = await call('auction_get_live_lots', {});
  assert.equal(forbiddenLiveLots.isError, true, JSON.stringify(forbiddenLiveLots));
  assert.match(forbiddenLiveLots.error.message, /权限/);
  assert.equal(forbiddenLiveLots.value, undefined);
  assert.doesNotMatch(JSON.stringify(forbiddenLiveLots), /private-|BACKEND_UNAVAILABLE|invalid.*output/i);
  liveErrorMode = 'snapshot-401';
  const expiredLiveLots = await call('auction_get_live_lots', {});
  assert.equal(expiredLiveLots.isError, true, JSON.stringify(expiredLiveLots));
  assert.match(expiredLiveLots.error.message, /重新登录/);
  assert.equal(expiredLiveLots.value, undefined);
  assert.doesNotMatch(JSON.stringify(expiredLiveLots), /private-|BACKEND_UNAVAILABLE|invalid.*output/i);
  assert.equal((await call('auction_login_status', {})).value.authenticated, false);
  await fiber.dispose();
  assert.equal(ctx.tools.schemas().length, 0);
  assert.equal((await ctx.skills.list()).length, 0);
  console.log('PASS: real Cordis registration, runtime skill discovery, strict schemas, login, dashboard, lot price, live lots, scoped member creation, isolation, invalid input and unload');
} finally {
  await ctx.fiber.dispose();
  await new Promise(resolve => { server.close(resolve); server.closeAllConnections(); });
}
