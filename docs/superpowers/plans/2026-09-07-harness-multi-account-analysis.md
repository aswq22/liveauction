# DeepSeek Harness Multi-Account Analysis Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a safe, read-only DeepSeek Harness Tool and runtime Skill that authenticate 2-10 independent merchant accounts, deterministically aggregate their dashboards, and report partial failures without changing the current single-account session.

**Architecture:** Keep the Go backend unchanged and reuse its existing per-tenant login and Dashboard APIs. The Harness plugin creates one temporary `AuctionAuth`/`AuctionApi` pair per input account, executes account chains with a three-worker limit, clears every temporary auth object in `finally`, and passes successful snapshots to a pure aggregation module. A narrow runtime Skill tells the model when to use the composite Tool and how to separate facts, risk signals, inferences, and recommendations.

**Tech Stack:** Node.js 22 ESM, DeepSeek Harness Cordis Tool/Skill services, built-in `fetch`, `node:test`, `node:assert/strict`.

**Spec:** `docs/superpowers/specs/2026-09-07-harness-multi-account-analysis-design.md`

## Global Constraints

- Accept 2-10 credential pairs; each username is 1-128 characters after trim, each password is 1-1024 characters, and each optional alias is at most 64 Unicode code points.
- Use at most three concurrent account login/query chains.
- Authenticate each account independently and deduplicate only after authentication by `mainAccountId`.
- Do not change or reuse the existing session-level `AuctionAuth` during a portfolio query.
- The plugin must not add persistence, active logging, or result/render echoing for passwords and temporary tokens; all temporary auth objects are cleared on every exit path. Document that credentials typed in chat still enter model context and the Harness's existing event logs unless a separate secret-input channel is added.
- Aggregate monetary values as safe integer cents in `CNY`; reject unsafe addition rather than rounding or silently losing precision.
- Recompute average deal amount and ratios from summed numerators and denominators; never average account averages or percentages.
- Do not expose a portfolio-wide unique participant count.
- Preserve partial success with explicit coverage; all-account failure must not look like a zero-valued successful report.
- Keep all existing Tools and Go APIs backward compatible.
- The runtime Skill must route only multi-account summary/comparison/diagnosis requests, not ordinary single-account queries.

---

### Task 1: Stable Authentication Error Classification

**Files:**
- Modify: `integrations/deepseek-harness/src/auth.mjs`
- Modify: `integrations/deepseek-harness/test/auth.test.mjs`

**Interfaces:**
- Consumes: the existing `AuctionAuth.login(args, signal)` path and backend result code `401005`.
- Produces: exported `AuthenticationFailedError` whose instances have `code === 'AUTH_FAILED'`; existing human-readable login behavior remains unchanged.

- [ ] **Step 1: Add the failing classification test**

Extend the existing failed-login test so callers can classify invalid credentials without matching localized text:

```js
import {
  AuctionAuth,
  AuthenticationFailedError,
  apiOrigin,
} from '../src/auth.mjs';

await assert.rejects(
  auth.login({ username: 'other', password: 'bad' }),
  error => error instanceof AuthenticationFailedError
    && error.code === 'AUTH_FAILED'
    && error.message === '用户名或密码不正确',
);
```

- [ ] **Step 2: Run the focused test and verify it fails**

Run:

```powershell
node --test --test-name-pattern="failed account switch" test/auth.test.mjs
```

Expected: FAIL because `AuthenticationFailedError` is not exported or the thrown error has no stable code.

- [ ] **Step 3: Implement the typed error**

Add beside the existing auth error types:

```js
export class AuthenticationFailedError extends Error {
  constructor() {
    super('用户名或密码不正确');
    this.code = 'AUTH_FAILED';
  }
}
```

In `AuctionAuth.#request`, map backend code `401005` to `new AuthenticationFailedError()` before the generic `ERRORS` lookup. Preserve the existing `clear()`-before-login behavior so a failed switch cannot retain old credentials.

- [ ] **Step 4: Run authentication tests**

Run:

```powershell
node --test test/auth.test.mjs
```

Expected: all authentication and Cordis adapter tests PASS.

- [ ] **Step 5: Commit the isolated change**

```powershell
git add -- integrations/deepseek-harness/src/auth.mjs integrations/deepseek-harness/test/auth.test.mjs
git commit -m "refactor: classify harness authentication failures"
```

---

### Task 2: Pure Portfolio Aggregator

**Files:**
- Create: `integrations/deepseek-harness/src/portfolio-aggregate.mjs`
- Create: `integrations/deepseek-harness/test/portfolio-aggregate.test.mjs`

**Interfaces:**
- Consumes: `Array<{ inputIndex: number, alias: string, user: PublicUser, dashboard: DashboardSnapshot }>` and `{ range: 'today'|'live'|'7d'|'30d' }`.
- Produces: `aggregatePortfolio(successes, options)` returning `{ currency, range, summary, accounts, rankings, topLots, categoryPerformance, trend, riskFlags }` with no credentials, tokens, global participant total, or I/O side effects.

- [ ] **Step 1: Write failing tests for weighted arithmetic and output boundaries**

Create fixtures with two dashboards whose per-account averages and rates differ. Assert exact totals and recomputation:

```js
const report = aggregatePortfolio([
  success('tenant-a', '店铺A', dashboard({
    paidAmount: 10_000,
    payment: [1, 2],
    deal: [2, 4],
    paidOrders: 1,
  })),
  success('tenant-b', '店铺B', dashboard({
    paidAmount: 90_000,
    payment: [9, 10],
    deal: [8, 10],
    paidOrders: 9,
  })),
], { range: '30d' });

assert.equal(report.summary.paidAmount, 100_000);
assert.equal(report.summary.averageDealAmount, 10_000);
assert.deepEqual(report.summary.paymentRate, {
  numerator: 10,
  denominator: 12,
  rate: 10 / 12,
});
assert.equal('participantCount' in report.summary, false);
assert.deepEqual(report.rankings.paidAmount.map(x => x.mainAccountId), [
  'tenant-b',
  'tenant-a',
]);
```

Also cover zero denominators, safe-integer overflow, mismatched currency/range/timezone, stable Top-10 ordering, category and trend bucket merging, and `INCOMPATIBLE_BUCKETS`.

- [ ] **Step 2: Run the aggregator test and verify it fails**

Run:

```powershell
node --test test/portfolio-aggregate.test.mjs
```

Expected: FAIL with `ERR_MODULE_NOT_FOUND` for `portfolio-aggregate.mjs`.

- [ ] **Step 3: Implement checked arithmetic and aggregate primitives**

Create focused helpers and export only the main entry point:

```js
function add(left, right, label) {
  const value = left + right;
  if (!Number.isSafeInteger(value)) throw new Error(`${label} 超出安全整数范围`);
  return value;
}

function ratio(numerator, denominator) {
  return { numerator, denominator, rate: denominator === 0 ? 0 : numerator / denominator };
}

function stableRank(accounts, read) {
  return [...accounts].sort((a, b) =>
    read(b) - read(a)
      || a.mainAccountId.localeCompare(b.mainAccountId, 'en'));
}

export function aggregatePortfolio(successes, { range }) {
  if (!Array.isArray(successes) || successes.length === 0) {
    throw new Error('portfolio 至少需要一个成功账号');
  }
  const accounts = successes.map(projectAccount);
  const summary = aggregateSummary(accounts);
  return {
    currency: 'CNY',
    range,
    summary,
    accounts,
    rankings: buildRankings(accounts),
    topLots: mergeTopLots(accounts),
    categoryPerformance: mergeCategories(accounts),
    trend: mergeTrend(accounts),
    riskFlags: buildRiskFlags(accounts, summary),
  };
}
```

Implement the internal functions referenced above in the same file: `projectAccount` validates and allow-lists one successful snapshot; `aggregateSummary` uses `add` and `ratio`; `buildRankings` sorts account projections; `mergeTopLots` annotates and caps lots at 10; `mergeCategories` groups by category; `mergeTrend` validates identical buckets; and `buildRiskFlags` implements the named rules. Never copy `participantCount` into the portfolio summary.

- [ ] **Step 4: Implement deterministic risk flags**

Generate only the rules from the spec with structured evidence:

```js
{
  code: 'PAYMENT_RATE_BELOW_PORTFOLIO',
  mainAccountId,
  accountLabel,
  actualRate,
  portfolioRate,
  denominator,
  thresholdPoints: 0.2,
}
```

Require denominator `>= 10` for comparative rate flags. Generate `LOT_NO_CONVERSION` only when the account does not report a `lotPerformance` partial error.

- [ ] **Step 5: Run aggregator tests and the existing Dashboard bounds tests**

Run:

```powershell
node --test test/portfolio-aggregate.test.mjs test/dashboard-bounds.test.mjs
```

Expected: all tests PASS.

- [ ] **Step 6: Commit the aggregator**

```powershell
git add -- integrations/deepseek-harness/src/portfolio-aggregate.mjs integrations/deepseek-harness/test/portfolio-aggregate.test.mjs
git commit -m "feat: aggregate multi-account auction dashboards"
```

---

### Task 3: Temporary Multi-Account Orchestration

**Files:**
- Create: `integrations/deepseek-harness/src/portfolio-tool.mjs`
- Create: `integrations/deepseek-harness/test/portfolio-tool.test.mjs`

**Interfaces:**
- Consumes: `getMultiAccountReport(args, { origin, createAuth? }, signal)`, `AuctionAuth`, `AuctionApi`, and `aggregatePortfolio`.
- Produces: normalized composite report `{ status, coverage, ...aggregate, partialErrors }`, or `{ status: 'ALL_ACCOUNTS_FAILED', coverage, partialErrors }`.

- [ ] **Step 1: Write failing normalization tests**

Test valid trimming/deduplication of sections and rejection before any auth creation:

```js
assert.deepEqual(normalizePortfolioArgs({
  accounts: [
    { username: ' merchant-a ', password: 'password-a', alias: ' 店铺A ' },
    { username: 'merchant-b', password: 'password-b' },
  ],
  range: '30d',
  sections: ['summary', 'orderRisks', 'summary'],
}), {
  accounts: [
    { username: 'merchant-a', password: 'password-a', alias: '店铺A', inputIndex: 0 },
    { username: 'merchant-b', password: 'password-b', alias: '', inputIndex: 1 },
  ],
  range: '30d',
  sections: ['summary', 'orderRisks'],
});
```

Reject fewer than 2 or more than 10 accounts, arrays/non-objects, empty or oversized fields, unknown keys, invalid ranges, invalid sections, and pre-aborted signals.

- [ ] **Step 2: Write failing lifecycle, concurrency, and partial-failure tests**

Inject a fake auth factory that records `login`, `authorizedRequest`, and `clear` calls. Cover:

```js
const report = await getMultiAccountReport(args, {
  origin: 'http://127.0.0.1:18080',
  createAuth: () => fakeAuthScenario.shift(),
}, controller.signal);

assert.equal(maximumActiveChains, 3);
assert.equal(fakeAuthScenario.every(auth => auth.clearCalls === 1), true);
assert.equal(report.coverage.requested, 5);
assert.equal(report.coverage.included, 3);
assert.equal(report.status, 'PARTIAL');
assert.doesNotMatch(JSON.stringify(report), /password-a|private-token|raw-backend-message/);
```

Add scenarios for duplicate `mainAccountId`, `AUTH_FAILED`, `PERMISSION_DENIED`, `BACKEND_UNAVAILABLE`, Dashboard partial errors, all accounts failed, cancellation, and a successful composite call that does not receive or mutate any session bundle.

- [ ] **Step 3: Run the Tool tests and verify they fail**

Run:

```powershell
node --test test/portfolio-tool.test.mjs
```

Expected: FAIL with `ERR_MODULE_NOT_FOUND` for `portfolio-tool.mjs`.

- [ ] **Step 4: Implement validation and safe error mapping**

Export:

```js
const ACCOUNT_KEYS = new Set(['username', 'password', 'alias']);
const ROOT_KEYS = new Set(['accounts', 'range', 'sections']);

export function normalizePortfolioArgs(args) {
  if (!args || typeof args !== 'object' || Array.isArray(args)
      || Object.keys(args).some(key => !ROOT_KEYS.has(key))
      || !Array.isArray(args.accounts)
      || args.accounts.length < 2
      || args.accounts.length > 10) {
    throw new Error('portfolio 参数无效');
  }
  const accounts = args.accounts.map((account, inputIndex) =>
    normalizeAccount(account, inputIndex, ACCOUNT_KEYS));
  const { range, sections } = normalizeDashboardArgs({
    range: args.range,
    sections: args.sections,
  });
  return { accounts, range, sections };
}

export async function getMultiAccountReport(
  args,
  { origin, createAuth = value => new AuctionAuth(value) },
  signal,
) {
  const input = normalizePortfolioArgs(args);
  signal?.throwIfAborted();
  const temporaryAuth = new Set();
  const results = new Array(input.accounts.length);
  let cursor = 0;
  async function worker() {
    while (cursor < input.accounts.length) {
      signal?.throwIfAborted();
      const index = cursor++;
      results[index] = await queryAccount(
        input.accounts[index],
        { origin, createAuth, temporaryAuth, range: input.range, sections: input.sections },
        signal,
      );
    }
  }
  try {
    await Promise.all(Array.from(
      { length: Math.min(3, input.accounts.length) },
      () => worker(),
    ));
    signal?.throwIfAborted();
    return finishPortfolioReport(results, input);
  } finally {
    for (const auth of temporaryAuth) auth.clear();
    temporaryAuth.clear();
  }
}
```

Implement the internal `normalizeAccount` to enforce the exact length and closed-key rules. Implement `queryAccount` to create and register one auth object, log in, validate `merchant_owner` plus required read permissions, build `AuctionApi`, query the Dashboard, and return either a private success record or a safe coded failure. Implement `finishPortfolioReport` to deduplicate successes by authenticated `mainAccountId`, call `aggregatePortfolio`, calculate coverage, and choose `OK`, `PARTIAL`, or `ALL_ACCOUNTS_FAILED`.

Map errors strictly by typed `error.code`:

```js
const SAFE_CODES = new Set([
  'AUTH_FAILED',
  'PERMISSION_DENIED',
  'BACKEND_UNAVAILABLE',
  'INVALID_BACKEND_RESPONSE',
]);

function safeFailure(error) {
  return SAFE_CODES.has(error?.code) ? error.code : 'ACCOUNT_QUERY_FAILED';
}
```

Never include `error.message`, stack traces, raw usernames, password values, or tokens in returned errors.

- [ ] **Step 5: Implement the three-worker scheduler and unconditional cleanup**

Create all auth objects only as workers claim accounts, keep them in a `Set`, and clear them in one outer `finally`:

```js
const temporaryAuth = new Set();
try {
  await Promise.all(Array.from(
    { length: Math.min(3, input.accounts.length) },
    () => worker(),
  ));
  signal?.throwIfAborted();
  return buildReport(results, input);
} finally {
  for (const auth of temporaryAuth) auth.clear();
  temporaryAuth.clear();
}
```

After `login`, require `roleCodes.includes('merchant_owner')` and the permissions needed by the selected Dashboard sections. Construct `AuctionApi` from `auth.authorizedRequest.bind(auth)`, query the Dashboard, and deduplicate successful items by authenticated `mainAccountId` in input order before aggregation.

- [ ] **Step 6: Run Tool and auth tests**

Run:

```powershell
node --test test/portfolio-tool.test.mjs test/auth.test.mjs
```

Expected: all tests PASS, including cleanup on abort and all failure paths.

- [ ] **Step 7: Commit the orchestration layer**

```powershell
git add -- integrations/deepseek-harness/src/portfolio-tool.mjs integrations/deepseek-harness/test/portfolio-tool.test.mjs
git commit -m "feat: orchestrate temporary merchant portfolio queries"
```

---

### Task 4: Tool Schema and Runtime Skill Registration

**Files:**
- Create: `integrations/deepseek-harness/src/portfolio-skill.mjs`
- Create: `integrations/deepseek-harness/test/portfolio-skill.test.mjs`
- Modify: `integrations/deepseek-harness/src/schemas.mjs`
- Modify: `integrations/deepseek-harness/src/plugin.mjs`
- Modify: `integrations/deepseek-harness/test/auth.test.mjs`
- Modify: `integrations/deepseek-harness/test/query-tools.test.mjs`

**Interfaces:**
- Consumes: `getMultiAccountReport`, `portfolioParameters`, `portfolioOutput`, and DeepSeek Harness `ctx.skills.register(SkillRegistration)`.
- Produces: registered Tool `auction_get_multi_account_report`; registered runtime Skill `analyzing-auction-portfolio`; plugin injection list `['tools', 'skills']`.

- [ ] **Step 1: Add failing closed-Schema tests**

Assert the Tool input requires only the credential list and constrains every field:

```js
assert.deepEqual(portfolioParameters.required, ['accounts']);
assert.equal(portfolioParameters.additionalProperties, false);
assert.equal(portfolioParameters.properties.accounts.minItems, 2);
assert.equal(portfolioParameters.properties.accounts.maxItems, 10);
assert.deepEqual(
  portfolioParameters.properties.accounts.items.required,
  ['username', 'password'],
);
assert.equal(portfolioParameters.properties.accounts.items.additionalProperties, false);
```

Define a bounded output schema for statuses `OK`, `PARTIAL`, and `ALL_ACCOUNTS_FAILED`. Use `additionalProperties: false` for all security-sensitive envelopes; permit detailed aggregate section objects only where their complete shapes are explicitly declared.

- [ ] **Step 2: Add failing Skill definition tests**

Create `portfolio-skill.test.mjs` and assert:

```js
assert.equal(portfolioSkill.name, 'analyzing-auction-portfolio');
assert.match(portfolioSkill.description, /多个|多账号/);
assert.match(portfolioSkill.description, /单账号.*不要/);
for (const rule of [
  'auction_get_multi_account_report',
  '不要.*自行.*聚合',
  'coverage',
  'partialErrors',
  '统计事实',
  '风险信号',
  '分析推断',
  '建议动作',
  '不要.*密码',
]) assert.match(portfolioSkill.content, new RegExp(rule));
```

This is the deterministic layer of Skill testing. Keep the Skill body concise and do not duplicate the full Tool schema or aggregation implementation.

- [ ] **Step 3: Add failing plugin registration tests**

Update direct `apply()` test contexts to include a fake Skill registry:

```js
function pluginContext() {
  const tools = new Map();
  const skills = new Map();
  const cleanup = [];
  return {
    tools,
    skills,
    cleanup,
    ctx: {
      tools: { register: tool => tools.set(tool.name, tool) },
      skills: {
        register: skill => {
          skills.set(skill.name, skill);
          return () => skills.delete(skill.name);
        },
      },
      effect: setup => cleanup.push(setup()),
    },
  };
}
```

Assert `inject` equals `['tools', 'skills']`, seven Tools are registered, the portfolio Tool executes without consulting the session bundle, the Skill registration has `source: 'runtime'`, and plugin disposal removes the Skill. Add one test that direct `apply({ tools, effect })` fails with a clear “skills service” configuration error.

- [ ] **Step 4: Run registration tests and verify they fail**

Run:

```powershell
node --test test/portfolio-skill.test.mjs test/auth.test.mjs test/query-tools.test.mjs
```

Expected: FAIL because the new schema, Tool, Skill, and `skills` injection do not exist.

- [ ] **Step 5: Implement the schema and Skill content**

Export from `schemas.mjs`:

```js
const accountCredentialSchema = {
  type: 'object',
  additionalProperties: false,
  properties: {
    username: { type: 'string', minLength: 1, maxLength: 128 },
    password: { type: 'string', minLength: 1, maxLength: 1024 },
    alias: { type: 'string', minLength: 1, maxLength: 64 },
  },
  required: ['username', 'password'],
};

export const portfolioParameters = {
  type: 'object',
  additionalProperties: false,
  properties: {
    accounts: {
      type: 'array',
      minItems: 2,
      maxItems: 10,
      items: accountCredentialSchema,
    },
    range: dashboardParameters.properties.range,
    sections: dashboardParameters.properties.sections,
  },
  required: ['accounts'],
};

export const portfolioOutput = {
  schema: { oneOf: [portfolioReportSchema, allAccountsFailedSchema] },
  render: (_args, value) => [{ type: 'text', text: JSON.stringify(value) }],
};
```

Define `portfolioReportSchema` and `allAccountsFailedSchema` immediately above the export as closed objects. The success schema requires `status`, `coverage`, `currency`, `range`, `summary`, `accounts`, `rankings`, `topLots`, `categoryPerformance`, `trend`, `riskFlags`, and `partialErrors`; status is `OK|PARTIAL`. The failure schema requires only `status: ALL_ACCOUNTS_FAILED`, `coverage`, and `partialErrors`. Define every nested coverage, ratio, account projection, lot, category, trend, risk, and safe error object with `additionalProperties: false` and the exact field names produced by Tasks 2 and 3.

Create `portfolio-skill.mjs`:

```js
export const portfolioSkill = Object.freeze({
  name: 'analyzing-auction-portfolio',
  description: '汇总、比较或诊断多个独立竞拍商家账号时使用；单账号查询或一次普通 Tool 即可完成时不要使用。',
  source: 'runtime',
  content: `# 多商家账号经营分析

仅在用户要求汇总、比较或诊断至少两个独立商家账号时使用。
凭据完整后调用 auction_get_multi_account_report，不要循环切换单账号登录，也不要自行聚合金额、平均值或百分比。
先检查 status、coverage、partialErrors 和 currency，再解释结果。数据不完整时明确说明 included/requested，不能称为全部账号。
回答依次区分统计事实、风险信号、分析推断和建议动作。系统 riskFlags 才是确定风险；其他解释标注为推断。
不要复述、保存或在后续调用中复制密码。需要单账号明细时，重新走正常的单账号登录与查询流程。`,
});
```

- [ ] **Step 6: Register the runtime Skill and composite Tool**

In `plugin.mjs`:

```js
export const inject = ['tools', 'skills'];

export function apply(ctx, config = {}) {
  if (!ctx.skills?.register) throw new Error('auction-backoffice 需要 Harness skills service');
  const origin = apiOrigin(config.apiBaseUrl ?? 'http://127.0.0.1:18080');
  ctx.effect(() => ctx.skills.register(portfolioSkill));
  // existing session setup and Tool registrations stay unchanged
  ctx.tools.register({
    name: 'auction_get_multi_account_report',
    description: '汇总、比较或诊断多个独立商家主账号……',
    parameters: portfolioParameters,
    output: portfolioOutput,
    execute: (args, exec) => getMultiAccountReport(args, { origin }, exec.signal),
  });
}
```

The Tool execute path must not call `client(exec)` and therefore cannot read or alter the current session auth bundle. It still requires the Harness execution signal but does not need an authenticated session.

- [ ] **Step 7: Run all plugin unit tests**

Run:

```powershell
npm test
```

Expected: all tests in `integrations/deepseek-harness/test/*.test.mjs` PASS.

- [ ] **Step 8: Commit Schema, Tool registration, and Skill**

```powershell
git add -- integrations/deepseek-harness/src/schemas.mjs integrations/deepseek-harness/src/plugin.mjs integrations/deepseek-harness/src/portfolio-skill.mjs integrations/deepseek-harness/test/portfolio-skill.test.mjs integrations/deepseek-harness/test/auth.test.mjs integrations/deepseek-harness/test/query-tools.test.mjs
git commit -m "feat: register auction portfolio analysis skill"
```

---

### Task 5: Behavioral Baseline, Runtime Smoke, and Full Verification

**Files:**
- Modify: `integrations/deepseek-harness/scripts/runtime-smoke.mjs`
- Modify: `integrations/deepseek-harness/README.md`
- Create: `integrations/deepseek-harness/test/portfolio-behavior-cases.md`

**Interfaces:**
- Consumes: the seven registered Tool definitions, runtime Skill catalog, local Harness composition, and existing plugin test command.
- Produces: repeatable behavioral cases, a runtime assertion that the Skill is discoverable, and operator documentation for one-time multi-account analysis.

- [ ] **Step 1: Record the no-Skill behavioral baseline**

Before enabling the runtime Skill in a controlled Harness prompt run, record outcomes for these cases in `portfolio-behavior-cases.md`:

```markdown
| Case | Prompt intent | Failure to watch for |
|---|---|---|
| P1 | Three-account 30-day summary | Repeated single-account login; model arithmetic |
| P2 | Three-account payment comparison | Averaging percentages; missing denominators |
| P3 | One account fails authentication | Claiming full coverage |
| P4 | Single-account GMV | Loading portfolio workflow unnecessarily |
| P5 | Diagnose abnormal lots | Treating inference as backend fact |
```

For each case, capture selected Tool names and whether the failure occurred. Do not store real passwords; use local fixture credentials only.

- [ ] **Step 2: Add a failing runtime smoke assertion**

Extend `runtime-smoke.mjs` to obtain the visible Skill catalog for the smoke agent and assert exactly one `analyzing-auction-portfolio` definition is discoverable and loadable. Also assert the Tool inventory contains `auction_get_multi_account_report` and still contains the six previous Tools.

- [ ] **Step 3: Run the smoke check and verify the new assertion fails before final wiring**

Run the existing smoke command documented in `README.md` from the sibling Harness root with `integrations/deepseek-harness/cordis.yml`.

Expected before final wiring: FAIL because the smoke script does not yet observe the new Skill/Tool. After Tasks 1-4 are present, rerun and expect PASS.

- [ ] **Step 4: Document secure use and output limitations**

Add a concise README section that states:

- users provide 2-10 credential pairs for one invocation;
- credentials are sent to the existing login endpoint and are not persisted or echoed by the plugin;
- conversation/model infrastructure may still retain text the user typed, so production use should prefer a secret-input surface when available;
- the report omits a cross-account unique participant total;
- partial failures are reported through coverage and `partialErrors`;
- the composite query does not change the current single-account login.

- [ ] **Step 5: Run post-Skill behavioral cases**

Repeat P1-P5 with the Skill available. Record pass/fail based on observable behavior:

- P1 uses `auction_get_multi_account_report` once.
- P2 reports a weighted ratio with numerator and denominator.
- P3 states `included/requested` coverage.
- P4 uses `auction_get_dashboard`, not the portfolio Tool.
- P5 labels non-`riskFlags` explanations as inference.

If model behavior still fails, make the smallest evidence-backed change to `portfolio-skill.mjs`, rerun the affected case, and record the result.

- [ ] **Step 6: Run complete automated verification**

Run:

```powershell
npm test
node --check src/auth.mjs
node --check src/auction-client.mjs
node --check src/query-tools.mjs
node --check src/portfolio-aggregate.mjs
node --check src/portfolio-tool.mjs
node --check src/portfolio-skill.mjs
node --check src/schemas.mjs
node --check src/plugin.mjs
git diff --check
```

Expected: every test and syntax check PASS; `git diff --check` produces no output. If unrelated dirty files produce pre-existing diff warnings, rerun `git diff --check -- integrations/deepseek-harness docs/superpowers` and record that narrowed result.

- [ ] **Step 7: Review the final diff for credential leakage and scope**

Run:

```powershell
git diff -- integrations/deepseek-harness docs/superpowers
rg -n "console\.|password|accessToken|Authorization" integrations/deepseek-harness/src integrations/deepseek-harness/test
```

Expected: password occurrences are limited to closed input schemas, login request construction, test fixtures, and explicit non-disclosure instructions; no logging statement prints Tool arguments, credentials, or tokens.

- [ ] **Step 8: Commit documentation and runtime verification**

```powershell
git add -- integrations/deepseek-harness/scripts/runtime-smoke.mjs integrations/deepseek-harness/README.md integrations/deepseek-harness/test/portfolio-behavior-cases.md
git commit -m "docs: verify multi-account harness analysis"
```

- [ ] **Step 9: Final repository status check**

```powershell
git status --short
git log -6 --oneline
```

Expected: this feature's files are committed. Pre-existing user changes outside the feature remain untouched and retain their original modified/untracked status.
