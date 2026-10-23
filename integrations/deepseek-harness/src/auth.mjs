const BACKOFFICE = ['lot.view_admin', 'auction.control', 'realtime.view', 'order.manage'];
const ERRORS = {
  400001: '创建信息不符合后台规则，请检查用户名、昵称、密码和角色',
  401005: '用户名或密码不正确',
  403002: '账号已停用，请联系管理员',
  403001: '账号没有执行该操作的权限',
  409002: '该成员用户名已被使用，请更换用户名',
};
const MANAGED_ROLES = ['anchor', 'operator'];
const USERNAME = /^[a-z0-9_-]{3,64}$/;
export class AuthenticationExpiredError extends Error { constructor() { super('登录已失效，请重新登录'); this.code = 'AUTHENTICATION_REQUIRED'; } }
export class AuthenticationFailedError extends Error { constructor() { super(ERRORS[401005]); this.code = 'AUTH_FAILED'; } }
export class PermissionDeniedError extends Error { constructor(message = ERRORS[403001]) { super(message); this.code = 'PERMISSION_DENIED'; } }
export class BackendUnavailableError extends Error { constructor(message = '竞拍服务暂时不可用，请稍后重试') { super(message); this.code = 'BACKEND_UNAVAILABLE'; } }
export class InvalidBackendResponseError extends Error { constructor() { super('竞拍服务返回了无效数据'); this.code = 'INVALID_BACKEND_RESPONSE'; } }

/** Only configured HTTPS or loopback HTTP endpoints may receive credentials. */
export function apiOrigin(value) {
  const url = new URL(value);
  const local = ['localhost', '127.0.0.1', '[::1]'].includes(url.hostname);
  if ((url.protocol !== 'https:' && !(url.protocol === 'http:' && local)) ||
      url.username || url.password || url.search || url.hash || url.pathname !== '/') {
    throw new Error('API 地址必须是 HTTPS 源地址，本地开发可用 http://127.0.0.1:18080');
  }
  return url.origin;
}

function account(raw) {
  const permissions = raw?.permissionCodes ?? raw?.permission_codes;
  const roles = raw?.roleCodes ?? raw?.role_codes ?? [];
  if (!raw || typeof raw.id !== 'string' || !raw.id || typeof raw.username !== 'string' ||
      !Array.isArray(permissions) || !permissions.every(p => typeof p === 'string') ||
      !Array.isArray(roles) || !roles.every(r => typeof r === 'string')) {
    throw new Error('登录服务返回的账号数据不完整');
  }
  if (![1, 'USER_STATUS_ACTIVE'].includes(raw.status) || !permissions.some(p => BACKOFFICE.includes(p))) {
    throw new Error('该账号无后台访问权限，或已被停用');
  }
  return {
    id: raw.id, username: raw.username,
    mainAccountId: String(raw.mainAccountId ?? raw.main_account_id ?? ''),
    roleCodes: [...roles], permissionCodes: [...permissions],
  };
}

function teamMember(raw, mainAccountId, requestedRole) {
  const roles = raw?.roleCodes ?? raw?.role_codes;
  const permissions = raw?.permissionCodes ?? raw?.permission_codes;
  const memberMainAccountId = raw?.mainAccountId ?? raw?.main_account_id;
  const createdByUserId = raw?.createdByUserId ?? raw?.created_by_user_id;
  const createdAtUnixMs = raw?.createdAtUnixMs ?? raw?.created_at_unix_ms;
  const updatedAtUnixMs = raw?.updatedAtUnixMs ?? raw?.updated_at_unix_ms;
  if (!raw || typeof raw.id !== 'string' || !raw.id || typeof raw.username !== 'string' || !raw.username ||
      typeof raw.nickname !== 'string' || !raw.nickname || !Array.isArray(roles) ||
      !roles.every(role => typeof role === 'string') || !Array.isArray(permissions) ||
      !permissions.every(permission => typeof permission === 'string') || typeof memberMainAccountId !== 'string' ||
      typeof createdByUserId !== 'string' || ![1, 'USER_STATUS_ACTIVE'].includes(raw.status) ||
      ![createdAtUnixMs, updatedAtUnixMs].every(value => Number.isSafeInteger(Number(value)))) {
    throw new Error('创建服务返回的成员数据不完整');
  }
  if (memberMainAccountId !== mainAccountId) throw new Error('创建服务返回了其他商家空间的成员数据');
  if (!roles.includes(requestedRole) || !roles.some(role => MANAGED_ROLES.includes(role))) {
    throw new Error('创建服务返回了不允许的团队角色');
  }
  return {
    id: raw.id, username: raw.username, nickname: raw.nickname, status: raw.status,
    roleCodes: [...roles], permissionCodes: [...permissions], mainAccountId: memberMainAccountId,
    createdByUserId, createdAtUnixMs, updatedAtUnixMs,
  };
}

function normalizeTeamMemberArgs(args) {
  const allowed = ['mainAccountUsername', 'username', 'nickname', 'password', 'roleCode'];
  if (!args || typeof args !== 'object' || Array.isArray(args) || Object.keys(args).some(key => !allowed.includes(key))) {
    throw new Error('创建团队成员参数不完整或包含未知参数');
  }
  const mainAccountUsername = typeof args.mainAccountUsername === 'string' ? args.mainAccountUsername.trim().toLowerCase() : '';
  const username = typeof args.username === 'string' ? args.username.trim().toLowerCase() : '';
  const nickname = typeof args.nickname === 'string' ? args.nickname.trim() : '';
  if (!USERNAME.test(mainAccountUsername)) throw new Error('必须提供有效的目标主账号用户名');
  if (!USERNAME.test(username)) throw new Error('成员用户名必须是 3-64 位小写字母、数字、下划线或连字符');
  if (!nickname || [...nickname].length > 128) throw new Error('成员昵称不能为空且不能超过 128 个字符');
  if (typeof args.password !== 'string' || args.password.length < 8 || args.password.length > 128) {
    throw new Error('成员初始密码必须是 8-128 个字符');
  }
  const roleCode = typeof args.roleCode === 'string' ? args.roleCode.trim().toLowerCase() : '';
  if (!MANAGED_ROLES.includes(roleCode)) throw new Error('成员角色只能是 anchor（主播/场控）或 operator（运营）');
  return { mainAccountUsername, username, nickname, password: args.password, roleCode };
}

function authorizedPath(path) {
  if (typeof path !== 'string' || !path.startsWith('/api/') || path.includes('#') || /[\r\n]/.test(path)) {
    throw new Error('只允许查询当前竞拍服务的 /api/ 路径');
  }
  const url = new URL(path, 'https://auction.invalid');
  if (url.origin !== 'https://auction.invalid' || !url.pathname.startsWith('/api/') || url.pathname.includes('..')) {
    throw new Error('只允许查询当前竞拍服务的 /api/ 路径');
  }
  return `${url.pathname}${url.search}`;
}

// The maximum dashboard fixture is 14,768 nodes / 253,191 UTF-8 bytes.
// Keep headroom for bounded strings and envelopes without allowing unbounded JSON.
const MAX_RESPONSE_NODES = 20000;
const MAX_RESPONSE_BYTES = 2 * 1024 * 1024;
const MAX_RESPONSE_DEPTH = 32;

async function responseJSON(response) {
  if (Number(response.headers.get('content-length')) > MAX_RESPONSE_BYTES) {
    await response.body?.cancel();
    throw new Error('竞拍服务返回了过大的响应');
  }
  const reader = response.body?.getReader();
  if (!reader) throw new InvalidBackendResponseError();
  const decoder = new TextDecoder();
  let size = 0;
  let body = '';
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      size += value.byteLength;
      if (size > MAX_RESPONSE_BYTES) {
        await reader.cancel();
        throw new Error('竞拍服务返回了过大的响应');
      }
      body += decoder.decode(value, { stream: true });
    }
    body += decoder.decode();
  } finally { reader.releaseLock(); }
  try { return JSON.parse(body); } catch { throw new InvalidBackendResponseError(); }
}

function publicResponse(value, budget = { nodes: MAX_RESPONSE_NODES }, depth = 0) {
  if (budget.nodes-- <= 0 || depth > MAX_RESPONSE_DEPTH) throw new Error('竞拍服务返回了过大的响应');
  if (Array.isArray(value)) return value.map(item => publicResponse(item, budget, depth + 1));
  if (!value || typeof value !== 'object') return value;
  const safe = {};
  for (const [key, item] of Object.entries(value)) {
    if (/(token|password|passwd|secret|authorization|cookie|credential|(?:api|private|access|refresh|session)[_-]?key|bearer)/i.test(key) || /^(message|error)$/i.test(key)) continue;
    safe[key] = publicResponse(item, budget, depth + 1);
  }
  return safe;
}

function publicUser(user) {
  return { id: user.id, username: user.username, mainAccountId: user.mainAccountId, roleCodes: [...user.roleCodes], permissionCodes: [...user.permissionCodes] };
}

function privateUser(user) {
  return Object.freeze({ ...publicUser(user), roleCodes: Object.freeze([...user.roleCodes]), permissionCodes: Object.freeze([...user.permissionCodes]) });
}

function belongsToTenant(value, mainAccountId, budget = { nodes: MAX_RESPONSE_NODES }, depth = 0) {
  if (budget.nodes-- <= 0 || depth > MAX_RESPONSE_DEPTH) throw new Error('竞拍服务返回了过大的响应');
  if (Array.isArray(value)) return value.every(item => belongsToTenant(item, mainAccountId, budget, depth + 1));
  if (!value || typeof value !== 'object') return true;
  const tenant = value.mainAccountId ?? value.main_account_id;
  if (tenant !== undefined && (typeof tenant !== 'string' || tenant !== mainAccountId)) return false;
  return Object.values(value).every(item => belongsToTenant(item, mainAccountId, budget, depth + 1));
}

/** Credentials live only in this instance; callers receive public account data. */
export class AuctionAuth {
  #origin;
  #state;
  #version = 0;
  constructor(origin) { this.#origin = apiOrigin(origin); }
  clear() { this.#version++; this.#state = undefined; }

  async #request(path, body, token, signal) {
    const combined = AbortSignal.any([AbortSignal.timeout(15000), ...(signal ? [signal] : [])]);
    try {
      const response = await fetch(this.#origin + path, {
        method: body ? 'POST' : 'GET', redirect: 'error', signal: combined,
        headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: `Bearer ${token}` } : {}) },
        ...(body ? { body: JSON.stringify(body) } : {}),
      });
      if (response.status === 401) throw new AuthenticationExpiredError();
      const data = await responseJSON(response).catch(error => {
        if (response.status === 403) throw new PermissionDeniedError();
        throw error;
      });
      const code = data?.result?.code ?? 0;
      if (!response.ok || code !== 0) {
        // Never expose untrusted response messages, which could echo credentials.
        if (code === 401005) throw new AuthenticationFailedError();
        if ([401001, 401002, 401003, 401004].includes(code)) throw new AuthenticationExpiredError();
        if (response.status === 403 || [403001, 403002].includes(code)) throw new PermissionDeniedError([403001, 403002].includes(code) ? ERRORS[code] : undefined);
        if (response.status >= 500 || (code >= 500000 && code < 600000)) throw new BackendUnavailableError();
        throw new Error(ERRORS[code] ?? '竞拍服务拒绝了请求，请检查服务状态');
      }
      if (!data || typeof data !== 'object') throw new InvalidBackendResponseError();
      return data;
    } catch (error) {
      if (error instanceof AuthenticationExpiredError) throw error;
      if (combined.aborted) throw new BackendUnavailableError('请求已取消或超时');
      if (error instanceof TypeError) throw new BackendUnavailableError('无法连接竞拍服务，请检查 API 地址和后端状态');
      throw error;
    }
  }

  async login(args, signal) {
    if (!args || typeof args.username !== 'string' || !args.username.trim() || args.username.length > 128 ||
        typeof args.password !== 'string' || !args.password || args.password.length > 1024 ||
        Object.keys(args).some(k => !['username', 'password'].includes(k))) {
      throw new Error('必须提供有效的 username 和 password');
    }
    this.clear(); // A failed account switch must not keep operating as the old user.
    const version = this.#version;
    const data = await this.#request('/api/users/login', { username: args.username.trim(), password: args.password }, undefined, signal);
    const user = account(data.user);
    const token = data.tokens?.accessToken ?? data.tokens?.access_token;
    const expires = Number(data.tokens?.accessExpiresAtUnixMs ?? data.tokens?.access_expires_at_unix_ms);
    if (typeof token !== 'string' || !token || !Number.isSafeInteger(expires) || expires <= Date.now()) {
      throw new Error('登录服务返回了无效或过期的凭证');
    }
    if (signal?.aborted || version !== this.#version) throw new Error('登录已取消或被后续操作替代');
    this.#state = { token, expires, user: privateUser(user), mainAccountId: user.mainAccountId };
    return { authenticated: true, user: publicUser(user), accessExpiresAtUnixMs: expires };
  }

  async status(signal) {
    const state = this.#state;
    if (!state || state.expires <= Date.now()) {
      this.clear();
      return { authenticated: false, message: '尚未登录或登录已过期，请提供账号密码登录' };
    }
    try {
      const data = await this.#request('/api/users/me', undefined, state.token, signal);
      const user = account(data.user);
      if (user.id !== state.user.id || this.#state !== state) throw new Error('登录状态已变化，请重新查询');
      return { authenticated: true, user: publicUser(user), accessExpiresAtUnixMs: state.expires };
    } catch (error) {
      if (this.#state === state) this.clear();
      throw error;
    }
  }

  async authorizedRequest(path, { signal } = {}) {
    const state = this.#state;
    if (!state || state.expires <= Date.now()) {
      this.clear();
      throw new Error('尚未登录或登录已过期，请先登录商家后台账号');
    }
    const safePath = authorizedPath(path);
    try {
      const data = await this.#request(safePath, undefined, state.token, signal);
      if (this.#state !== state) throw new Error('登录账号已发生变化，请重新查询');
      if (!belongsToTenant(data, state.mainAccountId)) throw new Error('竞拍服务返回了其他商家空间的数据');
      return publicResponse(data);
    } catch (error) {
      if (error instanceof AuthenticationExpiredError || (error?.message === '登录已失效，请重新登录')) if (this.#state === state) this.clear();
      throw error;
    }
  }

  async createTeamMember(args, signal) {
    const input = normalizeTeamMemberArgs(args);
    const state = this.#state;
    if (!state || state.expires <= Date.now()) {
      this.clear();
      throw new Error(`尚未登录，请先登录目标主账号 ${input.mainAccountUsername}`);
    }
    if (state.user.username.toLowerCase() !== input.mainAccountUsername) {
      throw new Error(`当前登录主账号是 ${state.user.username}，目标主账号是 ${input.mainAccountUsername}，请先切换登录`);
    }
    if (!state.user.roleCodes.includes('merchant_owner') || !state.user.permissionCodes.includes('team.user.create')) {
      throw new Error('当前账号不是主账号或缺少团队成员创建权限');
    }
    try {
      const data = await this.#request('/api/admin/users', {
        username: input.username, nickname: input.nickname, password: input.password, roleCode: input.roleCode,
      }, state.token, signal);
      if (this.#state !== state) throw new Error('登录账号已发生变化，不能确认成员创建结果');
      return { created: true, user: teamMember(data.user, state.user.mainAccountId, input.roleCode) };
    } catch (error) {
      if (error instanceof AuthenticationExpiredError || error?.message === '登录已失效，请重新登录') if (this.#state === state) this.clear();
      throw error;
    }
  }
}
