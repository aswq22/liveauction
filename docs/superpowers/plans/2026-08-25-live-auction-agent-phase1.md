# AI 直播竞拍 Agent 第一阶段实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付一个可运行的只读直播策划闭环：商家从后台提交目标，Python Agent 通过双向 gRPC 查询商家自有拍品与历史成交，使用 Skill 和 LangGraph 生成结构化方案，并通过 Go SSE 网关展示完整运行轨迹。

**Architecture:** 现有 Go 后端新增 Agent Run 控制面、Python Runtime gRPC Client 和只读 Tool gRPC Server；新建 Python Agent 微服务，以 LangGraph 1.2.11 驱动自研 AuctionAgentHarness，并使用 PostgreSQL/pgvector 保存运行状态、Checkpoint 与事件。浏览器只访问 Go HTTP/SSE，Python 不直接访问业务 MySQL。

**Tech Stack:** Go 1.22、Kratos、gRPC/Protobuf、Python 3.12、grpcio 1.83.0、LangGraph 1.2.11、langgraph-checkpoint-postgres 3.1.2、Pydantic 2.13.4、OpenAI Python SDK 3.3.1、PostgreSQL 16 + pgvector、React 19、TypeScript、Docker Compose。

**Spec:** `docs/superpowers/specs/2026-08-25-live-auction-agent-design.md`

## Global Constraints

- Go 后端是业务数据、租户权限和 Agent Run 所有权的唯一权威入口。
- Python Agent 不得连接业务 MySQL，只能调用 Go gRPC Tool Service。
- 第一阶段只有只读工具，不包含创建直播间、创建拍品、排品、调价、取消、支付或退款。
- 所有工具调用必须携带 `run_id`；Go 必须从 Agent Run 记录恢复 tenant/operator，不信任 Python 声明的身份。
- 浏览器继续使用 HTTP；运行事件通过 Go 代理的 SSE 返回，不引入 gRPC-Web。
- gRPC 用于同步控制和工具调用，RabbitMQ 在第三阶段才接入 Agent Runtime。
- Agent Service 不可用时，现有竞价、订单和 WebSocket 主链路必须正常启动和工作。
- 模型调用默认 `mock`；启用 DeepSeek/OpenAI-compatible Provider 时密钥只来自环境变量。
- Python 容器固定使用 Python 3.12，避免依赖宿主机 Python 3.14。
- 所有新增功能遵循 TDD；每个任务完成后运行其定向测试并独立提交。

---

## 文件结构

```text
proto/agent/v1/
├── common.proto                 # 请求上下文、Run/Event 枚举和公共金额类型
├── runtime.proto                # Go 调 Python 的 Agent Runtime 契约
└── tool.proto                   # Python 调 Go 的只读领域工具契约

scripts/
└── generate-agent-proto.ps1     # 同时生成 Go/Python gRPC 代码

live-auction-bid-backend/
├── api/agent/v1/                # 生成的 Go protobuf/grpc 文件
├── app/auction/service/internal/agentcontrol/
│   ├── model.go                 # Run/Event 领域类型与仓储接口
│   └── service.go               # Run 创建、所有权验证和 Runtime 调用
├── app/auction/service/internal/agenttool/
│   ├── service.go               # 只读 Tool gRPC 实现
│   └── auth.go                  # 服务 Token 与 Run 身份校验
├── app/auction/service/internal/data/
│   ├── agent_run_models.go      # GORM Agent Run/Event 模型
│   └── agent_run_repo.go        # Go 侧 Run/Event 仓储
├── app/auction/service/internal/server/
│   ├── agent_grpc.go            # Go Tool gRPC Server
│   └── agent_http.go            # Run HTTP/SSE Gateway
└── app/auction/service/cmd/server/main.go

live-auction-agent-service/
├── pyproject.toml
├── Dockerfile
├── app/
│   ├── config.py
│   ├── main.py
│   ├── generated/               # 生成的 Python protobuf/grpc 文件
│   ├── domain/models.py         # Pydantic Run/Decision/Proposal 类型
│   ├── harness/harness.py       # 统一 Agent 运行入口
│   ├── harness/context.py       # 上下文预算与观测整理
│   ├── graph/state.py           # LangGraph State
│   ├── graph/workflow.py        # decide/tool/finalize 状态图
│   ├── models/base.py           # ModelAdapter 协议
│   ├── models/fake.py           # 可重复测试模型
│   ├── models/openai_compat.py  # DeepSeek/OpenAI-compatible Adapter
│   ├── runtime/repository.py    # PostgreSQL Run/Event 持久化
│   ├── runtime/service.py       # AgentRuntimeService gRPC 实现
│   ├── skills/registry.py       # Skill 元数据发现与按需加载
│   ├── skills/packages/plan_live_session/
│   │   ├── SKILL.md
│   │   ├── references/pricing-policy.md
│   │   └── examples/balanced.json
│   └── tools/
│       ├── registry.py          # Tool Schema、风险和 Skill 允许列表
│       └── grpc_client.py       # AuctionAgentToolService Client
└── tests/

live-auction-bid-frontend/src/features/agent-console/
├── api/agentApi.ts              # Run HTTP 与 SSE fetch-stream Client
├── model/types.ts               # Agent Run/Event/Proposal 类型
├── AgentConsolePage.tsx         # 目标输入、状态、轨迹和方案页面
└── agent-console.css
```

---

### Task 1: 建立共享 Proto 与双语言代码生成

**Files:**
- Create: `proto/agent/v1/common.proto`
- Create: `proto/agent/v1/runtime.proto`
- Create: `proto/agent/v1/tool.proto`
- Create: `scripts/generate-agent-proto.ps1`
- Create: `live-auction-bid-backend/api/agent/v1/contract_test.go`
- Create: generated files under `live-auction-bid-backend/api/agent/v1/`
- Create: generated files under `live-auction-agent-service/app/generated/proto/agent/v1/`
- Create: `__init__.py` files under `live-auction-agent-service/app/generated/`, `proto/`, `proto/agent/`, and `proto/agent/v1/`

**Interfaces:**
- Produces: `agent.v1.AgentRuntimeServiceClient`, `agent.v1.AgentRuntimeServiceServer`, `agent.v1.AuctionAgentToolServiceClient`, `agent.v1.AuctionAgentToolServiceServer`.
- Produces: `RequestContext`, `AgentRun`, `AgentRunEvent`, `InventoryLot`, `AuctionHistoryItem`, `SessionSnapshot`.

- [ ] **Step 1: 写会因契约缺失而失败的 Go 测试**

```go
func TestPhaseOneProtoDefaultsAreSafe(t *testing.T) {
    if RunStatus_RUN_STATUS_UNSPECIFIED != 0 {
        t.Fatal("run status zero value must be unspecified")
    }
    req := &QueryInventoryRequest{Context: &RequestContext{RunId: "run-1"}}
    if req.GetContext().GetRunId() != "run-1" {
        t.Fatal("request context did not round trip")
    }
}
```

- [ ] **Step 2: 运行测试并确认因生成包不存在而失败**

Run: `cd live-auction-bid-backend; go test ./api/agent/v1`

Expected: FAIL，提示 `api/agent/v1` 尚无非测试 Go 文件或类型未定义。

- [ ] **Step 3: 编写三个 Proto 契约**

`common.proto` 必须包含：

```protobuf
syntax = "proto3";
package agent.v1;
option go_package = "live-auction-bid/backend/api/agent/v1;agentv1";

message RequestContext {
  string request_id = 1;
  string trace_id = 2;
  string run_id = 3;
  string tenant_id = 4;
  string operator_id = 5;
}

enum RunStatus {
  RUN_STATUS_UNSPECIFIED = 0;
  RUN_STATUS_CREATED = 1;
  RUN_STATUS_PLANNING = 2;
  RUN_STATUS_COLLECTING_CONTEXT = 3;
  RUN_STATUS_GENERATING_PROPOSAL = 4;
  RUN_STATUS_VERIFYING = 5;
  RUN_STATUS_COMPLETED = 6;
  RUN_STATUS_FAILED = 7;
  RUN_STATUS_CANCELLED = 8;
}
```

`runtime.proto` 定义 `StartRun`、`GetRun`、`CancelRun`、`StreamRunEvents`；`StartRunRequest` 必须包含由 Go 生成的 `run_id`、`RequestContext` 和 `goal`，Python 不得另行生成 Run ID；流请求包含 `run_id` 与 `after_seq`。`AgentRun` 保存 goal、status、proposal_json、error_code、error_message、created_at_unix_ms、updated_at_unix_ms。

`tool.proto` 定义：

```protobuf
service AuctionAgentToolService {
  rpc QueryInventory(QueryInventoryRequest) returns (QueryInventoryReply);
  rpc QueryAuctionHistory(QueryAuctionHistoryRequest) returns (QueryAuctionHistoryReply);
  rpc GetSessionSnapshot(GetSessionSnapshotRequest) returns (GetSessionSnapshotReply);
}
```

`InventoryLot` 只包含商家自有拍品的 id、room_id、title、category、description、tags、status、stock、start_price、current_price、created_at 和 updated_at。历史项包含拍品状态、最终价、订单/支付状态和参拍结果，不包含买家敏感信息。

- [ ] **Step 4: 编写可重复的 PowerShell 生成脚本**

```powershell
python -m grpc_tools.protoc `
  -I . `
  --python_out=live-auction-agent-service/app/generated `
  --grpc_python_out=live-auction-agent-service/app/generated `
  --go_out=live-auction-bid-backend `
  --go_opt=module=live-auction-bid/backend `
  --go-grpc_out=live-auction-bid-backend `
  --go-grpc_opt=module=live-auction-bid/backend `
  proto/agent/v1/common.proto proto/agent/v1/runtime.proto proto/agent/v1/tool.proto
```

脚本启动前使用 `Get-Command protoc-gen-go` 与 `Get-Command protoc-gen-go-grpc` 检查插件；缺失时输出明确安装命令并以非零退出。
生成后脚本为 Python 生成目录的每一级补齐空 `__init__.py`，确保 `app.generated.proto.agent.v1` 在 pytest、Docker 和 IDE 中使用一致的包语义。

- [ ] **Step 5: 生成代码并运行契约测试**

Run: `powershell -ExecutionPolicy Bypass -File .\scripts\generate-agent-proto.ps1`

Run: `cd live-auction-bid-backend; go test ./api/agent/v1`

Expected: PASS。

- [ ] **Step 6: 提交契约**

```bash
git add proto scripts/generate-agent-proto.ps1 live-auction-bid-backend/api/agent/v1 live-auction-agent-service/app/generated
git commit -m "feat(agent): add shared grpc contracts"
```

---

### Task 2: 持久化 Go Agent Run 所有权

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/model.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_run_models.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_run_repo.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_run_repo_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/store.go`

**Interfaces:**
- Produces: `agentcontrol.RunRepository.Create(ctx, Run)`, `Find(ctx, runID)`, `UpdateStatus(ctx, runID, expectedVersion, status, proposalJSON, errorCode, errorMessage)`, `AppendEvent(ctx, Event)`, `ListEvents(ctx, runID, afterSeq, limit)`.
- Produces authoritative ownership lookup through `RunRepository.Find(ctx, runID)`；Task 3 和 Task 8 必须使用该记录校验 tenant/operator。

- [ ] **Step 1: 写仓储隔离和乐观锁失败测试**

```go
func TestAgentRunRepositoryPreservesOwnerAndVersion(t *testing.T) {
    repo := newAgentRunTestStore(t)
    run := agentcontrol.Run{ID: "run-1", TenantID: "merchant-1", OperatorID: "user-1", Goal: "清理库存", Status: agentcontrol.StatusCreated, Version: 1}
    require.NoError(t, repo.CreateAgentRun(context.Background(), run))
    got, err := repo.FindAgentRun(context.Background(), "run-1")
    require.NoError(t, err)
    require.Equal(t, "merchant-1", got.TenantID)
    require.ErrorIs(t, repo.UpdateAgentRun(context.Background(), "run-1", 99, agentcontrol.StatusPlanning, "", "", ""), apperr.ErrConflict)
}
```

- [ ] **Step 2: 运行定向测试确认失败**

Run: `cd live-auction-bid-backend; go test ./app/auction/service/internal/data -run AgentRun -count=1 -v`

Expected: FAIL，缺少 Agent Run 模型和仓储方法。

- [ ] **Step 3: 实现领域类型和独立 GORM 模型**

`agentcontrol.Run` 使用字符串状态常量并包含 `ID/TenantID/OperatorID/TraceID/Goal/Status/ProposalJSON/ErrorCode/ErrorMessage/Version/CreatedAtUnixMs/UpdatedAtUnixMs`。数据库表名固定为 `agent_runs`；`tenant_id, updated_at` 建组合索引，`id` 为主键。

事件表 `agent_run_events` 使用 `(run_id, seq)` 唯一索引，字段为 event_type、message、payload_json 和 created_at_unix_ms。

- [ ] **Step 4: 将模型加入 AutoMigrate 并实现仓储**

更新必须使用：

```go
result := db.Model(&AgentRunModel{}).
    Where("id = ? AND version = ?", runID, expectedVersion).
    Updates(map[string]any{"status": status, "version": gorm.Expr("version + 1"), "updated_at_unix_ms": now})
if result.RowsAffected == 0 { return apperr.ErrConflict }
```

- [ ] **Step 5: 运行数据层测试**

Run: `cd live-auction-bid-backend; go test ./app/auction/service/internal/data -run AgentRun -count=1 -v`

Expected: PASS。

- [ ] **Step 6: 提交 Run 持久化**

```bash
git add live-auction-bid-backend/app/auction/service/internal/agentcontrol live-auction-bid-backend/app/auction/service/internal/data
git commit -m "feat(agent): persist agent run ownership"
```

---

### Task 3: 实现 Go 只读 Tool gRPC Server

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/agenttool/auth.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agenttool/service.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agenttool/service_test.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/server/agent_grpc.go`
- Modify: `live-auction-bid-backend/app/auction/service/cmd/server/main.go`
- Modify: `live-auction-bid-backend/deploy/docker-compose.yml`

**Interfaces:**
- Consumes: generated `AuctionAgentToolServiceServer`, `auctionUsecase.ListLotsByQuery`, `auctionUsecase.ListOrders`, Agent Run repository.
- Produces: Go gRPC listener at `AUCTION_AGENT_TOOL_GRPC_ADDR` (default `:19082`).

- [ ] **Step 1: 写服务 Token、Run 所有权和租户过滤测试**

```go
func TestQueryInventoryUsesPersistedRunTenant(t *testing.T) {
    fixture := newToolServiceFixture(t, "secret", agentcontrol.Run{ID: "run-1", TenantID: "merchant-a", OperatorID: "user-a"})
    ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret"))
    reply, err := fixture.Service.QueryInventory(ctx, &agentv1.QueryInventoryRequest{Context: &agentv1.RequestContext{RunId: "run-1", TenantId: "merchant-b"}})
    require.NoError(t, err)
    require.NotEmpty(t, reply.GetLots())
    require.Equal(t, "merchant-a", fixture.LastLotQuery.MainAccountID)
}
```

同时覆盖：缺少 Token 返回 `Unauthenticated`、未知 Run 返回 `NotFound`、消息 tenant 与数据库 tenant 不同时仍以数据库记录为准。

- [ ] **Step 2: 运行测试确认失败**

Run: `cd live-auction-bid-backend; go test ./app/auction/service/internal/agenttool -count=1 -v`

Expected: FAIL，包尚不存在。

- [ ] **Step 3: 实现 Tool 身份验证和数据映射**

Token 使用 `subtle.ConstantTimeCompare`；只接受 `authorization: Bearer <token>`。`QueryInventory` 查询当前 Run tenant 的 DRAFT、READY、QUEUED 拍品并按更新时间倒序返回最多 100 条。`QueryAuctionHistory` 查询 SETTLED、CANCELLED、FAILED 拍品与同 tenant 订单，聚合最终价与支付状态。`GetSessionSnapshot` 先验证 room 归属 tenant，再返回当前拍品和队列摘要。

- [ ] **Step 4: 启动独立 Kratos gRPC Server**

```go
func NewAgentToolGRPCServer(addr string, svc agentv1.AuctionAgentToolServiceServer) *grpc.Server {
    server := grpc.NewServer(grpc.Address(addr), grpc.Middleware(recovery.Recovery()))
    agentv1.RegisterAuctionAgentToolServiceServer(server, svc)
    return server
}
```

`main.go` 使用 `errgroup.WithContext` 并发启动 HTTP 与 Tool gRPC；未配置 Agent Runtime 不影响 HTTP/竞拍主链路。

- [ ] **Step 5: 运行 Tool 测试与 Go 全量测试**

Run: `cd live-auction-bid-backend; go test ./app/auction/service/internal/agenttool -count=1 -v`

Run: `cd live-auction-bid-backend; go test ./...`

Expected: 全部 PASS。

- [ ] **Step 6: 提交 Go Tool Server**

```bash
git add live-auction-bid-backend
git commit -m "feat(agent): expose read-only auction grpc tools"
```

---

### Task 4: 创建 Python Agent Service 基础与配置

**Files:**
- Create: `live-auction-agent-service/pyproject.toml`
- Create: `live-auction-agent-service/Dockerfile`
- Create: `live-auction-agent-service/app/__init__.py`
- Create: `live-auction-agent-service/app/config.py`
- Create: `live-auction-agent-service/tests/test_config.py`
- Modify: `.gitignore`

**Interfaces:**
- Produces: `Settings.from_env() -> Settings`。
- Produces exact fields: `grpc_addr`, `tool_grpc_addr`, `database_dsn`, `internal_service_token`, `model_provider`, `model_base_url`, `model_name`, `model_api_key`, `model_timeout_seconds`, `max_steps`, `token_budget`.

- [ ] **Step 1: 写环境变量解析失败测试**

```python
def test_settings_require_service_token(monkeypatch):
    monkeypatch.delenv("AUCTION_INTERNAL_SERVICE_TOKEN", raising=False)
    with pytest.raises(ValueError, match="AUCTION_INTERNAL_SERVICE_TOKEN"):
        Settings.from_env()
```

- [ ] **Step 2: 运行测试确认模块缺失**

Run: `cd live-auction-agent-service; python -m pytest tests/test_config.py -v`

Expected: FAIL，`app.config` 不存在。

- [ ] **Step 3: 编写 pyproject 与 Settings**

直接依赖固定为：`grpcio==1.83.0`、`grpcio-tools==1.83.0`、`langgraph==1.2.11`、`langgraph-checkpoint-postgres==3.1.2`、`psycopg[binary,pool]==3.3.4`、`pydantic==2.13.4`、`openai==3.3.1`、`opentelemetry-sdk==1.44.0`；dev 依赖为 `pytest==9.1.1`、`pytest-asyncio==1.4.0`。

默认模型 Provider 为 `mock`。非 mock 时必须同时提供 base URL、model 和 API key。日志输出只记录 `api_key_configured=true/false`。

- [ ] **Step 4: 编写 Python 3.12 Dockerfile**

```dockerfile
FROM python:3.12-slim
WORKDIR /app
COPY live-auction-agent-service/pyproject.toml ./
COPY live-auction-agent-service/app ./app
RUN pip install --no-cache-dir .
CMD ["python", "-m", "app.main"]
```

- [ ] **Step 5: 运行配置测试**

Run: `cd live-auction-agent-service; python -m pytest tests/test_config.py -v`

Expected: PASS。

- [ ] **Step 6: 提交 Python 服务骨架**

```bash
git add .gitignore live-auction-agent-service
git commit -m "feat(agent): scaffold python agent runtime"
```

---

### Task 5: 实现 Skill Registry 与 plan_live_session Skill

**Files:**
- Create: `live-auction-agent-service/app/skills/registry.py`
- Create: `live-auction-agent-service/app/skills/packages/plan_live_session/SKILL.md`
- Create: `live-auction-agent-service/app/skills/packages/plan_live_session/references/pricing-policy.md`
- Create: `live-auction-agent-service/app/skills/packages/plan_live_session/examples/balanced.json`
- Create: `live-auction-agent-service/tests/skills/test_registry.py`

**Interfaces:**
- Produces: `SkillMetadata(name, description, version, allowed_tools)`。
- Produces: `SkillRegistry.discover() -> list[SkillMetadata]` and `load(name: str) -> LoadedSkill`。

- [ ] **Step 1: 写渐进加载和路径逃逸测试**

```python
def test_registry_reads_metadata_before_full_body(skill_root):
    registry = SkillRegistry(skill_root)
    metadata = registry.discover()[0]
    assert metadata.name == "plan_live_session"
    assert "pricing-policy.md" not in metadata.description
    loaded = registry.load(metadata.name)
    assert "inventory.query" in loaded.allowed_tools

def test_registry_rejects_path_traversal(skill_root):
    with pytest.raises(SkillNotFound):
        SkillRegistry(skill_root).load("../secret")
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd live-auction-agent-service; python -m pytest tests/skills/test_registry.py -v`

Expected: FAIL，Skill Registry 尚不存在。

- [ ] **Step 3: 实现版本化 Skill 加载器**

`SKILL.md` front matter 固定为：

```yaml
---
name: plan_live_session
description: 根据商家自有待拍品和历史成交生成只读直播排品方案
version: 1.0.0
allowed_tools:
  - inventory.query
  - auction.history.query
  - session.snapshot.get
---
```

正文规定：只引用工具返回的 lot ID；必须给出保守、均衡、激进三档中的一个推荐方案；不得声称已经创建或修改业务数据；价格建议不得低于零且必须标注推断依据。

- [ ] **Step 4: 运行 Skill 测试**

Run: `cd live-auction-agent-service; python -m pytest tests/skills/test_registry.py -v`

Expected: PASS。

- [ ] **Step 5: 提交 Skill 系统**

```bash
git add live-auction-agent-service/app/skills live-auction-agent-service/tests/skills
git commit -m "feat(agent): add live session planning skill"
```

---

### Task 6: 实现结构化模型 Adapter 与 Tool Registry

**Files:**
- Create: `live-auction-agent-service/app/domain/models.py`
- Create: `live-auction-agent-service/app/models/base.py`
- Create: `live-auction-agent-service/app/models/fake.py`
- Create: `live-auction-agent-service/app/models/openai_compat.py`
- Create: `live-auction-agent-service/app/tools/registry.py`
- Create: `live-auction-agent-service/app/tools/grpc_client.py`
- Create: `live-auction-agent-service/tests/models/test_model_adapter.py`
- Create: `live-auction-agent-service/tests/tools/test_registry.py`

**Interfaces:**
- Produces: `ModelAdapter.decide(context: AgentContext) -> AgentDecision`。
- Produces: discriminated union `ToolCallDecision | FinalProposalDecision`。
- Produces: `ToolRegistry.execute(name, arguments, run_context) -> Observation`。

- [ ] **Step 1: 写结构化输出和越权工具测试**

```python
async def test_fake_model_returns_typed_decisions():
    model = FakeModel([
        ToolCallDecision(
            kind="tool_call",
            tool="inventory.query",
            arguments={"limit": 50},
        )
    ])
    decision = await model.decide(sample_context())
    assert decision.tool == "inventory.query"

async def test_tool_registry_rejects_tool_outside_skill():
    registry = ToolRegistry(fake_grpc_client())
    with pytest.raises(ToolPolicyError):
        await registry.execute("session.create", {}, sample_run(), allowed_tools={"inventory.query"})
```

- [ ] **Step 2: 运行测试确认失败**

Run: `cd live-auction-agent-service; python -m pytest tests/models tests/tools -v`

Expected: FAIL，模型与工具类型尚不存在。

- [ ] **Step 3: 实现 Pydantic 领域类型**

`SessionProposal` 包含 `summary`、`strategy`、`estimated_duration_minutes`、`lots: list[LotPlan]`、`evidence: list[EvidenceRef]`、`risks: list[str]`。`LotPlan` 必须引用真实 `lot_id`，包含 sequence、role、start_price_amount、increment_amount、duration_seconds、extension_seconds、reason。

- [ ] **Step 4: 实现 Model Adapter**

FakeModel 严格按预置队列返回，便于确定性测试。OpenAI-compatible Adapter 使用 `AsyncOpenAI(base_url=..., api_key=...)` 的 Chat Completions JSON 模式，将内容交给 `AgentDecision.model_validate_json`；超时映射为 `ModelTimeoutError`，非法 JSON 映射为 `ModelOutputError`，不返回半结构化字典。

- [ ] **Step 5: 实现 Tool Registry 和 gRPC Client**

注册表只包含三项只读工具，并为每项定义 Pydantic 参数 Schema、10 秒 Deadline 和 `risk="read_only"`。Client metadata 只发送内部服务 Token；RequestContext 中发送 run/request/trace，tenant/operator 仅作观测字段，Go 仍从数据库恢复权威身份。

- [ ] **Step 6: 运行模型与工具测试**

Run: `cd live-auction-agent-service; python -m pytest tests/models tests/tools -v`

Expected: PASS。

- [ ] **Step 7: 提交模型与工具层**

```bash
git add live-auction-agent-service/app/domain live-auction-agent-service/app/models live-auction-agent-service/app/tools live-auction-agent-service/tests/models live-auction-agent-service/tests/tools
git commit -m "feat(agent): add structured model and tool adapters"
```

---

### Task 7: 实现 LangGraph Harness、Checkpoint 与 Runtime gRPC

**Files:**
- Create: `live-auction-agent-service/app/harness/context.py`
- Create: `live-auction-agent-service/app/harness/harness.py`
- Create: `live-auction-agent-service/app/graph/state.py`
- Create: `live-auction-agent-service/app/graph/workflow.py`
- Create: `live-auction-agent-service/app/runtime/repository.py`
- Create: `live-auction-agent-service/app/runtime/service.py`
- Create: `live-auction-agent-service/app/observability/tracing.py`
- Create: `live-auction-agent-service/app/main.py`
- Create: `live-auction-agent-service/tests/harness/test_planning_run.py`
- Create: `live-auction-agent-service/tests/runtime/test_service.py`
- Create: `live-auction-agent-service/tests/observability/test_tracing.py`

**Interfaces:**
- Produces: `AuctionAgentHarness.start(run: RunInput) -> AgentRunSummary`。
- Produces: `AgentRuntimeService` implementation for StartRun/GetRun/CancelRun/StreamRunEvents。
- Persists: `agent_runtime_runs` and `agent_runtime_events`; uses run ID as LangGraph `thread_id`.
- Produces OpenTelemetry spans for `agent.run`, `agent.model`, and `agent.tool`，并从 gRPC metadata 继续 Go 生成的 trace context。

- [ ] **Step 1: 写完整 FakeModel 工具循环测试**

```python
async def test_harness_collects_evidence_then_completes(fake_tool_client, skill_registry):
    model = FakeModel([
        ToolCallDecision(
            kind="tool_call", tool="inventory.query", arguments={"limit": 50}
        ),
        ToolCallDecision(
            kind="tool_call", tool="auction.history.query", arguments={"limit": 50}
        ),
        FinalProposalDecision(
            kind="final_proposal",
            proposal=sample_proposal(),  # explicitly includes evidence and risks
        ),
    ])
    harness = AuctionAgentHarness(model, ToolRegistry(fake_tool_client), skill_registry, memory_checkpointer())
    result = await harness.start(sample_run_input())
    assert result.status == RunStatus.COMPLETED
    assert [call.name for call in fake_tool_client.calls] == ["inventory.query", "auction.history.query"]
    assert result.proposal.lots[0].lot_id == "lot-owned-by-tenant"
```

另写最大步数测试：第 `max_steps` 次仍返回 ToolCall 时进入 FAILED，error_code 为 `STEP_BUDGET_EXHAUSTED`。

- [ ] **Step 2: 运行 Harness 测试确认失败**

Run: `cd live-auction-agent-service; python -m pytest tests/harness/test_planning_run.py -v`

Expected: FAIL，Harness 尚不存在。

- [ ] **Step 3: 实现 LangGraph 状态与节点**

`AgentState` 使用 TypedDict，字段固定为 run、skill_name、step_count、observations、decision、proposal、status、error。图包含 `load_skill → decide → execute_tool → decide` 循环和 `finalize`；每次循环递增 step_count，达到预算后走 `fail`。

`finalize` 必须校验 proposal 中每个 lot_id 都存在于 inventory observation，否则返回 `UNGROUNDED_PROPOSAL`。

- [ ] **Step 4: 实现 PostgreSQL Runtime Repository 与 Checkpointer**

服务启动时创建 `agent_runtime_runs`、`agent_runtime_events` 表并执行 LangGraph PostgresSaver setup。事件 seq 通过 Run 行上的持久化计数器在同一事务内加锁分配；不得使用无锁的 `MAX(seq) + 1`。`(run_id, seq)` 唯一约束防止重放。

- [ ] **Step 5: 实现异步 gRPC Runtime Service**

`StartRun` 持久化 CREATED 后用受控 asyncio Task 启动 Harness，立即返回 Run。`StreamRunEvents` 先从 PostgreSQL replay `seq > after_seq`，再订阅内存广播队列；客户端断开只取消订阅，不取消 Run。`CancelRun` 设置取消标记，Harness 在每个模型或工具边界检查。

Runtime 表是 Python 执行状态和事件流的持久化镜像；Go 的 `agent_runs` 始终是 tenant/operator 所有权与 HTTP 授权的权威记录。两侧共享同一个 Go 生成的 run ID。

- [ ] **Step 6: 接入最小跨服务 Trace**

从 `traceparent` gRPC metadata 提取上下文；Harness 根跨度命名为 `agent.run`，模型与工具调用分别创建子跨度。跨度属性只记录 run ID、skill、tool、status、latency 和 token 计数，不记录 Prompt、API Key、内部 Token 或完整工具结果。使用内存 exporter 测试父子关系和敏感字段过滤。

- [ ] **Step 7: 运行 Harness、Runtime 与 Trace 测试**

Run: `cd live-auction-agent-service; python -m pytest tests/harness tests/runtime tests/observability -v`

Expected: PASS。

- [ ] **Step 8: 运行 Python 全量测试**

Run: `cd live-auction-agent-service; python -m pytest -q`

Expected: PASS，0 failures。

- [ ] **Step 9: 提交 Harness Runtime**

```bash
git add live-auction-agent-service/app live-auction-agent-service/tests
git commit -m "feat(agent): add durable planning harness"
```

---

### Task 8: 实现 Go Runtime Gateway 与 SSE

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/service.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/runtime_client.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/service_test.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/server/agent_http.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/server/agent_http_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/http.go`
- Modify: `live-auction-bid-backend/app/auction/service/cmd/server/main.go`

**Interfaces:**
- Produces HTTP: `POST /api/admin/agent/runs`。
- Produces HTTP: `GET /api/admin/agent/runs/{runId}`。
- Produces SSE: `GET /api/admin/agent/runs/{runId}/events?after_seq=N`。
- Requires permission: `lot.view_admin`。

- [ ] **Step 1: 写身份注入、跨租户拒绝和 SSE replay 测试**

```go
func TestCreateAgentRunIgnoresClientTenant(t *testing.T) {
    fixture := newAgentHTTPFixture(t, claimsFor("user-a", "merchant-a"))
    response := fixture.Handler.PostJSON("/api/admin/agent/runs", `{"goal":"清理库存","tenantId":"merchant-b"}`)
    require.Equal(t, http.StatusOK, response.Code)
    require.Equal(t, "merchant-a", fixture.RuntimeStart.Context.TenantId)
}
```

GET/SSE 必须先查询本地 `agent_runs` 并验证 tenant；其他 tenant 的 run 返回统一 permission denied envelope，不连接 Python。

- [ ] **Step 2: 运行定向测试确认失败**

Run: `cd live-auction-bid-backend; go test ./app/auction/service/internal/agentcontrol ./app/auction/service/internal/server -run Agent -count=1 -v`

Expected: FAIL，Gateway 尚不存在。

- [ ] **Step 3: 实现 Runtime Client 和 Agent Control Service**

创建 Run 时由 Go 生成雪花/UUID 风格 run ID、request ID 与 trace ID，从 JWT Claims 计算 tenant/operator，先写本地 Run，再调用 Python `StartRun`。Python 不可用时将 Run 更新为 FAILED/`AGENT_RUNTIME_UNAVAILABLE`，HTTP 仍返回可查询的 Run。

- [ ] **Step 4: 实现 HTTP 与 fetch-compatible SSE**

SSE 响应格式：

```text
id: 12
event: agent.run.event
data: {"runId":"run-1","seq":12,"type":"tool.completed","message":"库存查询完成","payload":{}}

```

每 15 秒发送 `: heartbeat\n\n`；客户端断开时取消 gRPC stream context。不得在 SSE payload 中输出 Prompt、API Key 或完整工具认证 metadata。

- [ ] **Step 5: 运行 Gateway 测试和 Go 全量测试**

Run: `cd live-auction-bid-backend; go test ./app/auction/service/internal/agentcontrol ./app/auction/service/internal/server -run Agent -count=1 -v`

Run: `cd live-auction-bid-backend; go test ./...`

Expected: 全部 PASS。

- [ ] **Step 6: 提交 Gateway**

```bash
git add live-auction-bid-backend
git commit -m "feat(agent): add run gateway and event stream"
```

---

### Task 9: 增加商家 Agent 控制台

**Files:**
- Create: `live-auction-bid-frontend/src/features/agent-console/model/types.ts`
- Create: `live-auction-bid-frontend/src/features/agent-console/api/agentApi.ts`
- Create: `live-auction-bid-frontend/src/features/agent-console/AgentConsolePage.tsx`
- Create: `live-auction-bid-frontend/src/features/agent-console/agent-console.css`
- Create: `live-auction-bid-frontend/src/features/agent-console/AgentConsolePage.test.tsx`
- Create: `live-auction-bid-frontend/src/test/setup.ts`
- Create: `live-auction-bid-frontend/vitest.config.ts`
- Modify: `live-auction-bid-frontend/src/pages/host-console/HostConsolePage.tsx`
- Modify: `live-auction-bid-frontend/src/shared/api/types.ts`
- Modify: `live-auction-bid-frontend/package.json`
- Modify: `live-auction-bid-frontend/package-lock.json`

**Interfaces:**
- Consumes: `/api/admin/agent/runs` and `/events` SSE from Task 8。
- Produces route: `/admin/agent` and alias `/host/agent`。

- [ ] **Step 1: 写目标提交与流式轨迹 UI 测试**

```tsx
it('submits a goal and renders streamed proposal completion', async () => {
  render(<AgentConsolePage api={fakeAgentApi()} />);
  await userEvent.type(screen.getByLabelText('直播目标'), '清理积压玉石库存');
  await userEvent.click(screen.getByRole('button', { name: '生成方案' }));
  expect(await screen.findByText('库存查询完成')).toBeInTheDocument();
  expect(await screen.findByText('均衡清仓方案')).toBeInTheDocument();
});
```

- [ ] **Step 2: 运行测试确认页面不存在**

Run: `cd live-auction-bid-frontend; npm.cmd test -- AgentConsolePage`

Expected: FAIL，页面模块不存在。先添加 `test: "vitest run"` script，以及固定 devDependencies：`vitest@4.1.11`、`jsdom@30.0.1`、`@testing-library/react@16.3.2`、`@testing-library/user-event@14.6.6`、`@testing-library/jest-dom@7.0.1`；`vitest.config.ts` 使用 jsdom，`src/test/setup.ts` 导入 jest-dom matcher。重复运行直到得到预期的模块缺失失败。

- [ ] **Step 3: 实现带 Authorization 的 fetch SSE Client**

不使用原生 `EventSource`，因为它无法设置 Bearer Header。使用 `fetch` + `ReadableStreamDefaultReader` 解析 `id/event/data` 帧；组件卸载或创建新 Run 时调用 AbortController。

- [ ] **Step 4: 实现控制台页面**

页面包含目标输入、推荐示例、Run 状态、步骤时间线、工具调用摘要、风险、三段式方案和失败重试提示。第一阶段按钮文案固定为“生成方案”，不得出现“执行方案”或暗示已经修改业务数据的文案。

- [ ] **Step 5: 接入 Host Console 导航与权限**

导航项“AI 运营 Agent”使用现有 `lot.view_admin` 权限；路由匹配 `/admin/agent` 和 `/host/agent`，复用现有 Studio primitives 与 token，不修改冻结的 Home 页面。

- [ ] **Step 6: 运行前端测试与构建**

Run: `cd live-auction-bid-frontend; npm.cmd test -- AgentConsolePage`

Run: `cd live-auction-bid-frontend; npm.cmd run build`

Expected: 测试与构建 PASS。

- [ ] **Step 7: 提交 Agent 控制台**

```bash
git add live-auction-bid-frontend
git commit -m "feat(agent): add merchant planning console"
```

---

### Task 10: Docker Compose 集成与端到端验证

**Files:**
- Modify: `live-auction-bid-backend/deploy/docker-compose.yml`
- Modify: `live-auction-bid-backend/deploy/.env.example`
- Modify: `README.md`
- Modify: `live-auction-bid-backend/README.md`
- Create: `live-auction-agent-service/README.md`
- Create: `live-auction-agent-service/tests/e2e/test_readonly_planning.py`

**Interfaces:**
- Produces Compose profile: `agent`。
- Produces health command for `auction-agent` gRPC Server。
- Produces documented startup: `docker compose --profile agent up -d --build`。

- [ ] **Step 1: 写 E2E 测试，先确认 Agent profile 尚不可用**

```python
def test_readonly_planning_completes(agent_client, seeded_merchant_run):
    run = agent_client.start(goal="用现有待拍拍品生成一场30分钟均衡场")
    completed = agent_client.wait(run.id, timeout=30)
    assert completed.status == "RUN_STATUS_COMPLETED"
    proposal = json.loads(completed.proposal_json)
    assert proposal["lots"]
    assert all(item["lotId"] in seeded_merchant_run.owned_lot_ids for item in proposal["lots"])
```

- [ ] **Step 2: 配置 agent Compose profile**

新增 `auction-agent`，依赖 backend healthy 与 pgvector healthy；pgvector profiles 改为 `["search", "agent"]`。共享环境变量：

```env
AUCTION_AGENT_RUNTIME_ADDR=auction-agent:19081
AUCTION_AGENT_TOOL_GRPC_ADDR=:19082
AUCTION_TOOL_GRPC_ADDR=auction-backend:19082
AUCTION_INTERNAL_SERVICE_TOKEN=local-agent-service-token-change-me
AGENT_DATABASE_DSN=postgresql://auction_search:auction_search_dev@pgvector:5432/live_auction_search
AGENT_MODEL_PROVIDER=mock
```

Agent profile 停止或不存在时，backend `/readyz` 仍返回成功；只有创建 Agent Run 才返回稳定的 Runtime unavailable 错误。

- [ ] **Step 3: 启动 profile 并运行健康检查**

Run: `cd live-auction-bid-backend/deploy; docker compose --profile agent up -d --build`

Run: `docker compose ps`

Expected: mysql、redis、rabbitmq、consul、auction-backend、pgvector、auction-agent healthy。

- [ ] **Step 4: 运行端到端只读规划测试**

Run: `cd live-auction-agent-service; python -m pytest tests/e2e/test_readonly_planning.py -v`

Expected: PASS；方案内所有 lot ID 均属于当前商家，没有业务写入。

- [ ] **Step 5: 运行全量回归**

Run: `cd live-auction-bid-backend; go test ./...`

Run: `cd live-auction-agent-service; python -m pytest -q`

Run: `cd live-auction-bid-frontend; npm.cmd run build`

Run: `cd live-auction-user-h5; npm.cmd run build`

Expected: 全部退出 0。

- [ ] **Step 6: 验证故障隔离**

Run: `docker compose stop auction-agent`

Run: `Invoke-WebRequest http://127.0.0.1:18080/readyz`

Expected: backend ready 返回 `{"ok":true}`；普通拍品列表与竞价主链路可用，创建 Agent Run 返回 `AGENT_RUNTIME_UNAVAILABLE`。

- [ ] **Step 7: 更新文档并提交第一阶段**

README 必须说明架构、端口、环境变量、mock 默认行为、启动/停止命令、故障隔离和当前只读边界。

```bash
git add README.md live-auction-bid-backend live-auction-agent-service
git commit -m "docs(agent): document readonly planning workflow"
```

---

## 第一阶段完成检查

- [ ] `POST /api/admin/agent/runs` 只能从 JWT Claims 推导 tenant/operator。
- [ ] Python 只能通过三个只读 gRPC Tool 获取业务数据。
- [ ] Tool Server 以 Go 数据库内 Agent Run 所有权为准。
- [ ] Skill 按元数据发现、选中后加载，并限制允许工具。
- [ ] FakeModel 能确定性覆盖完整 tool loop。
- [ ] 实际模型输出经过 Pydantic 校验且不从任意文本截取 JSON。
- [ ] Run 和事件能从 PostgreSQL replay，SSE 支持 `after_seq`。
- [ ] Go 生成的 trace context 能贯穿 Python `agent.run → agent.model/agent.tool` spans，且不记录敏感内容。
- [ ] 方案中不存在工具证据以外的 lot ID。
- [ ] 停止 Python Agent 不影响 Go 主业务 readiness。
- [ ] Go、Python、商家端和 H5 的回归命令全部通过。
