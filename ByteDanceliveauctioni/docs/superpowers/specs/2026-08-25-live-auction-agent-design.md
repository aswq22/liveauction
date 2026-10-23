# AI 直播竞拍运营 Agent 架构设计

日期：2026-08-25
状态：已完成方案讨论，等待用户审阅

> 说明：审批后创建拍品与入队的实现边界，已由
> `2026-08-28-agent-conversation-approval-design.md` 细化并取代。新设计要求
> Python Agent 始终只读，所有原子写入由 Go 审批执行器完成。

## 1. 背景与目标

现有项目已经包含 Go 竞拍后端、商家 React 控制台、买家 H5、MySQL、Redis、RabbitMQ、WebSocket、AI 买家咨询和可选 pgvector 搜索。本次改进不是增加一个聊天入口，而是增加一个可执行、可审批、可恢复、可审计和可评测的商家运营 Agent。

目标是同时体现两类秋招能力：

- Go 后端：领域边界、RBAC、租户隔离、gRPC、事务、幂等、Outbox、事件驱动和可观测性。
- AI Agent 应用：Harness、Skills、工具调用、持久化工作流、人工审批、记忆、Tracing 和 Evals。

首个完整闭环为：

```text
输入直播目标
→ 查询库存与历史数据
→ 生成排品及竞拍规则
→ 确定性风险校验
→ 人工审批
→ 调用 Go 工具创建场次和拍品
→ 验证执行结果
→ 直播结束后生成复盘
```

## 2. 非目标

第一版不实现：

- 买家自动代拍或任何自动资金决策。
- Agent 无审批修改价格、取消竞拍、结束竞拍或处理退款。
- 让模型直接连接或修改业务数据库。
- 为展示而拆分多个同质 Agent。
- 通用 Shell、桌面控制或不受限代码执行。
- 将现有 Go 后端继续拆成多个业务微服务。

## 3. 总体架构

新增一个 Python Agent 微服务，现有 Go 后端继续作为业务控制面和唯一权威写入口。

```text
React 管理端
  │ HTTP + SSE
  ▼
Go Auction Backend
  ├── Agent Run API
  ├── 审批、RBAC、租户隔离和审计
  ├── gRPC Agent Runtime Client ──────────────┐
  └── gRPC Auction Agent Tool Server ◄──────┐ │
                                            │ │
                                      Python Agent Service
                                      ├── AuctionAgentHarness
                                      ├── LangGraph
                                      ├── Skill Loader
                                      ├── Model Adapter
                                      ├── gRPC Tool Client
                                      └── Checkpoint / Memory

Go Outbox ─→ RabbitMQ ─→ Python Event Consumer
Python Run Event Stream ─→ Go SSE Gateway ─→ React
```

### 3.1 Go 后端职责

- 用户认证、RBAC、商家数据隔离。
- 直播间、拍品、竞价、订单和支付等核心领域状态。
- MySQL 事务、Redis Lua、Transactional Outbox 和 RabbitMQ 事件。
- Agent Run、审批记录和业务工具审计。
- 所有写工具的参数、权限、版本、审批和幂等校验。
- 将 Python gRPC 事件转换为面向浏览器的 SSE。

### 3.2 Python Agent Service 职责

- Agent Harness 和 LangGraph 执行图。
- Skill 发现、选择、按需加载和版本记录。
- 模型调用、上下文构建和结构化输出。
- 调用 Go 提供的受限 gRPC 工具。
- Checkpoint、暂停恢复、重试和停止条件。
- Agent 记忆、Tracing、成本统计和 Evals。
- 消费直播领域事件并生成实时运营建议。

### 3.3 数据所有权

| 数据 | 权威所有者 |
| --- | --- |
| 用户、商家、商品、直播间、拍品、出价、订单 | Go / MySQL |
| Agent Run、审批和业务写操作审计 | Go / MySQL |
| LangGraph Checkpoint、工作记忆、Embedding | Python / PostgreSQL + pgvector |
| 临时锁、缓存、限流状态 | Redis |
| 直播领域事件 | Go Outbox / RabbitMQ |

不使用跨 Go/Python 分布式事务。跨服务一致性依赖状态机、幂等键、业务版本和补偿操作。

## 4. AuctionAgentHarness

Harness 是 Python 服务中包围模型、Skills、Tools 和 LangGraph 的领域运行框架。LangGraph 只负责执行图与 Checkpoint，不等同于完整 Harness。

```text
AuctionAgentHarness
├── Run Manager
├── Context Builder
├── Skill Registry / Loader
├── Model Adapter
├── Tool Registry / gRPC Client
├── Policy Engine
├── LangGraph Runtime
├── Checkpoint Store
├── Trace Collector
└── Evaluator
```

### 4.1 Run Manager

每次任务设置最大步骤、截止时间、Token 预算、模型预算和重试次数。支持创建、暂停、恢复、取消和查询，不允许无限自主循环。

### 4.2 Context Builder

模型上下文由稳定策略、用户目标、当前 Run 状态、已选 Skill、允许工具、最近观察、商家偏好和未解决问题组成。压缩时必须保留已完成动作、业务 ID、工具结果、审批状态、阻塞项和下一步目标。

### 4.3 Model Adapter

模型层使用统一异步接口，首版支持当前 DeepSeek 的 OpenAI-compatible API，并保留切换 OpenAI 模型和测试 FakeModel 的能力。所有计划、工具调用和最终产物使用 Pydantic 结构化类型，不依赖从自由文本提取 JSON。

### 4.4 Tool Registry

每个工具定义名称、输入输出 Schema、只读属性、风险等级、超时、重试安全性、审批要求和对应 gRPC 方法。模型只能提出工具调用，Harness 和 Go 后端共同决定是否执行。

### 4.5 Policy Engine

工具执行前依次校验：

1. 参数是否符合 Schema。
2. 当前 Skill 是否声明允许该工具。
3. Run 是否拥有对应权限范围。
4. tenant、operator 与 Run 是否匹配。
5. 是否需要审批以及审批是否覆盖当前动作摘要。
6. 写操作是否携带稳定幂等键和期望业务版本。

模型输出的 `approved=true` 不具有任何授权意义。

## 5. Agent 状态机

```text
CREATED
→ PLANNING
→ COLLECTING_CONTEXT
→ GENERATING_PROPOSAL
→ VALIDATING
→ WAITING_APPROVAL
→ EXECUTING
→ VERIFYING
→ COMPLETED
```

终止或恢复状态包括：

- `FAILED`：不可恢复失败。
- `CANCELLED`：用户或系统取消。
- `RETRY_SCHEDULED`：可重试的临时故障。
- `COMPENSATING`：部分写入成功后执行补偿或转人工处理。

LangGraph 节点设计：

```text
parse_goal
→ select_skill
→ collect_context
→ generate_plan
→ validate_plan
→ risk_check
   ├── 无需审批 → execute
   └── 需要审批 → wait_approval → resume → execute
→ verify
   ├── 缺少证据 → collect_context
   ├── 可恢复失败 → retry
   ├── 部分成功 → compensate
   └── 成功 → complete
```

## 6. Skills

第一版提供四个领域 Skill：

1. `plan_live_session`：选择商品、设计拍品顺序和竞拍规则、输出多档方案。
2. `execute_approved_session`：根据审批内容幂等创建直播间、拍品和排品。
3. `monitor_live_auction`：消费实时事件，生成运营建议，不直接执行高风险动作。
4. `generate_session_review`：汇总计划与实际表现，生成可追溯复盘和候选长期偏好。

Skill 包结构：

```text
skills/plan_live_session/
├── SKILL.md
├── schema/
│   ├── input.json
│   └── output.json
├── references/
├── examples/
└── evals/
```

Harness 启动时只载入 Skill 名称、描述和版本；选中后才加载完整指令、参考资料和示例。Skill 声明可申请的工具范围，但不授予权限。

## 7. 双向 gRPC 契约

契约放入共享的 `proto/agent/v1`，生成 Go 与 Python 代码并在 CI 中验证生成结果。

### 7.1 Python 提供的 Runtime Service

```protobuf
service AgentRuntimeService {
  rpc StartRun(StartRunRequest) returns (StartRunReply);
  rpc ResumeRun(ResumeRunRequest) returns (ResumeRunReply);
  rpc CancelRun(CancelRunRequest) returns (CancelRunReply);
  rpc GetRun(GetRunRequest) returns (GetRunReply);
  rpc StreamRunEvents(StreamRunEventsRequest)
      returns (stream AgentRunEvent);
}
```

### 7.2 Go 提供的 Tool Service

首版工具包括：

- `QueryInventory`
- `QueryAuctionHistory`
- `GetSessionSnapshot`
- `CreateSession`
- `CreateLots`
- `ScheduleLots`

写工具必须支持幂等键，并返回稳定的业务错误码、资源 ID 和最终版本。

### 7.3 请求上下文

每次调用携带由系统注入而非模型生成的上下文：

```protobuf
message RequestContext {
  string request_id = 1;
  string trace_id = 2;
  string run_id = 3;
  string tenant_id = 4;
  string operator_id = 5;
  string approval_id = 6;
  string idempotency_key = 7;
}
```

本地环境使用内部服务 Token；生产扩展方向为 mTLS。Go 不信任 Python 单独传入的 tenant、operator 或 approval，必须与服务身份、Run 和数据库记录交叉验证。

### 7.4 Proto 兼容规则

- 已发布字段编号不复用。
- 删除字段使用 `reserved`。
- 枚举提供 `UNSPECIFIED = 0`。
- 新字段保持向后兼容默认值。
- 不直接修改已发布字段类型或语义。

## 8. RabbitMQ 与实时监控

gRPC 用于同步任务控制、查询、命令和运行轨迹；RabbitMQ 用于领域事件、异步通知和失败重放，两者不互相替代。

Python 订阅版本化领域事件，根据 `event_id` 幂等消费。第一版实时 Skill 只产生建议。任何调价、取消或结束动作都必须形成新的待审批 ProposedAction。

## 9. 错误、重试与补偿

gRPC 状态映射：

| Status | Harness 行为 |
| --- | --- |
| `InvalidArgument` | 允许模型有限次数修正参数 |
| `Unauthenticated` / `PermissionDenied` | 立即终止并记录安全事件 |
| `NotFound` | 重新收集上下文或终止 |
| `AlreadyExists` | 按幂等结果读取已有资源 |
| `FailedPrecondition` | 刷新业务状态后重新规划 |
| `Aborted` | 处理乐观锁冲突，不盲目重放 |
| `ResourceExhausted` | 按限流提示延迟重试 |
| `Unavailable` / `DeadlineExceeded` | 仅对重试安全操作指数退避 |

部分写入成功时，不让模型自由决定回滚。Harness 进入 `COMPENSATING`，执行预先定义的确定性补偿；没有安全补偿时转人工处理。

## 10. 记忆设计

- 工作记忆：当前 Run 的目标、计划、观察、动作、审批和错误，随 Checkpoint 保存。
- 长期记忆：经用户确认的商家偏好与历史复盘事实。
- 检索知识：竞拍规则、定价政策和经过版本管理的运营资料。

不得将所有聊天记录无界保存为长期记忆。长期记忆必须有来源、tenant、版本、创建时间、置信度和用户确认状态。

## 11. 可观测性与评测

OpenTelemetry Trace 贯穿浏览器请求、Go API、Python Run、模型调用、gRPC Tool、MySQL 事务和 RabbitMQ 事件。每个 Span 记录必要元数据，默认不记录密钥、完整 Prompt 或敏感业务数据。

核心指标：

- Run 完成率、平均耗时、暂停时长和恢复成功率。
- Tool 选择准确率、参数合法率、调用成功率和重试次数。
- 审批前后方案变更率。
- Token、模型调用成本和缓存命中率。
- Skill 选择准确率和各版本回归结果。

离线 Eval 至少覆盖：

- 目标解析是否正确。
- 是否选择正确 Skill 和最少必要工具。
- 是否引用真实业务证据。
- 是否违反成本、租户和审批边界。
- 故障注入下是否正确重试、暂停或终止。
- 同一写动作重放时是否保持幂等。

## 12. 测试策略

- Python 单元测试：Skill Loader、Context Builder、Policy、重试和状态转移。
- FakeModel 测试：固定模型决策，验证完整 Graph，不依赖外部模型稳定性。
- Proto 契约测试：Go/Python 生成代码和兼容性检查。
- gRPC 集成测试：权限、租户、审批、幂等、Deadline 和错误映射。
- 端到端测试：目标输入到创建场次，再到复盘报告。
- 故障测试：Python Worker 重启、RabbitMQ 重投、Go Tool 超时、模型超时和部分成功。
- Eval 回归：Skill 或 Prompt 变更必须运行固定数据集，并比较质量、成本和延迟。

## 13. 部署

在现有 Docker Compose 中增加：

- `auction-agent`：Python gRPC Server 和 RabbitMQ Consumer。
- `pgvector`：从可选搜索组件扩展为 Agent Checkpoint、记忆和向量检索存储；生产可拆分数据库实例。

现有 `auction-backend` 不拆分。Agent 服务失败时，竞价、订单和 WebSocket 主业务仍必须正常工作。

## 14. 分阶段交付

### 第一阶段：只读规划闭环

完成 gRPC 基础、Run 状态、Harness、`plan_live_session`、只读工具、结构化方案和 Trace。

### 第二阶段：审批执行闭环

完成审批、写工具、幂等、Checkpoint 恢复、执行验证和 Agent 控制台。

### 第三阶段：事件监控与复盘

接入 RabbitMQ 领域事件，完成 `monitor_live_auction` 和 `generate_session_review`。

### 第四阶段：质量与扩展

完善 Eval 数据集、故障测试、长期记忆和 MCP 只读适配器。只有出现明确的上下文隔离或并行需求时，才将数据分析拆为 Subagent。

## 15. 验收标准

系统达到第一版完成状态时必须满足：

1. 用户能从商家端提交自然语言直播目标并看到流式执行轨迹。
2. Agent 只通过 gRPC 工具读取业务数据，不直接访问业务 MySQL。
3. Agent 能输出带依据的结构化场次方案。
4. 未审批的写工具调用被 Python 和 Go 双层拒绝。
5. 审批后可幂等创建直播间、拍品与排品，并验证结果。
6. Python Worker 在等待审批或执行中重启后能从 Checkpoint 恢复。
7. 重复消息、重复恢复和重复写请求不会生成重复业务资源。
8. 关键链路可通过 trace_id 跨 Go、Python、gRPC 和 RabbitMQ 查询。
9. 固定 Eval 数据集可在 CI 中运行并输出质量、成本与延迟报告。
10. Agent 服务不可用时，现有直播竞拍主链路不受影响。
