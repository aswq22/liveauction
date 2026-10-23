# DeepSeek Harness Auction Statistics Queries Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add authoritative merchant dashboard statistics, lot-price lookup, and all-room live-lot queries to the Go auction backend and expose them as natural-language-friendly DeepSeek Harness tools.

**Architecture:** The Go backend owns tenant scoping, permissions, time windows, and aggregation. The Harness plugin keeps per-session authentication, translates bounded tool inputs into API calls, resolves ambiguity within the authenticated merchant, and returns privacy-safe JSON.

**Tech Stack:** Go 1.24, Kratos HTTP/gRPC generation, GORM/MySQL, Go tests, Node.js 22 ESM, Cordis/DeepSeek Harness, Node test runner.

**Spec:** `docs/superpowers/specs/2026-09-06-harness-statistics-query-design.md`

## Global Constraints

- “Today” uses `Asia/Shanghai` and returns a half-open range plus `snapshotAtUnixMs`.
- Money remains integer minor units with `currency: "CNY"`; never use floating-point money arithmetic.
- Tenant scope comes only from authenticated claims; APIs and tools never accept `mainAccountId`.
- Dashboard reads require `realtime.view`, `lot.view_admin`, and `order.manage` on every request.
- Ambiguity responses contain at most five candidates; detailed result arrays are bounded.
- Read tools never expose tokens, passwords, buyer IDs, phone numbers, or addresses.
- `NO_LIVE_LOT` is a successful empty result; non-core dashboard failures use `partialErrors`.
- Existing login, login-status, and team-member creation must remain compatible.
- Use TDD: observe each focused test fail, implement minimally, then run broader regression tests.

## File Map

- Create `live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard.go` and `_test.go` for domain types, ranges, and use-case behavior.
- Modify backend `repo.go` and `usecase.go` to attach `DashboardRepository`.
- Create `internal/data/dashboard_repo.go` and `_test.go` for tenant-scoped aggregates.
- Modify `internal/data/models.go` and `store.go` to remove the legacy one-room-per-main-account unique index so the confirmed all-room behavior is representable.
- Modify `api/auction/service/v1/auction.proto` and regenerate its three Go bindings.
- Modify `internal/service/auction_contract.go`; create `internal/service/dashboard_test.go`.
- Modify Harness `src/auth.mjs`; create `src/auction-client.mjs`, `src/schemas.mjs`, and `src/query-tools.mjs`.
- Modify Harness `src/plugin.mjs`, unit tests, runtime smoke, README, and package version.

---

### Task 1: Dashboard Domain and Shanghai Time Windows

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/biz/auction/repo.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/biz/auction/usecase.go`

**Interfaces:**
- Consumes: existing `AuctionUsecase` repository attachment and `apperr.ErrInvalidArgument`.
- Produces: `DashboardRange`, `DashboardSection`, `DashboardQuery`, `DashboardSnapshot`, `DashboardRepository.GetDashboard`, and `AuctionUsecase.GetDashboard`.

- [ ] **Step 1: Write failing domain tests**

Test exact Shanghai midnight, `7d`, `30d`, midnight boundary, default range, section deduplication, unknown values, and repository delegation. Core example:

```go
func TestNormalizeDashboardQueryTodayShanghai(t *testing.T) {
	now := time.Date(2026, 9, 6, 14, 30, 0, 0, time.FixedZone("input", 8*60*60))
	got, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRangeToday, NowUnixMs: now.UnixMilli()})
	if err != nil { t.Fatal(err) }
	wantStart := time.Date(2026, 9, 6, 0, 0, 0, 0, time.FixedZone("CST", 8*60*60)).UnixMilli()
	if got.Timezone != "Asia/Shanghai" || got.StartUnixMs != wantStart || got.EndUnixMs != now.UnixMilli() {
		t.Fatalf("normalized range = %#v", got)
	}
}
```

- [ ] **Step 2: Run the test and verify failure**

Run `go test ./app/auction/service/internal/biz/auction -run 'TestNormalizeDashboard|TestAuctionUsecaseGetDashboard' -count=1 -v` from `live-auction-bid-backend`.

Expected: build failure because dashboard symbols do not exist.

- [ ] **Step 3: Define exact domain contracts**

```go
type DashboardRange string
const (
	DashboardRangeToday DashboardRange = "today"
	DashboardRangeLive DashboardRange = "live"
	DashboardRange7D DashboardRange = "7d"
	DashboardRange30D DashboardRange = "30d"
)
type DashboardSection string
const (
	DashboardSectionSummary DashboardSection = "summary"
	DashboardSectionFunnel DashboardSection = "funnel"
	DashboardSectionTrend DashboardSection = "trend"
	DashboardSectionTopLots DashboardSection = "topLots"
	DashboardSectionCategories DashboardSection = "categoryPerformance"
	DashboardSectionOrderRisks DashboardSection = "orderRisks"
	DashboardSectionLotPerformance DashboardSection = "lotPerformance"
)
type DashboardQuery struct {
	MainAccountID string
	Range DashboardRange
	Timezone string
	Sections []DashboardSection
	NowUnixMs, StartUnixMs, EndUnixMs int64
}
type DashboardRepository interface {
	GetDashboard(context.Context, DashboardQuery) (DashboardSnapshot, error)
}
```

Add typed summary, funnel, trend, top-lot, category, risk, lot-performance, range, and partial-error structs. Ratios carry numerator and denominator counts. `NormalizeDashboardQuery` forces `Asia/Shanghai`, computes `[start,end)`, leaves `live` start resolution to the repository, defaults empty sections to all, and rejects unknown values.

- [ ] **Step 4: Attach repository and implement use-case delegation**

Add `dashboard DashboardRepository` to `AuctionUsecase`, assign it when `lots` implements the interface, and implement:

```go
func (uc *AuctionUsecase) GetDashboard(ctx context.Context, query DashboardQuery) (DashboardSnapshot, error) {
	if uc.dashboard == nil { return DashboardSnapshot{}, errors.New("dashboard repository is required") }
	normalized, err := NormalizeDashboardQuery(query)
	if err != nil { return DashboardSnapshot{}, err }
	return uc.dashboard.GetDashboard(ctx, normalized)
}
```

- [ ] **Step 5: Verify and commit**

Run the focused command from Step 2; expect PASS. Then stage only the four listed files and commit `feat: define auction dashboard query domain`.

### Task 2: Tenant-Scoped Data Aggregation

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/data/dashboard_repo.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/dashboard_repo_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/models.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/store.go`

**Interfaces:**
- Consumes: Task 1 dashboard domain types and existing auction/order/bid/participant GORM models.
- Produces: `Store.GetDashboard(context.Context, auction.DashboardQuery) (auction.DashboardSnapshot, error)`.

- [ ] **Step 1: Write failing SQLite-backed tests**

Migrate the required models and seed two merchants, two rooms for merchant A, repeated bidder participation, paid/pending/cancelled/expired/failed orders, boundary records, duplicate amounts, and 600 qualifying records. Assert exact GMV, pending value, rates, unique participants, stable Top 10, category totals, risk counts, zero denominators, no 500-row cap, and absence of merchant B data. Add a migration regression test proving two rooms with one `main_account_id` can coexist:

```go
snapshot, err := store.GetDashboard(ctx, auction.DashboardQuery{
	MainAccountID: "merchant-a", Range: auction.DashboardRangeToday,
	Timezone: "Asia/Shanghai", StartUnixMs: midnight, EndUnixMs: nowMs, NowUnixMs: nowMs,
})
if err != nil { t.Fatal(err) }
if snapshot.Summary.GMVAmount != 12_880 || snapshot.Summary.PendingAmount != 3_200 { t.Fatalf("%#v", snapshot.Summary) }
if snapshot.Summary.ParticipantCount != 2 { t.Fatalf("participants=%d", snapshot.Summary.ParticipantCount) }
```

- [ ] **Step 2: Run and observe failure**

Run `go test ./app/auction/service/internal/data -run 'TestDashboard' -count=1 -v`.

Expected: build failure because `Store.GetDashboard` is absent.

- [ ] **Step 3: Implement core summary and live range**

First remove `uniqueIndex:uidx_room_main_account` from `AuctionRoomModel.MainAccountID`. In `Store` migration, detect and drop the legacy `uidx_room_main_account` index after `AutoMigrate`; retain the existing non-unique composite `idx_room_main_status`. This is required because the accepted product behavior permits multiple rooms per merchant.

Every dashboard query begins with `main_account_id = ?`. Resolve `live` from the minimum positive `auction_rooms.live_started_at_unix_ms` for active rooms, falling back to Shanghai midnight. Aggregate paid values by `paid_at_unix_ms`; aggregate created/pending/cancelled counts by `created_at_unix_ms`. Set expiring-soon to `(now, now+10m]`; use effective expired state when expiry is at or before now.

```go
type dashboardOrderAggregate struct {
	PaidAmount, PendingAmount int64
	PaidCount, PayableCount, OrderCount, CancelledCount int64
	ExpiringSoon, Expired, PaymentFailed int64
}
```

Use conditional SQL aggregates over `user_orders` with `source = userOrderSourceAuction` and tenant filter. Compute percentages from integer counts; return 0 for zero denominators.

- [ ] **Step 4: Implement bounded panel helpers**

```go
func (s *Store) dashboardFunnel(context.Context, auction.DashboardQuery) (auction.DashboardFunnel, error)
func (s *Store) dashboardTrend(context.Context, auction.DashboardQuery) ([]auction.DashboardTrendPoint, error)
func (s *Store) dashboardTopLots(context.Context, auction.DashboardQuery) ([]auction.DashboardTopLot, error)
func (s *Store) dashboardCategories(context.Context, auction.DashboardQuery) ([]auction.DashboardCategoryPerformance, error)
func (s *Store) dashboardLotPerformance(context.Context, auction.DashboardQuery) ([]auction.DashboardLotPerformance, error)
```

Use 24 hourly buckets for today and natural-day buckets for 7d/30d, zero-fill missing buckets, `LIMIT 10` for Top 10, `paid_amount DESC, lot_id ASC` tie order, `COUNT(DISTINCT user_id)` for participants, `未分类` for blank categories, and cap lot performance at 100. A non-core helper failure appends `DATA_INCOMPLETE`; summary failure aborts.

- [ ] **Step 5: Verify and commit**

Run `go test ./app/auction/service/internal/data -run 'TestDashboard|TestStoreAllowsMultipleRoomsPerMainAccount' -count=1 -v` and `go test ./app/auction/service/internal/biz/auction ./app/auction/service/internal/data -count=1`; expect PASS. Commit the two new files plus `models.go` and `store.go` as `feat: aggregate merchant dashboard statistics`.

### Task 3: Authenticated Dashboard HTTP/RPC Contract

**Files:**
- Modify: `live-auction-bid-backend/api/auction/service/v1/auction.proto`
- Regenerate: `auction.pb.go`, `auction_grpc.pb.go`, `auction_http.pb.go` in the same directory
- Modify: `live-auction-bid-backend/app/auction/service/internal/service/auction_contract.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/service/dashboard_test.go`

**Interfaces:**
- Consumes: Task 1–2 `AuctionUsecase.GetDashboard` and result types.
- Produces: `GET /api/admin/dashboard` via `GetAdminDashboard`.

- [ ] **Step 1: Write failing permission and mapping tests**

Use a fake repository. Assert each missing permission refuses before repository access, all three permissions succeed, claim-derived `merchant-a` reaches the repository, and all integer/ratio/range/partial-error fields map exactly. The request deliberately contains no tenant field.

- [ ] **Step 2: Run and observe failure**

Run `go test ./app/auction/service/internal/service -run 'TestGetAdminDashboard' -count=1 -v`; expect missing RPC/message build errors.

- [ ] **Step 3: Add explicit proto messages**

```proto
rpc GetAdminDashboard(GetAdminDashboardRequest) returns (GetAdminDashboardReply) {
  option (google.api.http) = { get: "/api/admin/dashboard" };
}
message GetAdminDashboardRequest {
  string range = 1;
  string timezone = 2;
  repeated string sections = 3;
}
```

Add typed messages for range, summary, funnel, trend point, top lot, category performance, risks, lot performance, and partial error. Use `int64` for money/count/time, `double` for percentages, and field `99` for `ReplyResult result`.

- [ ] **Step 4: Regenerate bindings**

From the backend, run:

```powershell
protoc --proto_path=. --proto_path=third_party --go_out=paths=source_relative:. --go-grpc_out=paths=source_relative:. --go-http_out=paths=source_relative:. api/auction/service/v1/auction.proto api/auction/service/v1/user.proto api/auction/service/v1/shop.proto
```

Do not hand-edit generated files.

- [ ] **Step 5: Enforce all permissions and map fields**

Implement a helper that requires `PermissionRealtimeView`, `PermissionLotViewAdmin`, and `PermissionOrderManage` and verifies one non-empty main account. Implement `GetAdminDashboard` by constructing `auctionbiz.DashboardQuery` from range/timezone/sections plus the claim-derived tenant, then explicitly map every domain field into proto messages.

- [ ] **Step 6: Verify and commit**

Run focused service tests, then `go test ./app/auction/service/internal/biz/auction ./app/auction/service/internal/data ./app/auction/service/internal/service -count=1` and `go test ./app/auction/service/... -count=1`; expect PASS. Commit only the proto, three generated bindings, service adapter, and test as `feat: expose authenticated dashboard API`.

### Task 4: Token-Safe Harness Auction Client

**Files:**
- Modify: `integrations/deepseek-harness/src/auth.mjs`
- Create: `integrations/deepseek-harness/src/auction-client.mjs`
- Create: `integrations/deepseek-harness/test/auction-client.test.mjs`
- Modify: `integrations/deepseek-harness/test/auth.test.mjs`

**Interfaces:**
- Consumes: existing `AuctionAuth.login`, `status`, `createTeamMember`, cancellation, and safe backend error mapping.
- Produces: `AuctionAuth.authorizedRequest(path, {signal})`; `AuctionApi.getDashboard`, `searchLots`, `getLot`, `listRooms`, and `getRoomSnapshot`.

- [ ] **Step 1: Write failing auth-capability and client tests**

Assert unauthenticated calls stop before network I/O, bearer auth is attached after login, a 401 clears state, redirects remain disabled, unknown response shapes fail closed, query values are URL encoded, arrays are bounded, and sensitive fields are absent:

```js
await assert.rejects(auth.authorizedRequest('/api/admin/dashboard', { signal }), /尚未登录/);
await auth.login({ username: 'merchant', password: 'fixture-password' });
const result = await auth.authorizedRequest('/api/admin/dashboard?range=today', { signal });
assert.doesNotMatch(JSON.stringify(result), /private-fixture-token|fixture-password/);
```

- [ ] **Step 2: Run and observe failure**

Run `npm test -- --test-name-pattern="authorized request|auction api"` from `integrations/deepseek-harness`.

Expected: FAIL because the capability and client do not exist.

- [ ] **Step 3: Add a read-only authenticated capability**

Refactor the current transport without changing login behavior, then add:

```js
async authorizedRequest(path, { signal } = {}) {
  const state = this.#state;
  if (!state || state.expires <= Date.now()) {
    this.clear();
    throw new Error('尚未登录或登录已过期，请先登录商家后台账号');
  }
  try {
    const data = await this.#request(path, undefined, state.token, signal);
    if (this.#state !== state) throw new Error('登录账号已发生变化，请重新查询');
    return data;
  } catch (error) {
    if (error?.message === '登录已失效，请重新登录' && this.#state === state) this.clear();
    throw error;
  }
}
```

Accept only relative paths beginning `/api/` and GET semantics. Tokens remain private to `AuctionAuth`.

- [ ] **Step 4: Implement the allow-listing API client**

```js
export class AuctionApi {
  constructor(request) { /* reject non-functions */ }
  getDashboard({ range = 'today', sections = [] }, signal) {}
  searchLots({ lotId = '', keyword = '', roomId = '', pageSize = 5 }, signal) {}
  getLot(lotId, signal) {}
  listRooms(signal) {}
  getRoomSnapshot(roomId, signal) {}
}
```

Each method builds a fixed backend path with `URLSearchParams`, validates the `result.code` envelope and field types, and constructs a new allow-listed result. Candidate lists stop at five; recent bids stop at ten. Explicitly drop `mainAccountId`, `buyerUserId`, address/phone fields, leading user IDs, and token-like keys.

- [ ] **Step 5: Verify and commit**

Run `npm test`; expect all old and new tests PASS. Commit the four listed files as `refactor: add authenticated auction query client`.

### Task 5: Dashboard Harness Tool

**Files:**
- Create: `integrations/deepseek-harness/src/schemas.mjs`
- Create: `integrations/deepseek-harness/src/query-tools.mjs`
- Create: `integrations/deepseek-harness/test/query-tools.test.mjs`
- Modify: `integrations/deepseek-harness/src/plugin.mjs`

**Interfaces:**
- Consumes: Task 4 `AuctionApi.getDashboard` and existing per-Harness-session `AuctionAuth` lifecycle.
- Produces: `getDashboard(args, deps, signal)` and Cordis tool `auction_get_dashboard`.

- [ ] **Step 1: Write failing schema and handler tests**

Cover the `today` default, every range/section enum, unknown-key rejection, section deduplication, cancellation forwarding, partial panel errors, integer preservation, and registration:

```js
const value = await getDashboard({}, { api: fakeApi }, signal);
assert.equal(fakeApi.calls[0].range, 'today');
assert.deepEqual(dashboardParameters.properties.range.enum, ['today', 'live', '7d', '30d']);
assert.equal(dashboardParameters.additionalProperties, false);
assert.equal(value.summary.gmvAmount, 128800);
```

- [ ] **Step 2: Run and observe failure**

Run `npm test -- --test-name-pattern="dashboard tool"`; expect missing module/tool failure.

- [ ] **Step 3: Centralize schemas and implement handler**

Move existing account/member schemas from `plugin.mjs` to `schemas.mjs` unchanged. Add `dashboardParameters` with only `range` and `sections`, plus all seven backend section strings. Implement:

```js
export async function getDashboard(args, { api }, signal) {
  const input = normalizeDashboardArgs(args);
  const snapshot = await api.getDashboard(input, signal);
  return { ...snapshot, display: formatDashboardMoney(snapshot, 'zh-CN') };
}
```

The display object adds readable CNY text but never replaces raw integer amounts. Empty sections means all panels.

- [ ] **Step 4: Register with broad natural-language guidance**

Describe `auction_get_dashboard` using these intent terms: “今日工作台、今天咋样、经营数据、GMV、成交额、付款/支付率、成交率、参拍人数、平均成交价、漏斗、趋势、Top 10、品类、订单风险、拍品表现”. State that a named different account requires login/switch first and that this tool itself accepts no credentials.

- [ ] **Step 5: Verify and commit**

Run `npm test`; expect four registered tools and all tests PASS. Commit the four task files as `feat: add harness dashboard query tool`.

### Task 6: Lot Price Lookup and Ambiguity

**Files:**
- Modify: `integrations/deepseek-harness/src/schemas.mjs`
- Modify: `integrations/deepseek-harness/src/query-tools.mjs`
- Modify: `integrations/deepseek-harness/src/plugin.mjs`
- Modify: `integrations/deepseek-harness/test/query-tools.test.mjs`

**Interfaces:**
- Consumes: `AuctionApi.searchLots` and `AuctionApi.getLot`.
- Produces: `getLotPrice(args, deps, signal)` and `auction_get_lot_price`.

- [ ] **Step 1: Write failing resolution and privacy tests**

Cover direct ID, exact title, one fuzzy result, multiple results, no result, room narrowing, empty input, and no leader user ID:

```js
const result = await getLotPrice({ keyword: '茅台' }, { api }, signal);
assert.equal(result.status, 'LOT_AMBIGUOUS');
assert.equal(result.candidates.length, 2);
assert.deepEqual(Object.keys(result.candidates[0]).sort(), ['lotId', 'roomId', 'status', 'title']);
assert.equal(api.getLotCalls.length, 0);
```

- [ ] **Step 2: Run and observe failure**

Run `npm test -- --test-name-pattern="lot price"`; expect missing handler/tool failure.

- [ ] **Step 3: Implement deterministic resolution**

```js
if (input.lotId) return safePrice(await api.getLot(input.lotId, signal));
const found = await api.searchLots({ keyword: input.keyword, roomId: input.roomId, pageSize: 5 }, signal);
const exact = found.filter(x => x.title.localeCompare(input.keyword, 'zh-CN', { sensitivity: 'base' }) === 0);
if (exact.length === 1) return safePrice(await api.getLot(exact[0].lotId, signal));
if (found.length === 1) return safePrice(await api.getLot(found[0].lotId, signal));
if (found.length === 0) return { status: 'LOT_NOT_FOUND', keyword: input.keyword };
return { status: 'LOT_AMBIGUOUS', candidates: found.map(candidateSummary) };
```

Return start/current/min-increment/cap/final prices, status, leading nickname, bid count, room, and update time. Do not add approximate client-side scoring.

- [ ] **Step 4: Register the tool**

Schema accepts only `lotId`, `keyword`, and optional `roomId`; at least one locator is required in handler validation. Description covers “多少钱、当前价、起拍价、最低加价、封顶价、成交价、谁领先、有没有成交” and instructs the model to ask when it receives `LOT_AMBIGUOUS`.

- [ ] **Step 5: Verify and commit**

Run `npm test`; expect PASS. Commit the four modified files as `feat: add harness lot price query`.

### Task 7: All-Room Current Live Lots

**Files:**
- Modify: `integrations/deepseek-harness/src/schemas.mjs`
- Modify: `integrations/deepseek-harness/src/query-tools.mjs`
- Modify: `integrations/deepseek-harness/src/plugin.mjs`
- Modify: `integrations/deepseek-harness/test/query-tools.test.mjs`

**Interfaces:**
- Consumes: `AuctionApi.listRooms` and `AuctionApi.getRoomSnapshot`.
- Produces: `getLiveLots(args, deps, signal)` and `auction_get_live_lots`.

- [ ] **Step 1: Write failing multi-room tests**

Cover all active rooms, explicit room ID, unique exact room name, duplicate room names, one failed snapshot, no current lots, next queued lot, result ordering, and cancellation:

```js
const value = await getLiveLots({}, { api }, signal);
assert.equal(value.status, 'OK');
assert.deepEqual(value.rooms.map(x => x.roomId), ['room-a', 'room-b']);
assert.equal(value.rooms[0].currentLot.currentPrice.amount, 12800);
assert.equal(value.rooms[0].currentLot.leadingUserId, undefined);
```

Assert no-live returns `{status:'NO_LIVE_LOT', rooms:[...]}` instead of throwing. Assert one room failure preserves successful rooms and adds `{roomId, code:'BACKEND_UNAVAILABLE'}`.

- [ ] **Step 2: Run and observe failure**

Run `npm test -- --test-name-pattern="live lots"`; expect missing handler/tool failure.

- [ ] **Step 3: Implement room resolution and bounded fan-out**

Verify an explicit ID against `listRooms`. For a name, prefer case-insensitive exact matches; multiple exact matches return `ROOM_AMBIGUOUS` with at most five `{roomId,name,status}` candidates. With no selector, query every active room using a concurrency cap of four rather than unbounded `Promise.all`.

Return this allow-listed structure per room:

```js
{
  roomId, roomName, snapshotAtUnixMs,
  currentLot: current ? {
    lotId, title, status, startPrice, currentPrice, minIncrement,
    leadingNickname, participantCount, bidCount,
    startedAtUnixMs, endsAtUnixMs,
    remainingMs: Math.max(0, endsAtUnixMs - snapshotAtUnixMs),
  } : null,
  nextLot: next ? { lotId, title, queuePosition, startPrice } : null,
}
```

When `includeRecentBids` is true, cap at ten and keep only nickname, amount, and timestamp. Never return ranking/bid user IDs.

- [ ] **Step 4: Register the tool**

Schema accepts `roomId`, `roomName`, and `includeRecentBids`; no selector means all rooms. Description covers “现在正在拍什么、所有直播间/所有场子、哪个直播间、目前谁领先、还剩多久、最近谁出价”.

- [ ] **Step 5: Verify and commit**

Run `npm test`; expect all six tools and all tests PASS. Commit the four files as `feat: add harness live lot query`.

### Task 8: Cordis Runtime, Documentation, and Full Verification

**Files:**
- Modify: `integrations/deepseek-harness/scripts/runtime-smoke.mjs`
- Modify: `integrations/deepseek-harness/package.json`
- Modify: `integrations/deepseek-harness/README.md`

**Interfaces:**
- Consumes: all six tools and all backend query endpoints.
- Produces: repeatable real-Cordis smoke coverage, version `0.3.0`, and user examples.

- [ ] **Step 1: Extend runtime-smoke fixture and write failing assertions**

Add fixture responses for dashboard, admin lots, lot detail, rooms, and room snapshots. Assert exact registration:

```js
assert.deepEqual(ctx.tools.schemas().map(tool => tool.name).sort(), [
  'auction_create_team_member', 'auction_get_dashboard', 'auction_get_live_lots',
  'auction_get_lot_price', 'auction_login', 'auction_login_status',
]);
```

Execute every query after login; assert another Harness session remains unauthenticated, every backend query receives the fixture bearer token, and serialized outputs contain none of the fixture password/token/buyer ID/phone/address values.

- [ ] **Step 2: Run smoke and observe failure before fixture completion**

From `D:\Go\Auc\deepseek-harness`, run:

```powershell
pnpm exec tsx --tsconfig tsconfig.json D:\Go\Auc\ByteDanceliveauctioni\integrations\deepseek-harness\scripts\runtime-smoke.mjs
```

Expected: FAIL until new routes and assertions are fully implemented.

- [ ] **Step 3: Update metadata and README**

Bump the private package to `0.3.0`. Document setup, tool names, account switching, missing-password follow-up, Shanghai time, integer-money semantics, ambiguity behavior, and these examples:

```text
今天咋样？
只看今天支付率和待付款。
最近七天成交额最高的拍品有哪些？
青花瓷现在多少钱？
所有直播间目前分别在拍什么？
换到 merchant_b 账号查今天工作台。
```

- [ ] **Step 4: Run full backend verification**

```powershell
Set-Location D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend
gofmt -w app/auction/service/internal/biz/auction/dashboard.go app/auction/service/internal/biz/auction/dashboard_test.go app/auction/service/internal/data/dashboard_repo.go app/auction/service/internal/data/dashboard_repo_test.go app/auction/service/internal/service/auction_contract.go app/auction/service/internal/service/dashboard_test.go
go test ./app/auction/service/... -count=1
go test ./... -count=1
```

Expected: both test commands exit 0.

- [ ] **Step 5: Run full Harness verification**

```powershell
Set-Location D:\Go\Auc\ByteDanceliveauctioni\integrations\deepseek-harness
npm test
Set-Location D:\Go\Auc\deepseek-harness
pnpm exec tsx --tsconfig tsconfig.json D:\Go\Auc\ByteDanceliveauctioni\integrations\deepseek-harness\scripts\runtime-smoke.mjs
```

Expected: unit tests exit 0 and smoke output names dashboard, price, live-lot, login, and member creation coverage.

- [ ] **Step 6: Run controlled natural-language acceptance**

Start the backend and Harness using the existing local scripts. Run the README examples and these ambiguity/context cases without recording credentials:

```text
查一下茅台现在多少钱（测试数据中存在多个“茅台”）
看看一号直播间现在拍什么（测试数据中存在重名房间）
今天咋样
付款情况呢
```

Expected: ambiguous targets cause a follow-up; “今天咋样” is backed by all panels; the contextual payment follow-up requests only relevant data; no response repeats credentials.

- [ ] **Step 7: Inspect and commit final integration files**

Run `git diff --check`, `git status --short`, and `git diff -- integrations/deepseek-harness`. Stage only runtime smoke, package metadata, and README, then commit `docs: verify harness statistics queries`. Leave all unrelated pre-existing worktree changes untouched.

## Self-Review Results

- Spec coverage: authoritative aggregation is Tasks 1–3; safe session/API access is Task 4; dashboard, price, and all-room live queries are Tasks 5–7; model-facing language, runtime integration, and documentation are Task 8.
- Placeholder scan: every task names tests, interfaces, commands, expected outcomes, and commits; no deferred implementation markers remain.
- Type consistency: backend range/section names and Harness enum strings match; tool/API names stay stable throughout; raw amount fields remain integers.
- Safety check: no tool schema accepts tenant IDs or credentials, all result construction is allow-listed, and existing dirty-worktree files are never broadly staged.
