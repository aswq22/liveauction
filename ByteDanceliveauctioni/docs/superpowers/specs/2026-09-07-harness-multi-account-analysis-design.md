# DeepSeek Harness 多商家账号经营分析 Skill 设计

日期：2026-09-07
状态：设计已确认，等待实施计划

## 1. 背景

现有 DeepSeek Harness 竞拍后台插件支持单个商家账号登录，并能查询该账号的经营看板、拍品价格和实时拍品。每个 Harness 会话只维护一个 `AuctionAuth`，登录其他账号会清除之前的登录态。

新需求是由用户一次提供多组彼此独立的商家主账号凭据，汇总并比较这些账号的竞拍经营数据，例如总成交额、成交商品、账号排名、风险订单和异常拍品。账号之间不存在集团关系或预先建立的跨租户授权；成功登录每个账号是读取对应租户数据的唯一授权依据。

该需求既包含确定性的多账号认证与统计，也包含可复用的经营诊断流程。设计采用“组合 Tool 负责安全执行和精确计算，Skill 负责分析 SOP，模型负责理解与表达”的边界。

## 2. 目标与非目标

### 2.1 目标

- 接受 2 至 10 组独立商家主账号凭据，完成一次性跨账号经营分析。
- 每个账号使用隔离的认证实例和 Token，不改变当前单账号登录态。
- 复用现有 Go 单租户登录及 Dashboard 接口，不在 Go 服务内引入集团租户模型。
- 程序化计算总金额、订单数、加权平均值、加权比率、账号排名和风险标记。
- 单个账号失败时返回其余账号的有效结果和明确覆盖率。
- 注册窄范围的 `analyzing-auction-portfolio` Skill，指导跨账号比较、异常下钻和报告表达。
- 插件不额外持久化、主动记录或回显密码与临时 Token，并在任务结束后释放其引用；聊天输入和 Harness 事件日志的既有凭证暴露边界必须明确披露。
- 保持现有登录、成员创建和单账号查询 Tool 的行为兼容。

### 2.2 非目标

- 不创建集团、代运营方或永久账号绑定关系。
- 不持久化账号密码或跨会话复用临时 Token。
- 不允许用户绕过登录直接提交 `mainAccountId` 查询数据。
- 不让模型自行求和、平均百分比或判断权限。
- V1 不跨账号执行创建、修改、开拍、落锤等写操作。
- V1 不声称对跨账号参与者做全局去重；现有 Dashboard 只提供租户内去重后的数量，没有可安全合并的用户标识。
- 不保证擦除已经进入用户会话或模型上下文的密码文本；本设计保证的是插件不额外持久化、记录或回显凭据。JavaScript 字符串也不提供可验证的内存清零语义。

## 3. 方案选择

### 3.1 Skill 循环调用现有单账号 Tool

模型依次调用 `auction_login` 和 `auction_get_dashboard`，再自行汇总。改动少，但登录态反复覆盖，关键计算依赖模型，中间结果占用上下文，部分失败和重复账号也难以可靠处理，因此不采用。

### 3.2 Harness 组合 Tool + 运行时 Skill

插件新增一个只读组合 Tool。Tool 内部为每组凭据创建独立的临时 `AuctionAuth`，并发登录和查询 Dashboard，随后由确定性聚合器生成结构化报告。运行时 Skill 规定何时调用、何时下钻和如何呈现不完整数据。

该方案复用现有 Go 权限边界，不改变核心交易服务的租户模型，且把精确计算与语言推理分离。最终选择此方案。

### 3.3 Go 后端接收多组凭据并聚合

由 Go 新增批量登录接口。虽然聚合集中，但会让核心交易服务接收其他租户的批量明文凭据，扩大接口风险并引入与交易链路无关的编排职责，因此不采用。

## 4. 总体架构

```text
用户提供多组账号密码与分析范围
                │
                ▼
DeepSeek Harness
  └── analyzing-auction-portfolio Skill
                │
                ▼
auction_get_multi_account_report Tool
  ├── 参数校验与账号数量限制
  ├── 每账号独立 AuctionAuth / AuctionApi
  ├── 有界并发登录和 Dashboard 查询
  ├── 按认证后的 mainAccountId 去重
  ├── 确定性聚合、排名与风险标记
  └── finally 清理所有临时认证状态
                │ HTTP + 独立 Bearer Token
                ▼
Go 竞拍核心服务（现有单租户接口）
```

### 4.1 Go 后端职责

- 按每次登录生成的 Token 识别唯一商家租户。
- 对每个 Dashboard 请求重新执行认证、权限校验和租户隔离。
- 返回当前租户范围内的权威指标、统计分子/分母、快照时间和部分错误。
- 不感知本次请求属于跨账号组合分析。

### 4.2 Harness 组合 Tool 职责

- 校验多组账号输入，不记录和回显密码。
- 对各账号建立互相隔离、仅本次调用可见的认证对象。
- 使用固定并发上限访问 Go 服务。
- 验证登录身份，按 `mainAccountId` 去重，防止同一商家用不同登录名重复计入。
- 聚合数值并保留每个账号的原始权威分子、分母和快照范围。
- 返回账号级失败，不把一个账号的失败升级为整批失败。
- 在完成、失败或取消时清理全部临时认证对象。

### 4.3 Skill 职责

- 只对多账号汇总、比较或诊断请求加载。
- 确认用户已经给出至少两组完整凭据以及时间范围；缺失时追问，不猜测。
- 根据用户目标选择 Dashboard 板块，避免默认加载无关明细。
- 检查覆盖率、部分错误和统计口径后再解释结果。
- 先输出整体与账号级事实，再输出异常解释和建议。
- 需要精确商品或实时状态时，说明需要切换到对应账号并使用现有单账号 Tool 下钻；V1 不把密码或 Token 从组合 Tool 输出给后续 Tool。

## 5. Tool 契约

新增 Tool：

```text
auction_get_multi_account_report
```

建议输入：

```json
{
  "accounts": [
    { "username": "merchant_a", "password": "********", "alias": "店铺A" },
    { "username": "merchant_b", "password": "********", "alias": "店铺B" }
  ],
  "range": "30d",
  "sections": ["summary", "funnel", "trend", "topLots", "categoryPerformance", "orderRisks", "lotPerformance"]
}
```

输入规则：

- `accounts` 最少 2 项、最多 10 项。
- `username` 去除首尾空白后长度为 1 至 128；`password` 长度为 1 至 1024。
- `alias` 可省略，最长 64；输出优先使用别名，否则使用认证后的用户名。
- `range` 沿用 `today | live | 7d | 30d`，默认 `today`。
- `sections` 沿用现有 Dashboard 板块，去重后最多 7 项；省略表示全部。
- 拒绝未知字段，防止把任意内容传入登录或查询接口。

建议输出：

```json
{
  "status": "PARTIAL",
  "coverage": {
    "requested": 3,
    "authenticated": 2,
    "included": 2,
    "failed": 1
  },
  "currency": "CNY",
  "range": "30d",
  "summary": {
    "gmvAmount": 32000000,
    "paidAmount": 32000000,
    "pendingAmount": 1800000,
    "orderCount": 860,
    "cancelledOrderCount": 21,
    "averageDealAmount": 37209,
    "paymentRate": { "numerator": 860, "denominator": 945, "rate": 0.9100529 },
    "dealRate": { "numerator": 410, "denominator": 530, "rate": 0.7735849 }
  },
  "accounts": [],
  "rankings": {},
  "topLots": [],
  "categoryPerformance": [],
  "riskFlags": [],
  "partialErrors": [
    { "inputIndex": 2, "alias": "店铺C", "code": "AUTH_FAILED" }
  ]
}
```

输出不得包含密码、Token、Authorization Header、Cookie、内部异常堆栈或后端原始错误消息。失败项通过输入序号和安全别名定位；认证失败时不回显用户输入的登录名。

## 6. 聚合口径

- `gmvAmount`、`paidAmount`、`pendingAmount`、订单数和漏斗计数直接求和。
- `averageDealAmount = totalPaidAmount / totalPaidOrders`，不平均各账号的平均成交额。
- 支付率和成交率分别合并所有账号的分子与分母，再计算比率；不平均百分比。
- 金额只接受安全整数分，所有成功账号币种必须相同且为 `CNY`；否则该账号以 `CURRENCY_MISMATCH` 排除。
- 各账号统计范围必须与请求范围及 `Asia/Shanghai` 一致。返回快照时间可不同，账号明细保留各自 `snapshotAtUnixMs`。
- 不返回跨账号唯一参拍人数。账号明细仍可展示各租户内去重后的 `participantCount`，但不能把它们描述成全局唯一人数。
- 账号排名至少覆盖成交额、订单数、支付率和成交率。比率排名同时展示分母，避免小样本误导。
- 全局 Top 拍品从每个账号的 Top 列表合并并稳定排序；当前接口每账号返回 Top 10，足以确定全局 Top 10。相同金额按账号标识和拍品 ID 稳定排序。
- 品类表现按品类合并金额、开拍数、成交数及比率分子/分母；存在相应板块部分错误时标记结果不完整。
- 趋势按相同宽度、相同边界网格的绝对时间桶合并；各独立商家的 `live` 覆盖区间可以不同，此时按时间键取并集。粒度、网格或账号内连续性不兼容时不猜测对齐，返回 `INCOMPATIBLE_BUCKETS`。

## 7. 风险与异常规则

V1 只输出可以由数据确定的 `riskFlags`，不把模型判断包装成系统事实：

- `ACCOUNT_QUERY_FAILED`：账号登录成功但 Dashboard 查询失败。
- `DATA_INCOMPLETE`：账号 Dashboard 含 `partialErrors`。
- `EXPIRED_ORDERS`：过期待支付订单数大于 0。
- `PAYMENT_FAILED`：支付失败订单数大于 0。
- `NO_BID_LOTS`：已开拍拍品数大于有出价拍品数，返回差值。
- `UNPAID_SETTLED_LOTS`：已成交拍品数大于已支付拍品数，返回差值。
- `PAYMENT_RATE_BELOW_PORTFOLIO`：账号支付率分母至少 10，且比组合加权支付率低至少 20 个百分点。
- `DEAL_RATE_BELOW_PORTFOLIO`：账号成交率分母至少 10，且比组合加权成交率低至少 20 个百分点。
- `LOT_NO_CONVERSION`：拍品有出价但转化分子为 0；仅在 `lotPerformance` 完整时判断。

这些规则保留触发指标和阈值，方便 Skill 解释。排名靠后本身不等于异常；模型可以提出“值得关注”，但必须标注为分析推断。

## 8. 认证生命周期与并发

1. Tool 调用开始后，为每个输入项创建新的 `AuctionAuth` 和 `AuctionApi`。
2. 固定最多 3 个 worker；每个 worker 顺序执行一个账号的登录和 Dashboard 查询。
3. 登录返回后验证用户具备商家主账号身份以及查询所需权限。权限不足以 `PERMISSION_DENIED` 记录。
4. 以认证结果的 `mainAccountId` 去重。后出现的重复项记录 `DUPLICATE_ACCOUNT`，不重复统计。
5. 所有账号完成后执行纯函数聚合器。
6. 外层 `finally` 对全部临时 `AuctionAuth` 调用 `clear()`，包括取消、超时和聚合失败场景。
7. 现有会话级 `AuctionAuth` 不参与此流程，因此组合分析不会切换用户当前单账号登录态。

每个账号仍沿用登录和请求的 15 秒超时，同时服从 Harness 调用的取消信号。V1 不自动重试认证失败；只读 Dashboard 的网络失败也不自动重试，以免延长批量调用并模糊快照时间。

## 9. 失败降级

- 至少一个账号成功：返回 `OK` 或 `PARTIAL`，并明确 `included/requested` 覆盖率。
- 所有账号失败：返回结构化 `ALL_ACCOUNTS_FAILED`，不生成零值经营结论。
- 密码错误或登录失效：记录 `AUTH_FAILED`，不自动重试。
- 权限不足：记录 `PERMISSION_DENIED`，其数据不进入聚合。
- 后端超时或不可用：记录 `BACKEND_UNAVAILABLE`，保留其他账号结果。
- 同一商家重复提交：第一个有效账号进入聚合，其余记录 `DUPLICATE_ACCOUNT`。
- 成功账号币种、范围或趋势桶不兼容：排除不兼容板块或账号，并返回稳定错误码，不做隐式转换。
- 调用被取消：立即停止调度新账号，并清理所有已经创建的认证状态；取消不返回看似完整的部分报告。

Skill 必须在回答开头或指标旁明确披露部分覆盖，不能用“全部账号”描述不完整结果。

## 10. Skill 定义

插件从仅注入 `tools` 调整为注入 `tools` 与 `skills`，并通过 `ctx.skills.register()` 注册运行时 Skill。Skill 与插件一同交付，避免依赖 Harness 启动目录的文件系统发现路径。

```text
name: analyzing-auction-portfolio
description: 汇总、比较或诊断多个独立竞拍商家账号时使用；单账号查询或只需一次普通 Tool 调用时不要使用。
```

Skill 正文保持精简，只包含会改变模型决策的规则：

1. 至少两个账号且每个账号凭据完整后才能调用组合 Tool。
2. 用户只问单个指标时只请求必要板块；综合经营诊断才请求完整板块。
3. 不拆成多轮 `auction_login` + `auction_get_dashboard`，不让模型自行聚合。
4. 先检查 `status`、`coverage`、`partialErrors` 和币种，再解释数字。
5. 报告分为“统计事实、风险信号、分析推断、建议动作”；推断不得伪装为后端事实。
6. 不回显凭据，不把凭据复制到解释文本或后续 Tool 调用中。
7. 需要单账号精确下钻时，说明目标账号并使用正常单账号登录流程，不复用组合 Tool 的临时 Token。

该 Skill 自动可发现，但描述必须足够窄，避免普通单账号查询误加载。

## 11. 代码组织

预计只修改 Harness 集成，不修改 Go 核心服务：

```text
integrations/deepseek-harness/
├── src/
│   ├── auth.mjs                 # 复用 AuctionAuth，必要时补充安全错误分类
│   ├── portfolio-tool.mjs       # 参数规范化、临时认证编排和组合 Tool 入口
│   ├── portfolio-aggregate.mjs  # 无副作用的聚合、排名和风险规则
│   ├── portfolio-skill.mjs      # Skill 元数据与正文
│   ├── schemas.mjs              # 多账号输入输出 Schema
│   └── plugin.mjs               # 注册 Tool 与 Skill
└── test/
    ├── portfolio-aggregate.test.mjs
    ├── portfolio-tool.test.mjs
    └── portfolio-skill.test.mjs
```

`portfolio-aggregate.mjs` 不接触网络或凭据，便于覆盖统计边界。`portfolio-tool.mjs` 不生成自然语言经营建议。`portfolio-skill.mjs` 不保存统计实现或复制完整 Tool Schema。

## 12. 测试策略

### 12.1 聚合器单元测试

- 金额、订单、漏斗求和。
- 平均成交额按总额和总订单计算。
- 支付率和成交率按总分子/分母加权计算，零分母返回 0。
- 安全整数溢出、币种不一致和趋势桶不一致被拒绝或降级。
- Top 拍品稳定排序、品类合并、账号排名与风险阈值。
- 跨账号参与人数不生成全局唯一值。

### 12.2 组合 Tool 测试

- 2 至 10 个账号成功，且最多只有 3 个并发请求链。
- 输入不足、超限、未知字段和无效长度被拒绝。
- 登录失败、权限不足、Dashboard 超时、部分板块失败和全部失败。
- 两个登录名认证到同一 `mainAccountId` 时正确去重。
- 调用结束、失败和取消后所有临时认证对象均被清理。
- 组合调用不改变现有会话级登录账号。
- 输出和错误中不包含输入密码、Token 或后端不可信消息。

### 12.3 Skill 行为测试

先记录没有 Skill 时的基线行为，重点观察模型是否会循环切换单账号登录、遗漏覆盖率、平均百分比或把风险推断说成事实。加载 Skill 后验证：

- “统计这三个店最近 30 天总体表现”选择组合 Tool。
- “店铺 A 今天成交额”仍选择单账号 Dashboard Tool。
- 综合分析请求选择全部必要板块，单指标请求只选择相关板块。
- `PARTIAL` 结果明确披露覆盖率。
- 回答区分事实、风险信号、推断和建议。
- Skill 与 Tool 输出均不复述密码。

### 12.4 回归与运行时测试

- 现有 `auction_login`、`auction_login_status`、`auction_create_team_member` 和三个查询 Tool 全部通过。
- 插件在缺少 `ctx.skills` 时启动失败并给出明确配置错误，避免悄悄丢失 Skill。
- 真实 Harness 会话能够发现并加载 `analyzing-auction-portfolio`。
- 插件卸载和会话结束时，既有会话认证与临时组合认证均被清理。

## 13. 验收标准

1. 用户提供 2 至 10 组独立商家凭据后，可以一次获得跨账号经营报告。
2. 各账号认证状态彼此隔离，组合分析不改变当前单账号登录态。
3. 同一 `mainAccountId` 不会因不同登录名被重复统计。
4. 金额、平均成交额、支付率和成交率均由程序按明确口径计算。
5. 组合结果不声称存在跨账号全局去重的参与人数。
6. 单账号失败时返回其他账号数据和覆盖率；全部失败时不输出零值结论。
7. 风险标记可追溯到具体指标和规则，模型推断与系统事实明确分开。
8. 密码和 Token 不进入 Tool 输出或插件新增的持久化存储/主动日志；临时认证对象在所有退出路径清理，并明确披露聊天输入仍会进入模型上下文及 Harness 既有事件日志。
9. Skill 只路由多账号汇总、比较和诊断，不干扰普通单账号查询。
10. 现有 Tool 和 Go 接口保持兼容，全部回归测试通过。
