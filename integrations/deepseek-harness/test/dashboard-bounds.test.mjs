import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { AuctionAuth } from '../src/auth.mjs';
import { AuctionApi } from '../src/auction-client.mjs';
import { getDashboard } from '../src/query-tools.mjs';
import { dashboardReply } from './fixtures/dashboard.mjs';

test('dashboard preserves complete bounded trend, Top 10, categories and top 100 lot performance', async () => {
  for (const [range, count] of [['today', 24], ['7d', 7], ['30d', 30], ['live', 720]]) {
    const api = new AuctionApi(async () => dashboardReply({ trendCount: count, range }));
    const value = await getDashboard({ range }, { api });
    assert.equal(value.trend.length, count);
    assert.equal(value.trend.at(-1).paidAmount, count - 1);
    assert.equal(value.topLots.length, 10);
    assert.equal(value.topLots[9].lotId, 'top-9');
    assert.equal(value.categoryPerformance.length, 1000);
    assert.equal(value.categoryPerformance[999].paidAmount, 999);
    assert.equal(value.lotPerformance.length, 100);
    assert.equal(value.lotPerformance[99].lotId, 'lot-99');
    assert.equal(value.display.trend.length, count);
    assert.equal(value.coverage.lotPerformance.selection, 'top_by_paid_amount_then_lot_id');
    assert.equal(value.coverage.lotPerformance.limit, 100);
  }
});

test('dashboard rejects out-of-contract panel sizes rather than silently dropping rows', async () => {
  for (const over of [{ trendCount: 721 }, { topCount: 11 }, { categoryCount: 1001 }, { lotCount: 101 }]) {
    const api = new AuctionApi(async () => dashboardReply(over));
    await assert.rejects(api.getDashboard(), /无效/);
  }
});

async function authenticatedFixture(t, reply, { chunked = false } = {}) {
  const server = createServer((req, res) => {
    res.setHeader('Content-Type', 'application/json');
    if (req.url === '/api/users/login') return res.end(JSON.stringify({ user: { id: 'user-a', username: 'merchant', mainAccountId: 'tenant-a', status: 'USER_STATUS_ACTIVE', roleCodes: ['operator'], permissionCodes: ['lot.view_admin'] }, tokens: { accessToken: 'fixture-token', accessExpiresAtUnixMs: Date.now() + 60000 } }));
    assert.equal(req.headers.authorization, 'Bearer fixture-token');
    const payload = JSON.stringify(reply);
    if (chunked) { res.write(payload.slice(0, 100)); res.end(payload.slice(100)); }
    else res.end(payload);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => { server.close(resolve); server.closeAllConnections(); }));
  const auth = new AuctionAuth(`http://127.0.0.1:${server.address().port}`);
  await auth.login({ username: 'merchant', password: 'fixture-password' });
  return auth;
}

test('authorized dashboard accepts maximum cardinality through real HTTP transport', async t => {
  const reply = dashboardReply();
  const countNodes = value => 1 + (value && typeof value === 'object' ? Object.values(value).reduce((sum, item) => sum + countNodes(item), 0) : 0);
  t.diagnostic(`max dashboard fixture: ${countNodes(reply)} nodes; ${Buffer.byteLength(JSON.stringify(reply))} bytes`);
  const auth = await authenticatedFixture(t, reply);
  const value = await getDashboard({ range: 'live' }, { api: new AuctionApi(auth.authorizedRequest.bind(auth)) });
  assert.equal(value.trend.length, 720);
  assert.equal(value.categoryPerformance.length, 1000);
  assert.equal(value.lotPerformance[99].paidAmount, 901);
});

test('authorized transport retains node and byte DoS guards including chunked bodies', async t => {
  for (const [name, reply, options] of [
    ['nodes', { rows: Array(20001).fill(0) }, {}],
    ['depth', Array.from({ length: 40 }).reduce(value => ({ value }), 0), {}],
    ['bytes', { value: 'x'.repeat(2 * 1024 * 1024) }, {}],
    ['chunked bytes', { value: 'x'.repeat(2 * 1024 * 1024) }, { chunked: true }],
  ]) await t.test(name, async t => {
    const auth = await authenticatedFixture(t, reply, options);
    await assert.rejects(auth.authorizedRequest('/api/admin/dashboard'), /过大/);
  });
});
