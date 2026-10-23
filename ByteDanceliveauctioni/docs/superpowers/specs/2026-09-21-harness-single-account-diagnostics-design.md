# DeepSeek Harness 单账号经营异常诊断设计

日期：2026-09-21
状态：设计已确认，等待实施计划

## 1. 背景

现有 DeepSeek Harness 竞拍后台插件通过 `auction_get_dashboard` 查询当前商家在 `today`、`live`、`7d` 或 `30d` 范围内的经营数据。Go 后端已经提供成交额、订单量、支付率、成交率、漏斗、趋势、订单风险和拍品表现，但单账号结果没有统一、可追溯的异常诊断。模型可以阅读这些数字，却不应自行发明阈值或把主观判断描述成系统事实。

本设计在现有 Dashboard 接口中增加 `diagnostics` 板块。Go 后端负责基于固定业务规则和商家历史基线执行确定性诊断；Harness 插件负责校验结构化结果；运行时 Skill 负责约束模型如何解释证据、推断原因和提出建议。

## 2. 目标与非目标

### 2.1 目标

- 对当前登录的单个商家账号执行确定性经营异常诊断。
- 同时支持固定业务规则和历史同期偏离规则。
- 每条异常携带代码、严重等级、当前值、基线、阈值、样本量、时间窗口和证据。
- 历史数据不足或查询失败时保留仍可确定的固定规则结果，并明确诊断完整度。
- 由 Go 后端统一持有诊断口径和阈值，Harness 与提示词不复制判定逻辑。
- 让模型严格区分系统识别的异常、基于证据的分析推断和建议动作。
- 在空业务数据库上仍可通过纯函数测试、临时数据库场景和本地演示数据完成验证。
- 保持现有租户隔离、认证方式、Dashboard 查询范围和其他 Tool 行为兼容。

### 2.2 非目标

- 不做持续监控、定时扫描或主动通知。
- 不自动调价、调整拍品顺序、催付或执行其他写操作。
- 不在第一版建立机器学习异常检测模型。
- 不把排名靠后、数据为空或历史样本不足本身描述为经营异常。
- 不为 `live` 范围虚构跨场历史基线；当前数据模型没有可靠的历史直播场次边界。
- 不要求生产数据库预先具有大量数据才能验证实现。

## 3. 方案选择

### 3.1 由模型直接判断异常

模型阅读 Dashboard 后自行选择阈值和结论。该方案改动最少，但相同数据可能得到不同判断，阈值不可审计，且模型容易混淆事实与推断，因此不采用。

### 3.2 由 Harness 插件查询并计算异常

插件可以复用当前 Dashboard 响应执行固定规则，但现有 API 不支持任意历史窗口，无法可靠获取历史同期数据。统计口径还会同时存在于 Go 与 JavaScript，长期容易漂移，因此不采用。

### 3.3 后端诊断，Harness 解释

Go 后端在现有 Dashboard 中新增 `diagnostics` 板块，读取当前窗口和历史基线并运行纯函数诊断器。Harness 严格校验结果，Skill 规定模型的解释方式。该方案让统计口径、租户隔离和阈值留在权威数据侧，并保留 Harness 的自然语言交互价值，最终采用此方案。

## 4. 总体架构

```text
用户询问经营异常或综合经营情况
                │
                ▼
DeepSeek Harness
  ├── diagnosing-auction-performance Skill
  └── auction_get_dashboard Tool
                │ Bearer Token
                ▼
GET /api/admin/dashboard?range=...&sections=diagnostics
                │
                ▼
Go Dashboard
  ├── 当前窗口权威指标
  ├── 历史基线窗口指标
  ├── 固定规则诊断
  ├── 历史偏离诊断
  └── 结构化证据与完整度
```

### 4.1 Go 后端职责

- 从认证 claims 派生 `mainAccountId`，所有当前及历史查询都按该租户过滤。
- 规范化当前范围和历史基线窗口。
- 使用纯函数诊断器计算异常，不生成开放式自然语言原因。
- 返回机器可验证的证据、阈值、样本量和建议动作代码。
- 将历史数据不足、历史查询失败和依赖板块失败表达为诊断状态，而不是零值经营结论。

### 4.2 Harness 插件职责

- 支持 `diagnostics` Dashboard section，并严格校验后端响应。
- 保留整数金额、比例分子分母、时间窗口和异常代码，不重新计算阈值。
- 把诊断结果暴露给模型，但不执行建议动作。
- 对未知枚举、非法数值、越界数组和不一致时间窗口拒绝响应。

### 4.3 Skill 职责

- 在用户询问异常、下降原因、风险、经营健康度或改进建议时加载。
- 先读取诊断状态、数据完整度和证据，再解释异常。
- 只把后端返回的异常称为“系统识别的异常”。
- 将可能原因标记为分析推断，不把相关性表述为因果关系。
- 建议动作必须对应具体证据，不得自动调用写工具。

## 5. Dashboard 接口扩展

Dashboard section 新增：

```text
diagnostics
```

Dashboard 响应新增：

```json
{
  "diagnostics": {
    "status": "COMPLETE",
    "baselineStatus": "AVAILABLE",
    "currentWindow": {
      "startUnixMs": 0,
      "endUnixMs": 0
    },
    "baselineWindows": [],
    "anomalies": []
  }
}
```

`status` 取值：

- `COMPLETE`：所有请求的诊断依赖和历史基线均可用。
- `PARTIAL`：固定规则可用，但历史基线或部分依赖不可用。
- `UNAVAILABLE`：没有足够的当前事实执行任何诊断。

`baselineStatus` 取值：

- `AVAILABLE`：历史基线达到最低样本要求。
- `INSUFFICIENT_DATA`：历史查询成功，但样本不足。
- `NOT_SUPPORTED`：当前范围不支持历史基线，第一版用于 `live`。
- `QUERY_FAILED`：历史查询失败。

单条异常建议结构：

```json
{
  "code": "PAYMENT_RATE_DROP",
  "severity": "HIGH",
  "basis": "HISTORICAL",
  "metric": "paymentRate",
  "actual": { "numerator": 9, "denominator": 20, "rate": 0.45 },
  "baseline": { "numerator": 117, "denominator": 150, "rate": 0.78 },
  "deviation": -0.33,
  "threshold": -0.20,
  "sampleSize": 20,
  "baselineSampleSize": 150,
  "evidenceCode": "RATE_BELOW_BASELINE",
  "recommendedActionCode": "REVIEW_PAYMENT_FAILURES"
}
```

协议返回结构化证据代码和数值，不依赖后端自然语言文案。Harness Skill 根据当前语言生成面向用户的解释，避免 API 文案与客户端国际化耦合。

异常数组设置固定上限并按 `severity DESC`、`code ASC`、范围标识 ASC 稳定排序。超过上限时返回截断标记，不能静默截断。

## 6. 历史基线

- `today`：过去 7 个完整自然日各自从 00:00 到当前查询时刻对应的上海本地时刻。总量指标以 7 个窗口的均值为基线；比例指标合并全部窗口分子和分母后重算。
- `7d`：紧邻当前窗口之前的完整 7 天窗口。
- `30d`：紧邻当前窗口之前的完整 30 天窗口。
- `live`：第一版不计算跨场历史基线，只执行固定规则和本场可确定规则，`baselineStatus` 返回 `NOT_SUPPORTED`。

基线窗口为左闭右开区间，使用 `Asia/Shanghai`。历史查询与当前查询共用同一快照时刻，避免执行过程中时间漂移。

比例不得平均已经计算的百分比。支付率、成交率和拍品转化率必须合并分子与分母后重算。金额继续使用人民币分的安全整数。

## 7. 诊断规则

### 7.1 固定业务规则

- `PAYMENT_FAILED`：支付失败订单数大于 0。
- `EXPIRED_ORDERS`：已超时订单数大于 0。
- `NO_BID_LOTS`：已开拍拍品数大于有出价拍品数。
- `UNPAID_SETTLED_LOTS`：已成交拍品数大于已支付拍品数。
- `LOT_NO_CONVERSION`：拍品有出价且支付转化分子为 0；仅在拍品表现数据完整时判断。
- `DATA_INCOMPLETE`：Dashboard 依赖板块存在部分错误。它是数据质量信号，不是经营异常。

### 7.2 历史偏离规则

| 异常代码 | 最低样本 | 初始触发条件 |
| --- | --- | --- |
| `PAYMENT_RATE_DROP` | 当前分母 ≥ 10、基线分母 ≥ 30 | 比基线低至少 20 个百分点 |
| `DEAL_RATE_DROP` | 当前开拍 ≥ 10、基线开拍 ≥ 30 | 比基线低至少 20 个百分点 |
| `PAID_AMOUNT_DROP` | 当前与基线均有有效支付订单 | 比基线可比值低至少 30% |
| `PARTICIPANT_COUNT_DROP` | 基线平均参拍人数 ≥ 10 | 比基线可比值低至少 30% |
| `AVERAGE_DEAL_AMOUNT_DROP` | 当前已支付订单 ≥ 5、基线 ≥ 15 | 比基线低至少 25% |

上述数值是保守默认值。阈值和最低样本由后端诊断配置集中提供，不在 Harness 插件或 Skill 中复制。部署可以调整配置，但一次响应必须携带实际采用的阈值，以便审计。

### 7.3 严重等级

严重等级由确定性规则产生：

- `LOW`：值得关注但没有即时损失证据。
- `MEDIUM`：超过默认异常阈值，或存在少量失败、超时订单。
- `HIGH`：偏离达到高等级阈值，或失败、超时数量达到配置的高风险门槛。

每条规则必须同时定义普通阈值和高等级阈值。模型不能提高或降低严重等级。

## 8. 数据加载与失败降级

请求 `diagnostics` 时，后端内部加载诊断所需的当前 Summary、Funnel、OrderRisks 和 LotPerformance。依赖数据即使没有作为显式展示板块请求，也可以参与诊断；响应不会因此假装用户请求了所有展示板块。

- 历史样本不足：返回固定规则结果，`status=PARTIAL`、`baselineStatus=INSUFFICIENT_DATA`。
- 历史查询失败：返回固定规则结果，`status=PARTIAL`、`baselineStatus=QUERY_FAILED`。
- 单个依赖板块失败：跳过依赖该板块的规则，并在诊断缺失依赖列表和 `partialErrors` 中记录稳定错误码。
- 所有当前诊断事实不可用：`status=UNAVAILABLE`，不返回看似正常的空异常列表。
- 空 `anomalies` 只表示在已执行规则及有效数据范围内未发现异常，不表示经营绝对正常。

历史诊断失败不得使已成功的 Dashboard 经营数据整体失败。

## 9. 空数据库下的测试与演示数据

生产数据库是否有数据不影响自动测试。测试分为四层。

### 9.1 纯函数诊断测试

直接构造当前指标、历史基线和配置，不访问数据库。覆盖每条规则的触发、不触发、临界值、样本不足、严重等级、排序、截断和整数安全性。

### 9.2 临时数据库测试

数据层测试使用现有临时 SQLite 测试设施，在每个测试内插入商家、拍品、出价和订单。测试固定 `NowUnixMs`，构造准确的当前与历史窗口，不依赖机器当前时间或开发数据库。

至少包含三组场景：

- 健康场景：当前指标与历史基线接近，不触发异常。
- 异常场景：历史支付率约 80%，当前支付率 40%，同时包含支付失败、超时、无出价和零转化拍品。
- 样本不足场景：固定规则仍可触发，但历史异常不触发，基线状态明确为 `INSUFFICIENT_DATA`。

### 9.3 API 与 Harness fixture

Go API 测试验证 Proto/HTTP 映射、认证租户范围和部分失败。Harness 单元测试及 runtime smoke 使用本地 HTTP fixture 返回固定诊断结果，不调用真实模型、不要求真实商家账号，也不依赖 Go 服务数据库。

### 9.4 开发环境演示数据

提供仅限开发/测试环境的显式 seed 命令，创建独立演示商家及可重复的数据集。数据包含过去 7 天稳定基线和当天明显下降指标，使“今天有什么异常”“为什么支付率下降”等话术可以稳定展示。

seed 必须满足：

- 默认不在生产启动流程执行。
- 拒绝对非开发/测试环境运行。
- 使用固定前缀或固定演示租户，避免修改真实商家数据。
- 可重复执行并得到相同逻辑结果，不无限追加重复数据。
- 提供对应清理方式，只删除该 seed 明确拥有的数据。

## 10. 测试策略

### 10.1 Go 领域测试

- 当前和基线窗口规范化，包括上海时区边界。
- 所有固定规则和历史偏离规则。
- 比率合并分子分母，不平均百分比。
- 最低样本、普通阈值和高等级阈值边界。
- 部分依赖缺失时只跳过相关规则。
- 稳定排序、去重、上限和截断标记。

### 10.2 Go 数据与接口测试

- 所有当前及历史查询始终包含 `mainAccountId` 范围。
- `today`、`7d`、`30d` 基线窗口正确，`live` 明确不支持历史基线。
- Dashboard `diagnostics` section 和响应字段正确映射。
- 历史查询失败不会抹掉当前 Dashboard 数据。
- 未授权、权限不足和其他账号数据不会进入诊断。

### 10.3 Harness 插件测试

- `diagnostics` 参数可被接受，未知 section 仍被拒绝。
- 所有状态、枚举、数值、比例、窗口和数组上限严格校验。
- 非法或互相矛盾的诊断响应被拒绝。
- 诊断结果中的金额继续以分存储，显示格式不参与计算。
- 现有登录、成员创建、普通 Dashboard、拍品和多账号工具保持兼容。

### 10.4 Skill 与运行时验证

- “今天有什么异常”选择包含 `diagnostics` 的 Dashboard 查询。
- “为什么支付率下降”引用后端返回的当前值、基线和阈值。
- `INSUFFICIENT_DATA` 时不声称存在历史下降。
- 回答区分诊断事实、可能原因和建议动作。
- Skill 不调用任何写工具。
- runtime smoke 验证 Tool 与 Skill 在真实 Cordis/Harness 运行时中完成注册、调用和卸载。

## 11. 代码组织

预计涉及以下位置：

```text
live-auction-bid-backend/
├── api/auction/service/v1/auction.proto
└── app/auction/service/internal/
    ├── biz/auction/dashboard.go
    ├── biz/auction/dashboard_diagnostics.go
    ├── data/dashboard_repo.go
    └── service/auction_contract.go

integrations/deepseek-harness/
├── src/
│   ├── auction-client.mjs
│   ├── query-tools.mjs
│   ├── schemas.mjs
│   ├── diagnostic-skill.mjs
│   └── plugin.mjs
├── test/
└── scripts/runtime-smoke.mjs
```

生成的 Proto 文件随协议修改更新。当前工作区已有其他未提交改动，实施时只增量修改目标代码，不覆盖或回退用户现有修改；提交前逐文件核对差异。

## 12. 验收标准

1. 单账号用户可以询问当前经营异常，并获得结构化、可追溯的诊断结果。
2. 固定业务规则和受支持范围的历史偏离规则同时生效。
3. 每条异常包含实际值、基线、阈值、样本和严重等级。
4. 历史样本不足或查询失败时仍保留可确定的固定规则结果，且不会生成伪历史结论。
5. 所有当前及历史数据保持认证租户隔离。
6. 模型明确区分系统诊断、分析推断和建议动作。
7. 诊断不触发任何写操作。
8. 空开发数据库下，自动测试和开发 seed 均能稳定证明健康、异常和样本不足场景。
9. 现有 Dashboard 字段、其他 Harness Tool 和多账号分析保持兼容。
10. 相关 Go 测试、Harness 单元测试、runtime smoke 和差异检查全部通过。
