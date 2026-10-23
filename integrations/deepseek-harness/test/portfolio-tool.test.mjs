import assert from 'node:assert/strict';
import test from 'node:test';
import { AuthenticationFailedError, BackendUnavailableError } from '../src/auth.mjs';
import { getMultiAccountReport, normalizePortfolioArgs } from '../src/portfolio-tool.mjs';
import { dashboardReply } from './fixtures/dashboard.mjs';

const permissions = ['lot.view_admin', 'order.manage', 'realtime.view'];

function args(count = 2) {
  return {
    accounts: Array.from({ length: count }, (_, index) => ({
      username: ` merchant-${index} `,
      password: `password-${index}`,
      ...(index === 0 ? { alias: ' 店铺A ' } : {}),
    })),
    range: '30d',
    sections: ['summary', 'orderRisks', 'summary'],
  };
}

test('portfolio arguments are closed, bounded and normalized before authentication', () => {
  assert.deepEqual(normalizePortfolioArgs(args()), {
    accounts: [
      { username: 'merchant-0', password: 'password-0', alias: '店铺A', inputIndex: 0 },
      { username: 'merchant-1', password: 'password-1', alias: '', inputIndex: 1 },
    ],
    range: '30d',
    sections: ['summary', 'orderRisks'],
  });
  for (const invalid of [
    null, [], {}, args(1), args(11), { ...args(), unexpected: true },
    { ...args(), range: 'year' }, { ...args(), sections: ['unknown'] },
    { ...args(), accounts: [{ username: '', password: 'x' }, args().accounts[1]] },
    { ...args(), accounts: [{ username: 'a', password: '', alias: 'A' }, args().accounts[1]] },
    { ...args(), accounts: [{ username: 'a', password: 'x', token: 'secret' }, args().accounts[1]] },
  ]) assert.throws(() => normalizePortfolioArgs(invalid), /portfolio|dashboard/);
});

function scenario(mainAccountId, options = {}) {
  return {
    mainAccountId,
    clearCalls: 0,
    async login(input, signal) {
      signal?.throwIfAborted();
      options.onStart?.();
      if (options.delay) await new Promise(resolve => setTimeout(resolve, options.delay));
      options.onEnd?.();
      signal?.throwIfAborted();
      if (options.loginError) throw options.loginError;
      return {
        authenticated: true,
        user: {
          id: `${mainAccountId}-owner`, username: input.username, mainAccountId,
          roleCodes: options.roles ?? ['merchant_owner'], permissionCodes: options.permissions ?? permissions,
        },
        accessExpiresAtUnixMs: Date.now() + 60_000,
      };
    },
    async authorizedRequest(path, { signal } = {}) {
      signal?.throwIfAborted();
      if (options.queryError) throw options.queryError;
      assert.match(path, /^\/api\/admin\/dashboard\?range=30d/);
      return dashboardReply({ trendCount: 1, categoryCount: 1, lotCount: 1, topCount: 1, range: '30d' });
    },
    clear() { this.clearCalls++; },
  };
}

test('portfolio isolates accounts, limits concurrency, deduplicates tenants and preserves partial success', async () => {
  let active = 0;
  let maximum = 0;
  const lifecycle = {
    onStart() { maximum = Math.max(maximum, ++active); },
    onEnd() { active--; },
    delay: 5,
  };
  const scenarios = [
    scenario('tenant-a', lifecycle),
    scenario('invalid', { ...lifecycle, loginError: new AuthenticationFailedError() }),
    scenario('tenant-b', lifecycle),
    scenario('tenant-a', lifecycle),
    scenario('unavailable', { ...lifecycle, queryError: new BackendUnavailableError('raw-backend-message private-token') }),
  ];
  let cursor = 0;
  const report = await getMultiAccountReport(args(5), {
    origin: 'http://127.0.0.1:18080',
    createAuth: () => scenarios[cursor++],
  });

  assert.equal(maximum, 3);
  assert.equal(scenarios.every(item => item.clearCalls === 1), true);
  assert.deepEqual(report.coverage, { requested: 5, authenticated: 4, included: 2, failed: 3 });
  assert.equal(report.status, 'PARTIAL');
  assert.deepEqual(report.accounts.map(account => account.mainAccountId), ['tenant-a', 'tenant-b']);
  assert.deepEqual(report.partialErrors.map(error => error.code), ['AUTH_FAILED', 'DUPLICATE_ACCOUNT', 'BACKEND_UNAVAILABLE']);
  assert.doesNotMatch(JSON.stringify(report), /password-[0-9]|private-token|raw-backend-message/);
});

test('portfolio returns an explicit all-failed result and clears every temporary auth', async () => {
  const scenarios = [
    scenario('bad-a', { loginError: new AuthenticationFailedError() }),
    scenario('bad-b', { queryError: new BackendUnavailableError() }),
  ];
  let cursor = 0;
  const report = await getMultiAccountReport(args(), {
    origin: 'http://127.0.0.1:18080', createAuth: () => scenarios[cursor++],
  });
  assert.deepEqual(report, {
    status: 'ALL_ACCOUNTS_FAILED',
    coverage: { requested: 2, authenticated: 1, included: 0, failed: 2 },
    partialErrors: [
      { inputIndex: 0, alias: '店铺A', code: 'AUTH_FAILED' },
      { inputIndex: 1, alias: '', code: 'BACKEND_UNAVAILABLE' },
    ],
  });
  assert.equal(scenarios.every(item => item.clearCalls === 1), true);
});

test('portfolio cancellation stops the batch and clears claimed accounts', async () => {
  const controller = new AbortController();
  const scenarios = [scenario('tenant-a', {
    async onStart() { controller.abort(); },
  }), scenario('tenant-b')];
  let cursor = 0;
  await assert.rejects(getMultiAccountReport(args(), {
    origin: 'http://127.0.0.1:18080', createAuth: () => scenarios[cursor++],
  }, controller.signal), { name: 'AbortError' });
  assert.equal(scenarios[0].clearCalls, 1);
  assert.equal(scenarios[1].clearCalls, 0);
});
