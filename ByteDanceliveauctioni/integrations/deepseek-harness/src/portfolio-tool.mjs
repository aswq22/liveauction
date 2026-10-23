import { AuctionApi } from './auction-client.mjs';
import { AuctionAuth } from './auth.mjs';
import { aggregatePortfolio } from './portfolio-aggregate.mjs';
import { normalizeDashboardArgs } from './query-tools.mjs';

const ROOT_KEYS = new Set(['accounts', 'range', 'sections']);
const ACCOUNT_KEYS = new Set(['username', 'password', 'alias']);
const REQUIRED_PERMISSIONS = ['lot.view_admin', 'order.manage', 'realtime.view'];
const SAFE_CODES = new Set(['AUTH_FAILED', 'PERMISSION_DENIED', 'BACKEND_UNAVAILABLE', 'INVALID_BACKEND_RESPONSE']);

function invalid() { throw new Error('portfolio 参数无效'); }

function boundedText(value, { required = false, max, trim = false } = {}) {
  if (value === undefined && !required) return '';
  if (typeof value !== 'string') invalid();
  const result = trim ? value.trim() : value;
  if ((required && !result) || [...result].length > max) invalid();
  return result;
}

function normalizeAccount(account, inputIndex) {
  if (!account || typeof account !== 'object' || Array.isArray(account)
      || Object.keys(account).some(key => !ACCOUNT_KEYS.has(key))) invalid();
  return {
    username: boundedText(account.username, { required: true, max: 128, trim: true }),
    password: boundedText(account.password, { required: true, max: 1024 }),
    alias: boundedText(account.alias, { max: 64, trim: true }),
    inputIndex,
  };
}

export function normalizePortfolioArgs(args) {
  if (!args || typeof args !== 'object' || Array.isArray(args)
      || Object.keys(args).some(key => !ROOT_KEYS.has(key))
      || !Array.isArray(args.accounts) || args.accounts.length < 2 || args.accounts.length > 10) invalid();
  const accounts = args.accounts.map(normalizeAccount);
  const dashboard = normalizeDashboardArgs({
    ...(args.range === undefined ? {} : { range: args.range }),
    ...(args.sections === undefined ? {} : { sections: args.sections }),
  });
  return { accounts, ...dashboard };
}

function safeCode(error) {
  if (error?.code === 'AUTHENTICATION_REQUIRED') return 'AUTH_FAILED';
  return SAFE_CODES.has(error?.code) ? error.code : 'ACCOUNT_QUERY_FAILED';
}

function safeFailure(account, code, authenticated = false) {
  return {
    ok: false,
    authenticated,
    inputIndex: account.inputIndex,
    alias: account.alias,
    code,
  };
}

function validOwner(user) {
  return user?.roleCodes?.includes('merchant_owner')
    && REQUIRED_PERMISSIONS.every(permission => user.permissionCodes?.includes(permission));
}

async function queryAccount(account, context, signal) {
  let authenticated = false;
  try {
    const auth = context.createAuth(context.origin);
    if (!auth || typeof auth.login !== 'function' || typeof auth.authorizedRequest !== 'function' || typeof auth.clear !== 'function') {
      throw new Error('portfolio auth 工厂无效');
    }
    context.temporaryAuth.add(auth);
    const login = await auth.login({ username: account.username, password: account.password }, signal);
    signal?.throwIfAborted();
    authenticated = true;
    if (!validOwner(login.user)) return safeFailure(account, 'PERMISSION_DENIED', true);
    const api = new AuctionApi(auth.authorizedRequest.bind(auth));
    const dashboard = await api.getDashboard({ range: context.range, sections: context.sections }, signal);
    signal?.throwIfAborted();
    return {
      ok: true,
      authenticated: true,
      inputIndex: account.inputIndex,
      alias: account.alias,
      user: login.user,
      dashboard,
    };
  } catch (error) {
    signal?.throwIfAborted();
    return safeFailure(account, safeCode(error), authenticated);
  }
}

function finishPortfolioReport(results, input) {
  const successes = [];
  const partialErrors = [];
  const tenants = new Set();
  let authenticated = 0;
  for (const result of results) {
    if (result.authenticated) authenticated++;
    if (!result.ok) {
      partialErrors.push({ inputIndex: result.inputIndex, alias: result.alias, code: result.code });
      continue;
    }
    const tenant = result.user.mainAccountId;
    if (tenants.has(tenant)) {
      partialErrors.push({ inputIndex: result.inputIndex, alias: result.alias, code: 'DUPLICATE_ACCOUNT' });
      continue;
    }
    tenants.add(tenant);
    successes.push(result);
  }
  const coverage = {
    requested: input.accounts.length,
    authenticated,
    included: successes.length,
    failed: input.accounts.length - successes.length,
  };
  if (!successes.length) return { status: 'ALL_ACCOUNTS_FAILED', coverage, partialErrors };
  return {
    status: successes.length === input.accounts.length ? 'OK' : 'PARTIAL',
    coverage,
    ...aggregatePortfolio(successes, { range: input.range }),
    partialErrors,
  };
}

export async function getMultiAccountReport(args, { origin, createAuth = value => new AuctionAuth(value) }, signal) {
  const input = normalizePortfolioArgs(args);
  signal?.throwIfAborted();
  const temporaryAuth = new Set();
  const results = new Array(input.accounts.length);
  let cursor = 0;
  async function worker() {
    while (cursor < input.accounts.length) {
      signal?.throwIfAborted();
      const index = cursor++;
      results[index] = await queryAccount(input.accounts[index], {
        origin, createAuth, temporaryAuth, range: input.range, sections: input.sections,
      }, signal);
    }
  }
  try {
    await Promise.all(Array.from({ length: Math.min(3, input.accounts.length) }, () => worker()));
    signal?.throwIfAborted();
    return finishPortfolioReport(results, input);
  } finally {
    for (const auth of temporaryAuth) auth.clear();
    temporaryAuth.clear();
  }
}
