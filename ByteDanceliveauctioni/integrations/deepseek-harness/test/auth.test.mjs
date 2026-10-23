import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { AuctionAuth, AuthenticationFailedError, apiOrigin } from '../src/auth.mjs';
import { apply } from '../src/plugin.mjs';

const user = { id: 'u1', username: 'merchant', status: 'USER_STATUS_ACTIVE', permissionCodes: ['lot.view_admin'], roleCodes: ['merchant_owner'], mainAccountId: 'u1' };
const owner = { ...user, permissionCodes: ['lot.view_admin', 'team.user.create'] };
const member = {
  id: 'u2', username: 'operator_wang', nickname: '运营小王', status: 'USER_STATUS_ACTIVE',
  permissionCodes: ['lot.view_admin'], roleCodes: ['operator'], mainAccountId: 'u1', createdByUserId: 'u1',
  createdAtUnixMs: '1788624000000', updatedAtUnixMs: '1788624000000',
};
function reply() { return { result: { message: 'ok' }, user, tokens: { accessToken: 'secret-access', refreshToken: 'secret-refresh', accessExpiresAtUnixMs: String(Date.now() + 60000) } }; }
function ownerReply() { return { ...reply(), user: owner }; }

test('authorized HTTP 403 with malformed body still preserves safe permission denial', async t => {
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(reply()));
    res.statusCode = 403;
    res.end('private malformed response');
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'test-only-password' });
  await assert.rejects(auth.authorizedRequest('/api/admin/dashboard'), error => error.code === 'PERMISSION_DENIED' && !error.message.includes('private'));
});
async function fixture(t, handler) {
  const server = createServer(async (req, res) => {
    let raw = '';
    for await (const chunk of req) raw += chunk;
    res.setHeader('Content-Type', 'application/json');
    await handler(req, raw ? JSON.parse(raw) : undefined, res);
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => { server.close(resolve); server.closeAllConnections(); }));
  return `http://127.0.0.1:${server.address().port}`;
}

test('login uses existing API, keeps tokens private and authenticates me', async t => {
  const origin = await fixture(t, (req, body, res) => {
    if (req.url === '/api/users/login') {
      assert.deepEqual(body, { username: 'merchant', password: 'test-only-password' });
      res.end(JSON.stringify(reply()));
    } else {
      assert.equal(req.url, '/api/users/me');
      assert.equal(req.headers.authorization, 'Bearer secret-access');
      res.end(JSON.stringify({ user }));
    }
  });
  const auth = new AuctionAuth(origin);
  assert.equal((await auth.status()).authenticated, false);
  const result = await auth.login({ username: 'merchant', password: 'test-only-password' });
  assert.equal(result.user.username, 'merchant');
  assert.doesNotMatch(JSON.stringify(result), /secret|password|Token/);
  assert.equal((await auth.status()).authenticated, true);
});

test('failed account switch clears old credentials and sanitizes backend error', async t => {
  let calls = 0;
  const origin = await fixture(t, (_req, _body, res) => res.end(JSON.stringify(++calls === 1 ? reply() : { result: { code: 401005, message: 'secret-access test-only-password' } })));
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'test-only-password' });
  await assert.rejects(
    auth.login({ username: 'other', password: 'bad' }),
    error => error instanceof AuthenticationFailedError
      && error.code === 'AUTH_FAILED'
      && error.message === '用户名或密码不正确',
  );
  assert.equal((await auth.status()).authenticated, false);
});

test('rejects buyer, disabled account, malformed tokens and expired credentials', async t => {
  let payload;
  const origin = await fixture(t, (_req, _body, res) => res.end(JSON.stringify(payload)));
  for (const change of [
    p => { p.user = { ...user, permissionCodes: ['bid.place'] }; },
    p => { p.user = { ...user, status: 'USER_STATUS_DISABLED' }; },
    p => { p.tokens.accessToken = ''; },
    p => { p.tokens.accessExpiresAtUnixMs = '1'; },
  ]) {
    payload = reply(); change(payload);
    const auth = new AuctionAuth(origin);
    await assert.rejects(auth.login({ username: 'merchant', password: 'test' }));
    assert.equal((await auth.status()).authenticated, false);
  }
});

test('does not forward passwords across redirects; enforces configured origin', async t => {
  let destinationCalls = 0;
  const target = await fixture(t, (_req, _body, res) => { destinationCalls++; res.end('{}'); });
  const origin = await fixture(t, (_req, _body, res) => { res.writeHead(307, { Location: target }); res.end('{}'); });
  await assert.rejects(new AuctionAuth(origin).login({ username: 'merchant', password: 'test' }), /无法连接/);
  assert.equal(destinationCalls, 0);
  for (const url of ['http://example.com', 'https://user:pass@example.com', 'https://example.com/path', 'file:///tmp']) assert.throws(() => apiOrigin(url));
});

test('validates raw tool arguments before sending credentials', async () => {
  const auth = new AuctionAuth('http://127.0.0.1:1');
  for (const args of [{}, { username: 123, password: 'test' }, { username: 'u', password: 'test', url: 'x' }]) {
    await assert.rejects(auth.login(args), /必须提供/);
  }
});

test('cancelled login cannot store credentials', async t => {
  const origin = await fixture(t, (_req, _body, res) => res.end(JSON.stringify(reply())));
  const auth = new AuctionAuth(origin);
  const controller = new AbortController(); controller.abort();
  await assert.rejects(auth.login({ username: 'merchant', password: 'test' }, controller.signal), /取消/);
  assert.equal((await auth.status()).authenticated, false);
});

test('backend revocation clears cached login rather than trusting local status', async t => {
  const origin = await fixture(t, (req, _body, res) => res.end(JSON.stringify(req.url.endsWith('/login')
    ? reply() : { result: { code: 401004, message: 'revoked' } })));
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'test' });
  await assert.rejects(auth.status(), /登录已失效/);
  assert.equal((await auth.status()).authenticated, false);
});

test('authorized request requires a login, attaches the bearer token, and clears a revoked session', async t => {
  let protectedCalls = 0;
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(reply()));
    protectedCalls++;
    assert.equal(req.method, 'GET');
    assert.equal(req.url, '/api/admin/dashboard?range=today');
    assert.equal(req.headers.authorization, 'Bearer secret-access');
    res.end(JSON.stringify({ result: { code: 401004, message: 'secret-access test-only-password' } }));
  });
  const auth = new AuctionAuth(origin);
  await assert.rejects(auth.authorizedRequest('/api/admin/dashboard?range=today'), /尚未登录/);
  assert.equal(protectedCalls, 0);
  await auth.login({ username: 'merchant', password: 'test-only-password' });
  await assert.rejects(auth.authorizedRequest('/api/admin/dashboard?range=today'), /登录已失效/);
  assert.equal(protectedCalls, 1);
  assert.equal((await auth.status()).authenticated, false);
});

test('authorized request rejects a response from another merchant tenant without exposing it', async t => {
  const origin = await fixture(t, (req, _body, res) => res.end(JSON.stringify(req.url === '/api/users/login'
    ? reply()
    : { result: { code: 0, message: 'private-fixture-token' }, lots: [{ id: 'lot-1', mainAccountId: 'other-merchant' }] })));
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'fixture-password' });
  await assert.rejects(auth.authorizedRequest('/api/admin/lots'), /其他商家空间/);
});

test('authorized request never returns credential-like backend fields', async t => {
  const origin = await fixture(t, (req, _body, res) => res.end(JSON.stringify(req.url === '/api/users/login'
    ? reply()
    : { result: { code: 0 }, dashboard: { totalLots: 1, accessToken: 'secret-access', accessKey: 'fixture-access-key', password: 'fixture-password' } })));
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'fixture-password' });
  const result = await auth.authorizedRequest('/api/admin/dashboard?range=today');
  assert.deepEqual(result, { result: { code: 0 }, dashboard: { totalLots: 1 } });
  assert.doesNotMatch(JSON.stringify(result), /secret-access|fixture-access-key|fixture-password/);
});

test('HTTP 401 clears the session even when the backend code maps to another error', async t => {
  let protectedCalls = 0;
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(reply()));
    protectedCalls++; res.writeHead(401); res.end(JSON.stringify({ result: { code: 403001, message: 'private token error' } }));
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'fixture-password' });
  await assert.rejects(auth.authorizedRequest('/api/admin/dashboard'), /登录已失效/);
  await assert.rejects(auth.authorizedRequest('/api/admin/dashboard'), /尚未登录/);
  assert.equal(protectedCalls, 1);
});

test('a cancelled delayed HTTP 401 still clears the bearer session before another query', async t => {
  let protectedCalls = 0;
  let headersSent;
  const receivedHeaders = new Promise(resolve => { headersSent = resolve; });
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(reply()));
    protectedCalls++; res.writeHead(401, { 'Content-Type': 'application/json' }); res.flushHeaders(); res.write(' '); headersSent();
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'fixture-password' });
  const controller = new AbortController();
  const pending = auth.authorizedRequest('/api/admin/dashboard', { signal: controller.signal });
  const rejected = assert.rejects(pending, /登录已失效|取消/);
  await receivedHeaders; await new Promise(resolve => setTimeout(resolve, 10)); controller.abort();
  await rejected;
  await assert.rejects(auth.authorizedRequest('/api/admin/dashboard'), /尚未登录/);
  assert.equal(protectedCalls, 1);
});

test('mutating a login result cannot alter the private tenant guard or expose nested messages', async t => {
  let call = 0;
  const origin = await fixture(t, (req, _body, res) => res.end(JSON.stringify(req.url === '/api/users/login'
    ? reply()
    : (++call === 1 ? { result: { code: 0, message: 'top secret' }, nested: { error: 'private failure', message: 'private detail' } }
      : { result: { code: 0 }, lots: [{ mainAccountId: 'foreign' }] }))));
  const auth = new AuctionAuth(origin);
  const result = await auth.login({ username: 'merchant', password: 'fixture-password' });
  result.user.mainAccountId = 'foreign';
  assert.deepEqual(await auth.authorizedRequest('/api/admin/dashboard'), { result: { code: 0 }, nested: {} });
  await assert.rejects(auth.authorizedRequest('/api/admin/lots'), /其他商家空间/);
});

test('clear while login is in flight prevents late credentials from being retained', async t => {
  let received;
  const requestReceived = new Promise(resolve => { received = resolve; });
  let release;
  const released = new Promise(resolve => { release = resolve; });
  const origin = await fixture(t, async (_req, _body, res) => {
    received(); await released; res.end(JSON.stringify(reply()));
  });
  const auth = new AuctionAuth(origin);
  const pending = auth.login({ username: 'merchant', password: 'test' });
  await requestReceived;
  auth.clear(); release();
  await assert.rejects(pending, /替代/);
  assert.equal((await auth.status()).authenticated, false);
});

test('Cordis adapter separates sessions, reuses auth and clears on disposal', async t => {
  const origin = await fixture(t, (req, _body, res) => res.end(JSON.stringify(req.url.endsWith('/me') ? { user } : reply())));
  const tools = new Map(); const cleanup = [];
  const ctx = { tools: { register: tool => tools.set(tool.name, tool) }, skills: { register: () => () => {} }, effect: setup => cleanup.push(setup()) };
  apply(ctx, { apiBaseUrl: origin });
  const a = { agent: { session: {}, ctx }, signal: new AbortController().signal };
  const b = { agent: { session: {}, ctx }, signal: new AbortController().signal };
  const login = tools.get('auction_login'); const status = tools.get('auction_login_status');
  await login.execute({ username: 'merchant', password: 'test' }, a);
  assert.equal((await status.execute({}, a)).authenticated, true);
  assert.equal((await status.execute({}, b)).authenticated, false);
  assert.doesNotMatch(JSON.stringify(login.output.render({}, await status.execute({}, a))), /secret-access|secret-refresh/);
  for (const dispose of cleanup) dispose();
  assert.equal((await status.execute({}, a)).authenticated, false);
  await assert.rejects(status.execute({ unexpected: 1 }, a));
  await assert.rejects(status.execute({}, {}), /Harness Agent/);
});

test('creates a scoped team member with the authenticated owner token and keeps passwords private', async t => {
  let createCalls = 0;
  const origin = await fixture(t, (req, body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(ownerReply()));
    assert.equal(req.url, '/api/admin/users');
    assert.equal(req.method, 'POST');
    assert.equal(req.headers.authorization, 'Bearer secret-access');
    assert.deepEqual(body, {
      username: 'operator_wang', nickname: '运营小王', password: 'member-password', roleCode: 'operator',
    });
    createCalls++;
    res.end(JSON.stringify({ result: { code: 0, message: 'ok' }, user: member }));
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'owner-password' });
  const created = await auth.createTeamMember({
    mainAccountUsername: 'merchant', username: 'operator_wang', nickname: ' 运营小王 ',
    password: 'member-password', roleCode: 'operator',
  });
  assert.equal(createCalls, 1);
  assert.deepEqual(created, { created: true, user: member });
  assert.doesNotMatch(JSON.stringify(created), /member-password|secret-access|secret-refresh/);
});

test('requires login and refuses to create under a different main account', async t => {
  let createCalls = 0;
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(ownerReply()));
    createCalls++;
    res.end(JSON.stringify({ user: member }));
  });
  const args = { mainAccountUsername: 'merchant', username: 'operator_wang', nickname: '运营小王', password: 'member-password', roleCode: 'operator' };
  const auth = new AuctionAuth(origin);
  await assert.rejects(auth.createTeamMember(args), /尚未登录/);
  await auth.login({ username: 'merchant', password: 'owner-password' });
  await assert.rejects(auth.createTeamMember({ ...args, mainAccountUsername: 'other_owner' }), /当前登录.*merchant.*other_owner/);
  assert.equal(createCalls, 0);
});

test('validates team member fields and only permits managed subaccount roles', async t => {
  let createCalls = 0;
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(ownerReply()));
    createCalls++;
    res.end('{}');
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'owner-password' });
  const valid = { mainAccountUsername: 'merchant', username: 'operator_wang', nickname: '运营小王', password: 'member-password', roleCode: 'operator' };
  for (const invalid of [
    { ...valid, username: 'bad name' },
    { ...valid, nickname: '' },
    { ...valid, password: 'short' },
    { ...valid, roleCode: 'merchant_owner' },
    { ...valid, roleCode: 'buyer' },
    { ...valid, unexpected: true },
  ]) await assert.rejects(auth.createTeamMember(invalid), /用户名|昵称|密码|角色|参数/);
  assert.equal(createCalls, 0);
});

test('requires team creation permission and sanitizes username conflicts', async t => {
  let loginUser = user;
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify({ ...reply(), user: loginUser }));
    res.end(JSON.stringify({ result: { code: 409002, message: 'member-password secret-access' } }));
  });
  const args = { mainAccountUsername: 'merchant', username: 'operator_wang', nickname: '运营小王', password: 'member-password', roleCode: 'operator' };
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'owner-password' });
  await assert.rejects(auth.createTeamMember(args), /主账号.*权限/);
  loginUser = owner;
  await auth.login({ username: 'merchant', password: 'owner-password' });
  await assert.rejects(auth.createTeamMember(args), /用户名已被使用/);
});

test('rejects out-of-scope or malformed created users', async t => {
  let createdUser;
  const origin = await fixture(t, (req, _body, res) => {
    if (req.url === '/api/users/login') return res.end(JSON.stringify(ownerReply()));
    res.end(JSON.stringify({ user: createdUser }));
  });
  const auth = new AuctionAuth(origin);
  await auth.login({ username: 'merchant', password: 'owner-password' });
  const args = { mainAccountUsername: 'merchant', username: 'operator_wang', nickname: '运营小王', password: 'member-password', roleCode: 'operator' };
  for (createdUser of [
    { ...member, mainAccountId: 'another-space' },
    { ...member, roleCodes: ['buyer'] },
    { ...member, username: '' },
  ]) await assert.rejects(auth.createTeamMember(args), /成员数据|商家空间|团队角色/);
});

test('Cordis exposes a structured team creation tool tied to the requested owner', async t => {
  const origin = await fixture(t, (req, _body, res) => res.end(JSON.stringify(req.url === '/api/users/login'
    ? ownerReply() : { user: member })));
  const tools = new Map(); const cleanup = [];
  const ctx = { tools: { register: tool => tools.set(tool.name, tool) }, skills: { register: () => () => {} }, effect: setup => cleanup.push(setup()) };
  apply(ctx, { apiBaseUrl: origin });
  const create = tools.get('auction_create_team_member');
  assert.ok(create);
  assert.deepEqual(create.parameters.required, ['mainAccountUsername', 'username', 'nickname', 'password', 'roleCode']);
  assert.deepEqual(create.parameters.properties.roleCode.enum, ['anchor', 'operator']);
  const exec = { agent: { session: {}, ctx }, signal: new AbortController().signal };
  await tools.get('auction_login').execute({ username: 'merchant', password: 'owner-password' }, exec);
  const result = await create.execute({ mainAccountUsername: 'merchant', username: 'operator_wang', nickname: '运营小王', password: 'member-password', roleCode: 'operator' }, exec);
  assert.equal(result.created, true);
  assert.doesNotMatch(JSON.stringify(create.output.render({}, result)), /member-password|secret-access/);
  for (const dispose of cleanup) dispose();
});
