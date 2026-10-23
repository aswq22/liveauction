# DeepSeek Harness Single-Account Diagnostics Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add deterministic single-account auction diagnostics to the existing Dashboard API and expose the evidence through the DeepSeek Harness Tool and a focused runtime Skill.

**Architecture:** The Go backend owns diagnostic windows, thresholds, tenant-scoped aggregation, and deterministic anomaly evaluation. The existing Dashboard response gains a `diagnostics` section; the Harness plugin validates and preserves that structure, while a runtime Skill tells the model how to separate system facts, inference, and suggested action. Tests use pure values, temporary SQLite, local HTTP fixtures, and an explicit dev-only seed command, so no populated production database is required.

**Tech Stack:** Go 1.x, GORM, SQLite/MySQL, Protocol Buffers/Kratos HTTP bindings, Node.js 22 ESM, `node:test`, Cordis/DeepSeek Harness runtime smoke tests.

**Spec:** `docs/superpowers/specs/2026-09-21-harness-single-account-diagnostics-design.md`

## Global Constraints

- The authenticated claim remains the only source of `mainAccountId`; neither Dashboard nor diagnostics accepts a tenant identifier from the caller.
- Money remains a non-negative integer number of CNY cents; rates retain numerator and denominator and are never averaged as percentages.
- `today`, `7d`, and `30d` use `Asia/Shanghai`; `live` returns `baselineStatus=NOT_SUPPORTED` in V1.
- Historical failure must not erase successful current Dashboard data or fixed-rule diagnoses.
- Harness and the Skill never recalculate diagnostic thresholds or upgrade severity.
- No diagnostic result triggers a write action.
- The demo seed runs only when `AUCTION_ENV` is `dev` or `test`, owns only `diagnostic-demo-` identifiers, and refuses any other environment.
- Preserve all pre-existing uncommitted changes. Several backend, frontend, generated agent-protocol, and local Harness launcher files are already dirty; inspect each target diff before editing and stage only task-owned paths.
- Do not stage `integrations/deepseek-harness/cordis.yml` or `integrations/deepseek-harness/start.ps1`; both are local untracked files.

## Review Focus

- A request for `sections=["diagnostics"]` must internally load its dependencies without falsely returning those panels as explicitly requested; Task 2 pins this behavior.
- A foreign tenant with extreme historical values must never affect the authenticated merchant's baseline; Task 2 seeds and asserts cross-tenant exclusion.
- A ratio at the exact minimum sample and exact drop threshold must trigger once, while one unit below either boundary must not; Task 1 pins all threshold edges.
- `PARTIAL` and `INSUFFICIENT_DATA` must not be normalized into an empty “healthy” result by the JavaScript parser or Skill; Tasks 4 and 5 pin this distinction.
- Re-running or cleaning the demo seed must affect only rows with the owned prefix, including child order items and bids; Task 6 pins idempotence and cleanup ownership.

---

### Task 1: Pure diagnostic domain model and evaluator

**Files:**
- Modify: `live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard_diagnostics.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard_diagnostics_test.go`

**Interfaces:**
- Consumes: existing `DashboardQuery`, `DashboardSummary`, `DashboardFunnel`, `DashboardOrderRisks`, `DashboardLotPerformance`, and `DashboardPartialError`.
- Produces: `DashboardDiagnosticConfig`, `DashboardDiagnosticFacts`, `DashboardBaselinePlan`, `DashboardDiagnostics`, `BuildDashboardBaselinePlan(DashboardQuery)`, and `EvaluateDashboardDiagnostics(DashboardDiagnosticFacts, DashboardDiagnosticConfig) DashboardDiagnostics`.

- [ ] **Step 1: Add failing tests for range planning and configuration validation**

Create table tests that pin `today` to seven prior same-elapsed-day windows, `7d` and `30d` to the immediately preceding equal-duration window, and `live` to no baseline windows:

```go
const dashboardDiagnosticNoon int64 = 1788667200000 // 2026-09-06 12:00 Asia/Shanghai

func TestBuildDashboardBaselinePlanUsesComparableShanghaiWindows(t *testing.T) {
	today, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRangeToday, NowUnixMs: dashboardDiagnosticNoon})
	if err != nil { t.Fatal(err) }
	plan := BuildDashboardBaselinePlan(today)
	if plan.Status != DashboardBaselineAvailable || len(plan.Windows) != 7 {
		t.Fatalf("today plan = %#v", plan)
	}
	if got := plan.Windows[0].EndUnixMs - plan.Windows[0].StartUnixMs; got != 12*60*60*1000 {
		t.Fatalf("today baseline width = %d", got)
	}
	live, err := NormalizeDashboardQuery(DashboardQuery{Range: DashboardRangeLive, NowUnixMs: dashboardDiagnosticNoon})
	if err != nil { t.Fatal(err) }
	if got := BuildDashboardBaselinePlan(live); got.Status != DashboardBaselineNotSupported || len(got.Windows) != 0 {
		t.Fatalf("live plan = %#v", got)
	}
}

func TestDashboardDiagnosticConfigRejectsInvalidThresholds(t *testing.T) {
	cfg := DefaultDashboardDiagnosticConfig()
	cfg.PaymentRateDropPoints = 1.1
	if err := cfg.Validate(); err == nil {
		t.Fatal("invalid rate threshold accepted")
	}
}
```

- [ ] **Step 2: Run the focused domain tests and verify they fail**

Run from `live-auction-bid-backend`:

```powershell
go test ./app/auction/service/internal/biz/auction -run 'TestBuildDashboardBaselinePlan|TestDashboardDiagnosticConfig' -count=1
```

Expected: FAIL because the diagnostic types and functions do not exist.

- [ ] **Step 3: Add the domain types, defaults, and baseline planner**

Add `DashboardSectionDiagnostics` to `dashboard.go`, include it in `allDashboardSections`, and add `Diagnostics DashboardDiagnostics` to `DashboardSnapshot`. Define these stable interfaces in `dashboard_diagnostics.go`:

```go
type DashboardDiagnosticConfig struct {
	PaymentRateDropPoints      float64
	DealRateDropPoints         float64
	PaidAmountDropRatio        float64
	ParticipantCountDropRatio  float64
	AverageDealAmountDropRatio float64
	CurrentRateMinDenominator  int64
	BaselineRateMinDenominator int64
	CurrentAverageMinPaid      int64
	BaselineAverageMinPaid     int64
	HighSeverityMultiplier     float64
	HighRiskCount              int64
	MaxAnomalies               int
}

type DashboardDiagnosticWindow struct { StartUnixMs, EndUnixMs int64 }
type DashboardBaselinePlan struct {
	Status  string
	Windows []DashboardDiagnosticWindow
}
type DashboardDiagnosticMetric struct {
	Amount      *int64
	Count       *int64
	Ratio       *DashboardRatio
}
type DashboardAnomaly struct {
	Code, Severity, Basis, Metric, EvidenceCode, RecommendedActionCode string
	Actual, Baseline DashboardDiagnosticMetric
	Deviation, Threshold float64
	SampleSize, BaselineSampleSize int64
	LotID, LotTitle string
}
type DashboardDiagnostics struct {
	Status, BaselineStatus string
	CurrentWindow DashboardDiagnosticWindow
	BaselineWindows []DashboardDiagnosticWindow
	MissingDependencies []DashboardSection
	Anomalies []DashboardAnomaly
	Truncated bool
}
type DashboardDiagnosticFacts struct {
	Current DashboardSnapshot
	Baseline DashboardSnapshot
	BaselineWindows []DashboardDiagnosticWindow
	BaselineStatus string
	MissingDependencies []DashboardSection
}
```

`DefaultDashboardDiagnosticConfig()` must return the approved thresholds: 0.20 payment/deal points, 0.30 paid amount/participants, 0.25 average deal amount, current/baseline ratio denominators 10/30, current/baseline paid counts 5/15, high-severity multiplier 1.5, high-risk count 3, and maximum 50 anomalies. `Validate()` rejects non-finite or out-of-range ratios, non-positive samples, a multiplier below 1, and a maximum outside 1..100.

- [ ] **Step 4: Re-run the planning/config tests and verify they pass**

Run:

```powershell
go test ./app/auction/service/internal/biz/auction -run 'TestBuildDashboardBaselinePlan|TestDashboardDiagnosticConfig' -count=1
```

Expected: PASS.

- [ ] **Step 5: Add failing evaluator tests for fixed, historical, boundary, partial, ordering, and truncation behavior**

Use literal current and baseline facts. Include tests named exactly:

```go
func TestEvaluateDashboardDiagnosticsCombinesFixedAndHistoricalRules(t *testing.T)
func TestEvaluateDashboardDiagnosticsRequiresMinimumSamples(t *testing.T)
func TestEvaluateDashboardDiagnosticsIncludesExactThresholdBoundary(t *testing.T)
func TestEvaluateDashboardDiagnosticsSkipsOnlyMissingDependencies(t *testing.T)
func TestEvaluateDashboardDiagnosticsOrdersAndTruncatesDeterministically(t *testing.T)
func TestEvaluateDashboardDiagnosticsDoesNotTreatDataQualityAsBusinessHealth(t *testing.T)
```

The main fixture must use payment rate 9/20 against 117/150, deal rate 4/10 against 24/30, current paid amount 7000 against comparable baseline 10000, current participants 7 against 10, two payment failures, one expired order, and one bid-bearing zero-conversion lot. Assert exact codes, severities, evidence codes, actual/baseline ratios, thresholds, sample sizes, and action codes.

- [ ] **Step 6: Run the evaluator tests and verify they fail**

Run:

```powershell
go test ./app/auction/service/internal/biz/auction -run 'TestEvaluateDashboardDiagnostics' -count=1
```

Expected: FAIL because the evaluator does not yet emit anomalies.

- [ ] **Step 7: Implement the pure evaluator**

Implement private helpers for safe rates, relative deviation, severity, stable sorting, and bounded append. Fixed rules emit `PAYMENT_FAILED`, `EXPIRED_ORDERS`, `NO_BID_LOTS`, `UNPAID_SETTLED_LOTS`, and `LOT_NO_CONVERSION`. Historical rules emit `PAYMENT_RATE_DROP`, `DEAL_RATE_DROP`, `PAID_AMOUNT_DROP`, `PARTICIPANT_COUNT_DROP`, and `AVERAGE_DEAL_AMOUNT_DROP` only after their sample guards pass.

Use stable severity order `HIGH`, `MEDIUM`, `LOW`; then code; then lot ID. `DATA_INCOMPLETE` affects `MissingDependencies` and status but is not inserted as a business anomaly. Exact thresholds use `>=`, not `>`.

- [ ] **Step 8: Run all auction domain tests**

Run:

```powershell
go test ./app/auction/service/internal/biz/auction -count=1
```

Expected: PASS.

- [ ] **Step 9: Commit the domain evaluator**

```powershell
git add -- live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard.go live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard_diagnostics.go live-auction-bid-backend/app/auction/service/internal/biz/auction/dashboard_diagnostics_test.go
git commit -m "feat: define dashboard anomaly diagnostics"
```

### Task 2: Tenant-scoped current facts, historical baseline, and graceful degradation

**Files:**
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/store.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/dashboard_repo.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/dashboard_repo_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/cmd/server/main.go`
- Test: `live-auction-bid-backend/app/auction/service/cmd/server/main_test.go`

**Interfaces:**
- Consumes: Task 1 `BuildDashboardBaselinePlan` and `EvaluateDashboardDiagnostics`.
- Produces: `data.Config.DashboardDiagnostics auction.DashboardDiagnosticConfig`; `Store.GetDashboard` populates `DashboardSnapshot.Diagnostics` when requested; `dashboardDiagnosticConfigFromEnv(func(string) string) (auction.DashboardDiagnosticConfig, error)`.

- [ ] **Step 1: Add failing repository tests for comparable windows and tenant isolation**

Extend the existing SQLite fixture with deterministic current and historical orders, lots, and bids. Add tests named:

```go
func TestDashboardDiagnosticsCombinesSevenTodayBaselineWindows(t *testing.T)
func TestDashboardDiagnosticsUsesPreviousEqualWindowForSevenAndThirtyDays(t *testing.T)
func TestDashboardDiagnosticsExcludesForeignTenantHistory(t *testing.T)
func TestDashboardDiagnosticsLiveUsesRulesWithoutHistoricalBaseline(t *testing.T)
```

The tenant-isolation test must insert a foreign merchant with a 100% payment rate and very large paid amount; assert the authenticated merchant baseline is unchanged.

- [ ] **Step 2: Run the repository tests and verify they fail**

Run:

```powershell
go test ./app/auction/service/internal/data -run 'TestDashboardDiagnostics' -count=1
```

Expected: FAIL because repository diagnostics are not populated.

- [ ] **Step 3: Add diagnostic configuration to Store and parse environment overrides**

Add a `dashboardDiagnostics auction.DashboardDiagnosticConfig` field to `Store`, validate it in `NewStore`, and pass it from `main.go`. Implement these environment keys with the Task 1 defaults when absent:

```text
AUCTION_DIAGNOSTIC_PAYMENT_RATE_DROP_POINTS
AUCTION_DIAGNOSTIC_DEAL_RATE_DROP_POINTS
AUCTION_DIAGNOSTIC_PAID_AMOUNT_DROP_RATIO
AUCTION_DIAGNOSTIC_PARTICIPANT_DROP_RATIO
AUCTION_DIAGNOSTIC_AVERAGE_DEAL_DROP_RATIO
AUCTION_DIAGNOSTIC_CURRENT_RATE_MIN_SAMPLE
AUCTION_DIAGNOSTIC_BASELINE_RATE_MIN_SAMPLE
AUCTION_DIAGNOSTIC_CURRENT_AVERAGE_MIN_SAMPLE
AUCTION_DIAGNOSTIC_BASELINE_AVERAGE_MIN_SAMPLE
AUCTION_DIAGNOSTIC_HIGH_SEVERITY_MULTIPLIER
AUCTION_DIAGNOSTIC_HIGH_RISK_COUNT
AUCTION_DIAGNOSTIC_MAX_ANOMALIES
```

Add parser tests proving invalid floats, NaN/Inf, zero samples, and an excessive maximum fail server configuration instead of silently falling back.

- [ ] **Step 4: Implement tenant-scoped baseline aggregation and dependency loading**

Add focused private helpers:

```go
func (s *Store) dashboardDiagnosticSnapshot(ctx context.Context, q auction.DashboardQuery) (auction.DashboardSnapshot, []auction.DashboardSection)
func (s *Store) dashboardBaseline(ctx context.Context, q auction.DashboardQuery, plan auction.DashboardBaselinePlan) (auction.DashboardSnapshot, string)
func dashboardHasSection(sections []auction.DashboardSection, target auction.DashboardSection) bool
```

When `diagnostics` is requested, always load current Summary, Funnel, OrderRisks, and LotPerformance for evaluation. Baseline aggregation loads only Summary and Funnel for each planned window; it sums ratio numerators/denominators and paid-order counts, and computes comparable means for total/count metrics. Every query receives the same authenticated `MainAccountID`.

Keep dependency-loading errors isolated: record the failed dependency, evaluate remaining rules, set `Diagnostics.Status=PARTIAL`, and append `DashboardPartialError{Section: DashboardSectionDiagnostics, Code: "DATA_INCOMPLETE"}` only when diagnostic completeness is affected.

- [ ] **Step 5: Add and pass degradation tests**

Add tests named:

```go
func TestDashboardDiagnosticsOnlySectionLoadsDependenciesWithoutExposingRequestedPanels(t *testing.T)
func TestDashboardDiagnosticsHistoryFailurePreservesFixedRulesAndCurrentSummary(t *testing.T)
func TestDashboardDiagnosticsCurrentDependencyFailureSkipsOnlyRelatedRules(t *testing.T)
```

Use a test-only GORM callback or dropped table to force the intended query failure. Assert fixed rules survive a baseline failure and an unavailable dependency does not produce zero-value “healthy” evidence.

Run:

```powershell
go test ./app/auction/service/internal/data -run 'TestDashboardDiagnostics' -count=1
go test ./app/auction/service/cmd/server -run 'TestDashboardDiagnosticConfig' -count=1
```

Expected: PASS.

- [ ] **Step 6: Run all affected backend packages**

Run:

```powershell
go test ./app/auction/service/internal/biz/auction ./app/auction/service/internal/data ./app/auction/service/cmd/server -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit repository diagnostics**

```powershell
git add -- live-auction-bid-backend/app/auction/service/internal/data/store.go live-auction-bid-backend/app/auction/service/internal/data/dashboard_repo.go live-auction-bid-backend/app/auction/service/internal/data/dashboard_repo_test.go live-auction-bid-backend/app/auction/service/cmd/server/main.go live-auction-bid-backend/app/auction/service/cmd/server/main_test.go
git commit -m "feat: calculate dashboard diagnostic baselines"
```

### Task 3: Protobuf and HTTP response contract

**Files:**
- Modify: `live-auction-bid-backend/api/auction/service/v1/auction.proto`
- Modify (generated): `live-auction-bid-backend/api/auction/service/v1/auction.pb.go`
- Modify (generated if generator changes it): `live-auction-bid-backend/api/auction/service/v1/auction_http.pb.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/service/auction_contract.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/service/dashboard_test.go`

**Interfaces:**
- Consumes: Task 1 domain diagnostics populated by Task 2.
- Produces: ProtoJSON fields `diagnostics.status`, `baselineStatus`, `currentWindow`, `baselineWindows`, `missingDependencies`, `anomalies`, and `truncated` on `GetAdminDashboardReply`.

- [ ] **Step 1: Extend the service fixture and add a failing mapping assertion**

Populate `dashboardServiceSnapshot().Diagnostics` with one `PAYMENT_RATE_DROP` and assert all scalar, ratio, window, status, action-code, and truncation fields in `TestGetAdminDashboardMapsClaimScopedSnapshot`.

Also add a test that sends `Sections: []string{"diagnostics"}` and verifies the normalized query retains that section and the authenticated `merchant-a` scope.

- [ ] **Step 2: Run the service test and verify it fails**

Run:

```powershell
go test ./app/auction/service/internal/service -run 'TestGetAdminDashboard' -count=1
```

Expected: FAIL because the generated reply has no diagnostics fields.

- [ ] **Step 3: Add the protobuf messages and response field**

Add messages with these field names and categories:

```proto
message DashboardDiagnosticWindow {
  int64 start_unix_ms = 1;
  int64 end_unix_ms = 2;
}
message DashboardDiagnosticMetric {
  optional int64 amount = 1;
  optional int64 count = 2;
  DashboardRatio ratio = 3;
}
message DashboardAnomaly {
  string code = 1;
  string severity = 2;
  string basis = 3;
  string metric = 4;
  DashboardDiagnosticMetric actual = 5;
  DashboardDiagnosticMetric baseline = 6;
  double deviation = 7;
  double threshold = 8;
  int64 sample_size = 9;
  int64 baseline_sample_size = 10;
  string evidence_code = 11;
  string recommended_action_code = 12;
  string lot_id = 13;
  string lot_title = 14;
}
message DashboardDiagnostics {
  string status = 1;
  string baseline_status = 2;
  DashboardDiagnosticWindow current_window = 3;
  repeated DashboardDiagnosticWindow baseline_windows = 4;
  repeated string missing_dependencies = 5;
  repeated DashboardAnomaly anomalies = 6;
  bool truncated = 7;
}
```

Append `DashboardDiagnostics diagnostics = 12;` to `GetAdminDashboardReply`; do not renumber existing fields.

- [ ] **Step 4: Regenerate Go bindings and inspect generated scope**

From `live-auction-bid-backend`, run the repository generator:

```powershell
make api
```

Expected: `auction.pb.go` and, if affected by the installed generator, `auction_http.pb.go` update. Run `git diff -- api/auction/service/v1` and confirm no unrelated agent-protocol generated files are staged or modified by this task.

- [ ] **Step 5: Map domain diagnostics to protobuf**

Add explicit conversion functions:

```go
func dashboardDiagnosticsToProto(value auctionbiz.DashboardDiagnostics) *v1.DashboardDiagnostics
func dashboardAnomalyToProto(value auctionbiz.DashboardAnomaly) *v1.DashboardAnomaly
func dashboardDiagnosticMetricToProto(value auctionbiz.DashboardDiagnosticMetric) *v1.DashboardDiagnosticMetric
func dashboardDiagnosticWindowToProto(value auctionbiz.DashboardDiagnosticWindow) *v1.DashboardDiagnosticWindow
```

Only set optional amount/count pointers when present; preserve zero as a valid explicit value. Map every list in source order.

- [ ] **Step 6: Run service and API compilation tests**

Run:

```powershell
go test ./app/auction/service/internal/service ./api/auction/service/v1 -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the API contract**

```powershell
git add -- live-auction-bid-backend/api/auction/service/v1/auction.proto live-auction-bid-backend/api/auction/service/v1/auction.pb.go live-auction-bid-backend/api/auction/service/v1/auction_http.pb.go live-auction-bid-backend/app/auction/service/internal/service/auction_contract.go live-auction-bid-backend/app/auction/service/internal/service/dashboard_test.go
git commit -m "feat: expose dashboard diagnostics API"
```

If `auction_http.pb.go` is byte-identical after generation, omit it from `git add`.

### Task 4: Harness Dashboard parser and Tool schema

**Files:**
- Modify: `integrations/deepseek-harness/src/auction-client.mjs`
- Modify: `integrations/deepseek-harness/src/query-tools.mjs`
- Modify: `integrations/deepseek-harness/src/schemas.mjs`
- Modify: `integrations/deepseek-harness/test/fixtures/dashboard.mjs`
- Modify: `integrations/deepseek-harness/test/auction-client.test.mjs`
- Modify: `integrations/deepseek-harness/test/query-tools.test.mjs`
- Modify: `integrations/deepseek-harness/test/dashboard-bounds.test.mjs`

**Interfaces:**
- Consumes: Task 3 ProtoJSON response.
- Produces: validated JavaScript `snapshot.diagnostics` and JSON Schema output accepted by Harness ToolRuntime.

- [ ] **Step 1: Extend the fixture and add failing happy-path parser assertions**

Make `dashboardReply()` include `diagnostics` with `COMPLETE`, `AVAILABLE`, one baseline window, and one `PAYMENT_RATE_DROP`. In `auction-client.test.mjs`, assert the exact nested object survives `AuctionApi.getDashboard()` without string-to-number coercion or percentage recomputation.

- [ ] **Step 2: Add failing hostile-response tests**

Add table cases for unknown status/severity/basis, non-finite deviation, a ratio outside `[0,1]`, negative sample sizes, reversed windows, more than 50 anomalies without `truncated=true`, and `PARTIAL` with neither a missing dependency nor unavailable baseline. Each case must reject with `InvalidBackendResponseError`.

- [ ] **Step 3: Run the focused JavaScript tests and verify they fail**

Run from `integrations/deepseek-harness`:

```powershell
npm test -- --test-name-pattern="dashboard diagnostics|diagnostic response"
```

Expected: FAIL because diagnostics are not parsed or declared.

- [ ] **Step 4: Implement strict parsers and schema definitions**

Add parser helpers for enumerations, optional metric variants, windows, and bounded arrays. Require exactly one of `amount`, `count`, or `ratio` for a populated metric. Enforce:

```text
status: COMPLETE | PARTIAL | UNAVAILABLE
baselineStatus: AVAILABLE | INSUFFICIENT_DATA | NOT_SUPPORTED | QUERY_FAILED
severity: LOW | MEDIUM | HIGH
basis: FIXED_RULE | HISTORICAL
anomalies: at most 50
baselineWindows: at most 7
```

Add `diagnostics` to the accepted section enum, raising the maximum section count from 7 to 8. Extend `dashboardOutput` with required diagnostic fields while retaining backward compatibility for backend replies that omit `diagnostics` unless the caller requested that section.

- [ ] **Step 5: Preserve diagnostics in Tool output and add display-only evidence formatting**

`getDashboard()` must return parsed diagnostics unchanged. Add money display strings only for diagnostic `amount` metrics; do not format counts or ratios into values used for decisions.

- [ ] **Step 6: Run all Harness plugin tests**

Run:

```powershell
npm test
```

Expected: PASS.

- [ ] **Step 7: Commit the Harness contract**

```powershell
git add -- integrations/deepseek-harness/src/auction-client.mjs integrations/deepseek-harness/src/query-tools.mjs integrations/deepseek-harness/src/schemas.mjs integrations/deepseek-harness/test/fixtures/dashboard.mjs integrations/deepseek-harness/test/auction-client.test.mjs integrations/deepseek-harness/test/query-tools.test.mjs integrations/deepseek-harness/test/dashboard-bounds.test.mjs
git commit -m "feat: validate auction dashboard diagnostics"
```

### Task 5: Diagnostic runtime Skill and tool routing contract

**Files:**
- Create: `integrations/deepseek-harness/src/diagnostic-skill.mjs`
- Create: `integrations/deepseek-harness/test/diagnostic-skill.test.mjs`
- Modify: `integrations/deepseek-harness/src/plugin.mjs`
- Modify: `integrations/deepseek-harness/test/query-tools.test.mjs`
- Modify: `integrations/deepseek-harness/scripts/runtime-smoke.mjs`
- Create: `integrations/deepseek-harness/test/diagnostic-behavior-cases.md`

**Interfaces:**
- Consumes: Task 4 `auction_get_dashboard` diagnostics output and existing `ctx.skills` registry.
- Produces: runtime Skill `diagnosing-auction-performance` and updated Tool description that routes anomaly questions to `sections=["diagnostics"]` plus user-requested evidence panels.

- [ ] **Step 1: Write failing Skill content tests**

Assert the exported object has the exact name and narrow description, and its content requires all of these phrases or equivalent stable tokens:

```text
status
baselineStatus
系统识别的异常
分析推断
建议动作
INSUFFICIENT_DATA
不得调用写工具
```

Also assert it tells the model that an empty anomaly list is limited to executed rules and available data.

- [ ] **Step 2: Run the Skill tests and verify they fail**

Run:

```powershell
npm test -- --test-name-pattern="diagnosing-auction-performance"
```

Expected: FAIL because the Skill does not exist.

- [ ] **Step 3: Implement and register the diagnostic Skill**

Export a frozen runtime Skill:

```js
export const diagnosticSkill = Object.freeze({
  name: 'diagnosing-auction-performance',
  description: 'Use when the user asks about a single merchant account\'s经营异常、指标下降、风险原因、经营健康度或改进建议；普通数值查询和多账号组合分析不要使用。',
  source: 'runtime',
  content: `# 单商家经营异常诊断

先调用 auction_get_dashboard 并请求 diagnostics。先检查 status、baselineStatus、missingDependencies 和 partialErrors。

只有 anomalies 中的项目可以称为系统识别的异常。可能原因必须标记为分析推断，建议动作必须对应具体 evidenceCode。baselineStatus 为 INSUFFICIENT_DATA、NOT_SUPPORTED 或 QUERY_FAILED 时，不得声称指标相对历史下降。空 anomalies 只表示已执行规则和有效数据范围内未发现异常。

按“诊断结论、数据证据、可能原因、建议动作”组织回答。不得调用写工具，也不得自行修改阈值或严重等级。`,
});
```

Register it beside `portfolioSkill` using `ctx.effect()`. Update the Dashboard Tool description so anomaly requests select `diagnostics`; general “今天咋样” with omitted sections still loads all sections.

- [ ] **Step 4: Extend runtime smoke assertions**

Make the smoke server return one diagnostic anomaly. Assert both runtime skills are discoverable and loadable, `auction_get_dashboard({sections:['diagnostics']})` returns the anomaly, malformed diagnostic input is rejected, and unloading removes both skills.

- [ ] **Step 5: Document model-level behavior cases**

In `diagnostic-behavior-cases.md`, record these acceptance prompts and expected routing:

```text
今天有什么异常？
为什么支付率下降？
哪些拍品需要关注？
今天成交额是多少？
比较店铺 A 和店铺 B 的异常。
```

The first three use the diagnostic Skill and single-account Tool; the fourth stays a plain Dashboard query; the fifth remains owned by `analyzing-auction-portfolio`.

- [ ] **Step 6: Run Harness unit tests and runtime smoke**

Run:

```powershell
Set-Location integrations/deepseek-harness
npm test
Set-Location ../../deepseek-harness
pnpm exec tsx --tsconfig tsconfig.json ../ByteDanceliveauctioni/integrations/deepseek-harness/scripts/runtime-smoke.mjs
```

Expected: all unit tests PASS; runtime smoke prints its PASS line and reports seven tools plus two runtime skills.

- [ ] **Step 7: Commit the Skill and routing behavior**

```powershell
git add -- integrations/deepseek-harness/src/diagnostic-skill.mjs integrations/deepseek-harness/test/diagnostic-skill.test.mjs integrations/deepseek-harness/src/plugin.mjs integrations/deepseek-harness/test/query-tools.test.mjs integrations/deepseek-harness/scripts/runtime-smoke.mjs integrations/deepseek-harness/test/diagnostic-behavior-cases.md
git commit -m "feat: guide single-account anomaly diagnosis"
```

### Task 6: Development-only diagnostic demo seed

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/data/dashboard_diagnostic_seed.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/dashboard_diagnostic_seed_test.go`
- Create: `live-auction-bid-backend/app/auction/service/cmd/diagnostic-seed/main.go`
- Create: `live-auction-bid-backend/app/auction/service/cmd/diagnostic-seed/main_test.go`
- Modify: `live-auction-bid-backend/deploy/.env.example`

**Interfaces:**
- Consumes: existing GORM models and `AUCTION_MYSQL_DSN`.
- Produces: `SeedDashboardDiagnosticDemo(context.Context, *gorm.DB, DiagnosticDemoSeedOptions) error`, `CleanDashboardDiagnosticDemo(context.Context, *gorm.DB, string) error`, and CLI flags `--username`, `--now`, and `--clean`.

- [ ] **Step 1: Add failing environment-guard and ownership tests**

Test that `dev` and `test` are accepted, while empty, `prod`, `production`, and `verify` are rejected before opening the database. Test that an empty username and invalid `--now` fail with clear errors.

- [ ] **Step 2: Add failing SQLite seed tests**

Create an existing main-account user, an unrelated merchant with rows, and one unrelated row under the target merchant. Seed at fixed Shanghai noon and assert:

- seven prior comparable windows produce about 80% historical payment rate;
- the current window produces 40% payment rate;
- payment-failed, expired, no-bid, and zero-conversion fixtures exist;
- all owned IDs begin `diagnostic-demo-`;
- running seed twice yields the same owned row counts and metrics;
- clean removes only owned rows and preserves unrelated rows and the user.

- [ ] **Step 3: Run the seed tests and verify they fail**

Run:

```powershell
go test ./app/auction/service/internal/data ./app/auction/service/cmd/diagnostic-seed -run 'TestDiagnosticDemo' -count=1
```

Expected: FAIL because the seeder and command do not exist.

- [ ] **Step 4: Implement transactional owned-prefix seed and cleanup**

Use one transaction. Resolve the target `AuctionUserModel` by username and use its authenticated `MainAccountID`; do not accept a main-account ID flag. Delete child order items before orders, then bids, lots, and rooms, all restricted to exact owned IDs or the `diagnostic-demo-` prefix plus target tenant. Insert deterministic rows relative to the supplied `--now` timestamp.

Expose these exact options:

```go
type DiagnosticDemoSeedOptions struct {
	Environment string
	Username string
	NowUnixMs int64
}
```

The CLI reads `AUCTION_ENV` and `AUCTION_MYSQL_DSN`, defaults `--username` to `main`, defaults `--now` to current time, and supports `--clean`. It must print only the target username, logical scenario, and row counts; never print credentials or DSNs.

- [ ] **Step 5: Run seed tests and manually verify help output**

Run:

```powershell
go test ./app/auction/service/internal/data ./app/auction/service/cmd/diagnostic-seed -run 'TestDiagnosticDemo' -count=1
go run ./app/auction/service/cmd/diagnostic-seed --help
```

Expected: tests PASS; help lists `--username`, `--now`, and `--clean` without touching the database.

- [ ] **Step 6: Document the opt-in environment variables**

Add to `deploy/.env.example`:

```text
# Dev/test-only manual command inputs; the server does not run this seed automatically.
AUCTION_DIAGNOSTIC_DEMO_USERNAME=main
```

Document command examples in the final integration README task, not in the environment file.

- [ ] **Step 7: Commit the demo seed**

```powershell
git add -- live-auction-bid-backend/app/auction/service/internal/data/dashboard_diagnostic_seed.go live-auction-bid-backend/app/auction/service/internal/data/dashboard_diagnostic_seed_test.go live-auction-bid-backend/app/auction/service/cmd/diagnostic-seed/main.go live-auction-bid-backend/app/auction/service/cmd/diagnostic-seed/main_test.go live-auction-bid-backend/deploy/.env.example
git commit -m "test: add dashboard diagnostic demo seed"
```

### Task 7: Documentation, regression verification, and final diff audit

**Files:**
- Modify: `integrations/deepseek-harness/README.md`
- Modify: `live-auction-bid-backend/README.md`
- Modify: `docs/superpowers/specs/2026-09-21-harness-single-account-diagnostics-design.md` only if implementation reveals a factual mismatch; keep status current.

**Interfaces:**
- Consumes: all previous tasks.
- Produces: operator instructions for querying diagnostics and creating/cleaning deterministic dev data.

- [ ] **Step 1: Update the integration README**

Document:

- the eighth Dashboard section `diagnostics` without changing the seven Tool count;
- diagnostic status and baseline status meanings;
- the fixed-rule versus historical-rule boundary;
- example prompts and the fact that suggestions never execute writes;
- the exact dev seed commands:

```powershell
Set-Location live-auction-bid-backend
$env:AUCTION_ENV = 'dev'
go run ./app/auction/service/cmd/diagnostic-seed --username main
go run ./app/auction/service/cmd/diagnostic-seed --username main --clean
```

- [ ] **Step 2: Update the backend README**

Document all diagnostic threshold environment keys, defaults, accepted ranges, and failure-on-invalid-config behavior. State that the seed is never run by server startup and refuses non-dev/test environments.

- [ ] **Step 3: Run focused backend verification**

Run from `live-auction-bid-backend`:

```powershell
go test ./app/auction/service/internal/biz/auction ./app/auction/service/internal/data ./app/auction/service/internal/service ./app/auction/service/cmd/server ./app/auction/service/cmd/diagnostic-seed -count=1
```

Expected: PASS.

- [ ] **Step 4: Run full backend verification**

Run:

```powershell
go test ./...
```

Expected: PASS. If an unrelated pre-existing dirty area fails, record the exact package and rerun the focused packages above; do not modify unrelated code merely to obtain a green full-suite result.

- [ ] **Step 5: Run Harness verification**

Run:

```powershell
Set-Location ..\integrations\deepseek-harness
npm test
Set-Location ..\..\deepseek-harness
pnpm exec tsx --tsconfig tsconfig.json ../ByteDanceliveauctioni/integrations/deepseek-harness/scripts/runtime-smoke.mjs
```

Expected: unit tests and runtime smoke PASS.

- [ ] **Step 6: Audit generated files, secrets, and unintended changes**

Run from the `ByteDanceliveauctioni` root:

```powershell
git diff --check
git status --short
git diff -- live-auction-bid-backend/app/auction/service/internal/biz/auction live-auction-bid-backend/app/auction/service/internal/data/dashboard_repo.go live-auction-bid-backend/app/auction/service/internal/service/auction_contract.go live-auction-bid-backend/api/auction/service/v1/auction.proto integrations/deepseek-harness
rg -n "password|accessToken|Authorization|AUCTION_MYSQL_DSN" live-auction-bid-backend/app/auction/service/internal/data/dashboard_diagnostic_seed.go live-auction-bid-backend/app/auction/service/cmd/diagnostic-seed integrations/deepseek-harness/src/diagnostic-skill.mjs
```

Expected: no whitespace errors; only intended target changes; no credentials, tokens, DSNs, or authorization headers emitted by the seed or Skill. Existing unrelated dirty files remain untouched.

- [ ] **Step 7: Commit documentation and final verification metadata**

```powershell
git add -- integrations/deepseek-harness/README.md live-auction-bid-backend/README.md docs/superpowers/specs/2026-09-21-harness-single-account-diagnostics-design.md
git commit -m "docs: explain auction anomaly diagnostics"
```

If the design spec did not require a factual correction, omit it from `git add`.

## Final Acceptance

- [ ] `auction_get_dashboard` can request `diagnostics` for one authenticated merchant.
- [ ] Fixed and historical diagnoses include auditable evidence, thresholds, samples, and severity.
- [ ] `live` never claims a historical baseline in V1.
- [ ] Historical or dependency failures preserve valid current data and expose partial status.
- [ ] The Harness Skill distinguishes system anomalies, inference, and suggestions and never calls writes.
- [ ] The dev/test seed reproduces healthy baseline plus current anomalies without production data.
- [ ] All focused Go tests, Harness unit tests, runtime smoke, and diff checks pass.
