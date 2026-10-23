# AI 竞拍 Agent 对话补全与审批入队 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在不赋予 Python 写权限、且最终确认前不创建真实拍品的前提下，实现可恢复的 Agent 对话、最少追问、图片绑定、可编辑批量确认，以及 Go 单事务追加队列。

**Architecture:** React 负责对话、草稿编辑、上传和确认交互；Go 是会话、候选项、快照和审批执行的权威边界；Python 只做意图识别、只读查询和结构化建议。一次用户消息对应一个异步 Run；一次最终确认对应一个不可变快照和一个幂等事务。

**Tech Stack:** Go 1.24、Kratos HTTP/gRPC、GORM/MySQL、Redis Stream、Protocol Buffers、Python 3.12、Pydantic 2、LangGraph、OpenAI-compatible API、React 19、TypeScript、Vitest。

**Spec:** `docs/superpowers/specs/2026-08-28-agent-conversation-approval-design.md`

## Global Constraints

- 按用户要求直接在当前 checkout 实施，不创建 Git worktree。
- 严格 TDD：每个行为先写失败测试，确认失败原因正确，再写最小实现。
- Python Tool Registry 始终只包含 `inventory.query`、`history.query`、`session.snapshot` 等只读工具；不得新增创建拍品、编辑拍品、入队或开拍工具。
- `agent_draft_candidates` 不是业务拍品表；只有 `ApplyAgentProposal` 的最终事务可以创建新拍品。
- 名称、起拍价、时长和主图必须由用户明确提供；模型不得用默认值使缺失项变为可确认。
- 浏览器金额使用元，HTTP/gRPC/数据库金额统一使用人民币分；自然语言“价格 10000”解析为 `1_000_000` 分。
- 批量确认必须全有或全无，追加到现有队尾，不自动调用 `StartLot`。
- 每个任务完成后运行其局部测试并提交；阶段结束运行全量测试。

---

## Phase 1：共享协议、持久会话与结构化草稿

### Task 1: 扩展 Agent gRPC 契约并保持旧 Run 兼容

**Files:**

- Modify: `proto/agent/v1/runtime.proto`
- Modify: `proto/agent/v1/common.proto`
- Modify: `live-auction-bid-backend/api/agent/v1/runtime.pb.go`
- Modify: `live-auction-bid-backend/api/agent/v1/runtime_grpc.pb.go`
- Modify: `live-auction-agent-service/app/generated/proto/agent/v1/runtime_pb2.py`
- Modify: `live-auction-agent-service/app/generated/proto/agent/v1/runtime_pb2_grpc.py`
- Test: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/runtime_client_test.go`
- Test: `live-auction-agent-service/tests/runtime/test_service.py`

**Interfaces:**

```proto
enum RunType {
  RUN_TYPE_UNSPECIFIED = 0;
  RUN_TYPE_SESSION_PLAN = 1;
  RUN_TYPE_LOT_DRAFT_ASSIST = 2;
}

message StartRunRequest {
  string run_id = 1;
  RequestContext context = 2;
  string goal = 3; // retained for old callers
  RunType run_type = 4;
  string session_id = 5;
  int64 session_version = 6;
  string turn_input_json = 7;
}

message AgentRun {
  // fields 1-8 remain unchanged
  RunType run_type = 9;
  string session_id = 10;
  int64 session_version = 11;
  string turn_result_json = 12;
}
```

- [ ] **Step 1: 写 Go 与 Python 失败测试**

  断言 `SESSION_PLAN`、`LOT_DRAFT_ASSIST`、`session_id`、`session_version` 和 `turn_input_json` 能完整往返；旧请求只传 `goal` 时仍能启动现有只读规划。

- [ ] **Step 2: 运行测试并确认因字段不存在而失败**

  Run: `go test ./app/auction/service/internal/agentcontrol -run 'TestAgentRuntimeClient.*(Session|Legacy)'`

  Run: `python -m pytest tests/runtime/test_service.py -k 'turn_contract or legacy_goal' -q`

- [ ] **Step 3: 只追加 proto 字段并重新生成代码**

  Run: `powershell -ExecutionPolicy Bypass -File .\scripts\generate-agent-proto.ps1`

  不修改或复用现有字段编号；`goal` 与 `proposal_json` 保留为向后兼容字段。

- [ ] **Step 4: 更新 Go/Python 映射并运行测试**

  Run: `go test ./app/auction/service/internal/agentcontrol`

  Run: `python -m pytest tests/runtime/test_service.py -q`

- [ ] **Step 5: 提交**

  Run: `git add proto/agent/v1 live-auction-bid-backend/api/agent/v1 live-auction-agent-service/app/generated live-auction-bid-backend/app/auction/service/internal/agentcontrol live-auction-agent-service/tests/runtime/test_service.py && git commit -m "feat(agent): add session turn protocol"`

### Task 2: 定义 Go 会话领域模型、状态机和确定性校验

**Files:**

- Create: `live-auction-bid-backend/app/auction/service/internal/agentconversation/model.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentconversation/validation.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentconversation/validation_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/pkg/apperr/errors.go`

**Interfaces:**

```go
type SessionStatus string
const (
    StatusCollectingInput SessionStatus = "COLLECTING_INPUT"
    StatusReadyForReview SessionStatus = "READY_FOR_REVIEW"
    StatusApplying SessionStatus = "APPLYING"
    StatusApplied SessionStatus = "APPLIED"
    StatusNeedsRevision SessionStatus = "NEEDS_REVISION"
    StatusCancelled SessionStatus = "CANCELLED"
)

type SourceType string
const (SourceExisting SourceType = "EXISTING"; SourceNew SourceType = "NEW")

type RequiredInput string
const (
    RequiredTitle RequiredInput = "TITLE"
    RequiredStartPrice RequiredInput = "START_PRICE"
    RequiredDuration RequiredInput = "DURATION"
    RequiredMainImage RequiredInput = "MAIN_IMAGE"
    RequiredAmbiguousCandidate RequiredInput = "AMBIGUOUS_CANDIDATE"
)

type CandidateDraft struct {
    Title string `json:"title"`
    StartPriceAmount int64 `json:"startPriceAmount"`
    DurationSeconds int32 `json:"durationSeconds"`
    MainImageAssetID string `json:"mainImageAssetId"`
    MainImageURL string `json:"mainImageUrl"`
    MinIncrementAmount int64 `json:"minIncrementAmount"`
    CapPriceAmount *int64 `json:"capPriceAmount,omitempty"`
    Category, Description string
    Tags []string
    Stock int32
    AntiSnipeWindowSeconds, AntiSnipeExtendSeconds, MaxExtendCount int32
    DepositAmount int64
}
```

- [ ] **Step 1: 写状态机、必填项、金额与竞拍规则失败测试**

  覆盖：新候选缺名称/起拍价/时长/主图时分别返回稳定缺失项；现有候选必须包含 `existing_lot_id` 与读取版本；CNY 金额非负、最低加价大于 0、时长至少 60 秒、封顶价大于起拍价。

- [ ] **Step 2: 运行测试并确认包尚不存在**

  Run: `go test ./app/auction/service/internal/agentconversation`

- [ ] **Step 3: 实现纯领域模型和校验器**

  提供 `MissingRequiredInputs(candidate Candidate) []RequiredInput`、`ValidateCandidate(candidate Candidate) error`、`CanTransition(from, to SessionStatus) bool`。错误使用新增 sentinel：`ErrAgentSessionVersionConflict`、`ErrAgentRequiredInputMissing`、`ErrAgentImageAssetInvalid`、`ErrAgentReviewSnapshotStale`、`ErrAgentCandidateChanged`、`ErrAgentBatchApplyFailed`、`ErrAgentBatchAlreadyApplied`。

- [ ] **Step 4: 运行局部测试和 Go 全包编译**

  Run: `go test ./app/auction/service/internal/agentconversation ./app/auction/service/internal/pkg/apperr`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/agentconversation live-auction-bid-backend/app/auction/service/internal/pkg/apperr/errors.go && git commit -m "feat(agent): define conversation domain"`

### Task 3: 持久化会话、消息、候选项并关联 Run

**Files:**

- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_conversation_models.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_conversation_repo.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_conversation_repo_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/agent_run_models.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/store.go`
- Modify: `live-auction-bid-backend/deploy/mysql/schema.sql`
- Create: `live-auction-bid-backend/deploy/mysql/migrations/20260828_agent_conversation.sql`

**Interfaces:**

```go
type Repository interface {
    CreateSession(ctx context.Context, session Session) error
    FindSession(ctx context.Context, tenantID, sessionID string) (SessionAggregate, error)
    AppendUserMessageAndStartRun(ctx context.Context, command StartTurnCommand) (SessionAggregate, error)
    MergeTurnResult(ctx context.Context, command MergeTurnCommand) (SessionAggregate, error)
    PatchCandidate(ctx context.Context, command PatchCandidateCommand) (SessionAggregate, error)
    CancelSession(ctx context.Context, tenantID, operatorID, sessionID string, expectedVersion int64) error
}
```

- [ ] **Step 1: 写 SQLite 仓储失败测试**

  覆盖租户隔离、消息 `session_id + seq` 唯一、候选 `session_id + position` 唯一、会话版本 CAS、Run 持有 `session_id/run_type/session_version`、刷新后完整恢复聚合。

- [ ] **Step 2: 运行测试并确认缺表/缺实现失败**

  Run: `go test ./app/auction/service/internal/data -run 'TestAgentConversation'`

- [ ] **Step 3: 增加模型、AutoMigrate、旧库 migration 与仓储事务**

  `agent_sessions`、`agent_session_messages`、`agent_draft_candidates` 使用设计文档字段。`agent_runs.session_id` 可空字符串以兼容历史 Run；索引为 `(tenant_id, updated_at_unix_ms)`、`(session_id, seq)`、`(session_id, position)`。

- [ ] **Step 4: 运行仓储测试并校验 migration 可重复执行**

  Run: `go test ./app/auction/service/internal/data -run 'TestAgentConversation|TestAgentRun'`

  Run: `Get-Content -Raw .\mysql\migrations\20260828_agent_conversation.sql | docker compose exec -T mysql mysql -uroot -pauction_root live_auction`（目录：`live-auction-bid-backend/deploy`）

  Expected: 第二次执行不破坏已有行与索引。

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/data live-auction-bid-backend/deploy/mysql && git commit -m "feat(agent): persist conversation drafts"`

### Task 4: 实现 Python 新拍品解析、最少追问和用户字段保护

**Files:**

- Modify: `live-auction-agent-service/app/domain/models.py`
- Modify: `live-auction-agent-service/app/harness/context.py`
- Modify: `live-auction-agent-service/app/harness/harness.py`
- Modify: `live-auction-agent-service/app/models/openai_compat.py`
- Modify: `live-auction-agent-service/app/models/fake.py`
- Create: `live-auction-agent-service/tests/harness/test_lot_draft_assist.py`
- Modify: `live-auction-agent-service/tests/models/test_model_adapter.py`
- Modify: `live-auction-agent-service/tests/tools/test_registry.py`

**Interfaces:**

```python
class CandidatePatch(BaseModel):
    model_config = ConfigDict(extra="forbid")
    candidate_id: str
    source_type: Literal["EXISTING", "NEW"]
    fields: dict[str, JsonValue]
    field_sources: dict[str, Literal["USER", "AI", "SYSTEM", "EXISTING"]]

class TurnResult(BaseModel):
    model_config = ConfigDict(extra="forbid")
    assistant_message: str
    candidate_patches: list[CandidatePatch]
    required_inputs: list[RequiredInput]
    review_readiness: Literal["NEEDS_INPUT", "READY_FOR_REVIEW"]
    warnings: list[str]
```

- [ ] **Step 1: 写固定语料失败测试**

  至少覆盖：

  - “海洋之心，价格10000，时长50分钟，封顶100000”输出起拍价 `1_000_000` 分、时长 `3000` 秒、封顶 `10_000_000` 分，仅缺 `MAIN_IMAGE`。
  - 缺价格时包含 `START_PRICE`；缺时长时包含 `DURATION`。
  - 模型对 `USER` 来源字段给出不同值时转入 `warnings`，不能覆盖字段。
  - 只读工具注册表中不存在任何 create/update/queue/start 工具。

- [ ] **Step 2: 运行测试并确认结构化类型不存在而失败**

  Run: `python -m pytest tests/harness/test_lot_draft_assist.py tests/models/test_model_adapter.py tests/tools/test_registry.py -q`

- [ ] **Step 3: 实现严格 Pydantic Schema 与 harness 分支**

  Prompt 明确四个用户必填项和人民币元到分的换算；`LOT_DRAFT_ASSIST` 使用 `TurnResult` schema，`SESSION_PLAN` 兼容旧 `SessionProposal` 并适配为候选补丁。合并函数只允许 AI 写入空字段或 AI/SYSTEM 来源字段。

- [ ] **Step 4: 运行 Python 局部测试**

  Run: `python -m pytest tests/harness tests/models tests/tools -q`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-agent-service/app live-auction-agent-service/tests && git commit -m "feat(agent): add lot draft assistance"`

### Task 5: 将会话 Turn 接入 Python Runtime 与 Go Run Gateway

**Files:**

- Modify: `live-auction-agent-service/app/runtime/service.py`
- Modify: `live-auction-agent-service/app/runtime/repository.py`
- Modify: `live-auction-agent-service/tests/runtime/test_service.py`
- Modify: `live-auction-agent-service/tests/runtime/test_postgres_repository.py`
- Modify: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/model.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/service.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/runtime_client.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/agentcontrol/service_test.go`

**Interfaces:**

```go
type CreateSessionRunCommand struct {
    SessionID string
    SessionVersion int64
    RunType RunType
    TurnInputJSON string
    IdempotencyKey string
}
```

- [ ] **Step 1: 写失败测试**

  断言 Run 在调用 Python 前已与会话版本一起持久化；Runtime 完成后 `turn_result_json` 可投影；当前 Turn 失败不删除消息/候选；同一幂等键只启动一次 Runtime。

- [ ] **Step 2: 运行测试并确认旧 Gateway 无法传递 Turn**

  Run: `go test ./app/auction/service/internal/agentcontrol -run 'TestAgentService.*Session'`

  Run: `python -m pytest tests/runtime/test_service.py tests/runtime/test_postgres_repository.py -k 'session or turn' -q`

- [ ] **Step 3: 扩展 Runtime 持久层和投影**

  Python Runtime 保存 `run_type/session_id/session_version/turn_input_json/turn_result_json`；Go 仍是租户与操作者授权权威。旧 `proposal_json` 继续支持现有控制台和旧测试。

- [ ] **Step 4: 运行 Runtime/Gateway 全部测试**

  Run: `go test ./app/auction/service/internal/agentcontrol`

  Run: `python -m pytest tests/runtime -q`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-agent-service/app/runtime live-auction-agent-service/tests/runtime live-auction-bid-backend/app/auction/service/internal/agentcontrol && git commit -m "feat(agent): execute persistent session turns"`

### Task 6: 实现 Go 会话服务的消息提交、结果合并与恢复 API

**Files:**

- Create: `live-auction-bid-backend/app/auction/service/internal/agentconversation/service.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentconversation/service_test.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/http.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/registry.go`

**Interfaces:**

```text
POST /api/admin/agent/sessions
  { roomId } -> { session }
GET /api/admin/agent/sessions/{id}
  -> { session, messages, candidates, activeRun }
POST /api/admin/agent/sessions/{id}/messages
  Idempotency-Key + { expectedVersion, content, runType }
  -> { session, run }
GET /api/admin/agent/sessions/{id}/events?after_seq=N
POST /api/admin/agent/sessions/{id}/cancel
  { expectedVersion }
```

- [ ] **Step 1: 写服务与 HTTP 失败测试**

  覆盖 JWT/RBAC、主账户隔离、房间归属、请求体上限、严格幂等键、版本冲突、Run 完成后事务性合并、刷新恢复、SSE `after_seq` 连续性以及安全错误消息。

- [ ] **Step 2: 运行测试并确认路由 404/服务缺失**

  Run: `go test ./app/auction/service/internal/agentconversation ./app/auction/service/internal/server -run 'TestAgentSession'`

- [ ] **Step 3: 实现服务和 HTTP 路由**

  创建会话、发消息、编辑候选和只读恢复均要求现有 `lot.view_admin`；图片上传另要求 `upload.image`。不要信任 body 中的 tenant/operator。模型结果先通过 `ValidateCandidate`，再按字段来源规则合并并递增会话版本。

- [ ] **Step 4: 运行 Go 测试**

  Run: `go test ./app/auction/service/internal/agentconversation ./app/auction/service/internal/server ./app/auction/service/internal/agentcontrol`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/agentconversation live-auction-bid-backend/app/auction/service/internal/server && git commit -m "feat(agent): expose recoverable sessions"`

---

## Phase 2：候选编辑、图片绑定与不可变快照

### Task 7: 候选项结构化编辑与字段来源保护

**Files:**

- Modify: `live-auction-bid-backend/app/auction/service/internal/agentconversation/service.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/agentconversation/service_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http_test.go`

**Interfaces:**

```text
PATCH /api/admin/agent/sessions/{sessionId}/candidates/{candidateId}
If-Match: <candidate-version>
{ expectedSessionVersion, fields: { ... } }
```

- [ ] **Step 1: 写失败测试**

  覆盖允许字段白名单、未知字段拒绝、元到分不在后端重复换算、用户编辑将来源标记为 `USER`、旧候选版本冲突、修改后旧 active snapshot 失效、必填齐全才进入 `READY_FOR_REVIEW`。

- [ ] **Step 2: 运行失败测试**

  Run: `go test ./app/auction/service/internal/agentconversation ./app/auction/service/internal/server -run 'Test.*PatchCandidate'`

- [ ] **Step 3: 实现 JSON Merge Patch 白名单与双重 CAS**

  同时校验 `session.version` 和 `candidate.version`；在同一事务中更新候选、重新校验聚合、递增会话版本并清除 `active_snapshot_version`。

- [ ] **Step 4: 运行局部测试**

  Run: `go test ./app/auction/service/internal/agentconversation ./app/auction/service/internal/server`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/agentconversation live-auction-bid-backend/app/auction/service/internal/server && git commit -m "feat(agent): edit structured candidates"`

### Task 8: 将临时图片资产安全绑定到候选项

**Files:**

- Modify: `live-auction-bid-backend/app/auction/service/internal/data/models.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/asset_repo.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http_test.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/server/upload_agent_candidate_test.go`
- Modify: `live-auction-bid-backend/deploy/mysql/schema.sql`
- Modify: `live-auction-bid-backend/deploy/mysql/migrations/20260828_agent_conversation.sql`

**Interfaces:**

```text
POST /api/admin/agent/sessions/{sessionId}/candidates/{candidateId}/images
{ expectedSessionVersion, expectedCandidateVersion, assetId }
```

- [ ] **Step 1: 写失败测试**

  覆盖资产必须是 `TEMPORARY`、`biz_type=agent_lot_image`、属于当前操作者/主账户、未过期且未绑定其他候选；替换图片只解除旧候选引用，不删除仍被引用资产；跨租户返回统一权限错误。

- [ ] **Step 2: 运行失败测试**

  Run: `go test ./app/auction/service/internal/server ./app/auction/service/internal/data -run 'TestAgentCandidateImage|TestUpload'`

- [ ] **Step 3: 增加候选资产引用并实现绑定事务**

  在 `asset_files` 增加可空的 `agent_session_id/agent_candidate_id`，或建立唯一引用表 `agent_candidate_assets`；选择引用表以避免改变现有拍品附件语义。确认前资产保持 `TEMPORARY`，但过期清理必须排除仍被活跃会话引用的资产。

- [ ] **Step 4: 运行上传与会话测试**

  Run: `go test ./app/auction/service/internal/server ./app/auction/service/internal/data`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/data live-auction-bid-backend/app/auction/service/internal/server live-auction-bid-backend/deploy/mysql && git commit -m "feat(agent): bind draft image assets"`

### Task 9: 生成内容哈希稳定的不可变确认快照

**Files:**

- Create: `live-auction-bid-backend/app/auction/service/internal/agentconversation/snapshot.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentconversation/snapshot_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/agent_conversation_models.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/agent_conversation_repo.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http.go`
- Modify: `live-auction-bid-backend/deploy/mysql/schema.sql`
- Modify: `live-auction-bid-backend/deploy/mysql/migrations/20260828_agent_conversation.sql`

**Interfaces:**

```text
POST /api/admin/agent/sessions/{sessionId}/review-snapshots
{ expectedSessionVersion, selectedCandidateIds, orderedCandidateIds }
-> { snapshotVersion, contentHash, candidates }
```

- [ ] **Step 1: 写失败测试**

  覆盖默认全选由前端提交、至少选择一项、selected 与 ordered 集合完全一致、候选无重复、批内排序稳定、已有拍品记录读取版本、JSON canonicalization 后 SHA-256 哈希可复现、快照不可更新。

- [ ] **Step 2: 运行失败测试**

  Run: `go test ./app/auction/service/internal/agentconversation ./app/auction/service/internal/data -run 'Test.*Snapshot'`

- [ ] **Step 3: 实现 canonical snapshot 与 append-only 仓储**

  `agent_approval_snapshots` 唯一键 `(session_id, snapshot_version)`；快照 JSON 包含候选最终字段、字段来源、选择、顺序、现有拍品版本、资产 ID 和操作者，不包含模型密钥或原始图片。

- [ ] **Step 4: 运行快照与 HTTP 测试**

  Run: `go test ./app/auction/service/internal/agentconversation ./app/auction/service/internal/data ./app/auction/service/internal/server`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/agentconversation live-auction-bid-backend/app/auction/service/internal/data live-auction-bid-backend/app/auction/service/internal/server live-auction-bid-backend/deploy/mysql && git commit -m "feat(agent): create immutable review snapshots"`

---

## Phase 3：Go 原子审批执行器

### Task 10: 定义批量应用命令和事务仓储端口

**Files:**

- Create: `live-auction-bid-backend/app/auction/service/internal/agentapproval/model.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentapproval/service.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/agentapproval/service_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/biz/auction/repo.go`

**Interfaces:**

```go
type ApplyCommand struct {
    TenantID, OperatorID, SessionID string
    SnapshotVersion int64
    ContentHash string
    SelectedCandidateIDs []string
    OrderedCandidateIDs []string
    IdempotencyKey string
}

type TransactionalRepository interface {
    Apply(ctx context.Context, command ApplyCommand) (Execution, error)
    FindExecutionByKey(ctx context.Context, sessionID string, snapshotVersion int64, key string) (Execution, error)
}
```

- [ ] **Step 1: 写服务级失败测试**

  覆盖权限身份不能为空、幂等键格式、集合/顺序与快照一致、同键同请求返回原结果、同键不同 payload 返回 conflict、已应用会话稳定返回既有执行结果。

- [ ] **Step 2: 运行失败测试**

  Run: `go test ./app/auction/service/internal/agentapproval`

- [ ] **Step 3: 实现薄服务层**

  服务只做命令验证、权限上下文和仓储调用；所有业务状态重检必须仍在仓储事务内执行，避免 TOCTOU。

- [ ] **Step 4: 运行服务测试**

  Run: `go test ./app/auction/service/internal/agentapproval`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/agentapproval live-auction-bid-backend/app/auction/service/internal/biz/auction/repo.go && git commit -m "feat(agent): define atomic approval command"`

### Task 11: 实现 MySQL 全有或全无创建与队尾追加

**Files:**

- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_approval_models.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_approval_repo.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_approval_repo_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/store.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/room_state_repo.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/data/lot_repo.go`
- Modify: `live-auction-bid-backend/deploy/mysql/schema.sql`
- Create: `live-auction-bid-backend/deploy/mysql/migrations/20260828_agent_approval.sql`

**Interfaces:**

```sql
UNIQUE KEY uk_agent_execution_idempotency
  (session_id, snapshot_version, idempotency_key)
```

- [ ] **Step 1: 写 MySQL 集成失败测试**

  在 disposable schema 中验证：

  - `SELECT ... FOR UPDATE` 锁定 session、snapshot、room_state、已有 lots 和 assets。
  - NEW 候选只在事务内创建真实 lot；EXISTING 候选版本/状态变化时整批失败。
  - 从 `room_state.next_queue_position` 分配连续位置，更新一次计数器，不改变原队列。
  - 第 N 项无效时 lot 数、queue position、asset status、execution、event 全部无变化。
  - 两个并发批次位置唯一，各批内部连续。
  - 同幂等键并发重放只生成一组拍品。

- [ ] **Step 2: 运行测试并确认仓储未实现**

  Run: `docker compose exec -T mysql mysql -uroot -pauction_root -e "CREATE DATABASE IF NOT EXISTS live_auction_agent_test CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_ai_ci"`（目录：`live-auction-bid-backend/deploy`）

  Run: `$env:AUCTION_AGENT_TEST_MYSQL_DSN='root:auction_root@tcp(127.0.0.1:13306)/live_auction_agent_test?parseTime=true&charset=utf8mb4&loc=Local'; go test ./app/auction/service/internal/data -run 'TestAgentApprovalMySQL'`（目录：`live-auction-bid-backend`）

- [ ] **Step 3: 实现单个 `db.Transaction`**

  事务顺序固定为：锁 execution key → 锁 session/snapshot → 锁 room state → 按 ID 排序锁现有 lot/asset → 校验全部候选 → 创建 NEW lot → 调用领域 `auction.QueueLot` → 更新 lots/assets/room state → 写 execution、auction event/outbox → session=`APPLIED`。任何错误直接返回让 GORM 回滚。

- [ ] **Step 4: 运行数据层测试与竞拍回归**

  Run: `go test ./app/auction/service/internal/data ./app/auction/service/internal/biz/auction`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal/data live-auction-bid-backend/deploy/mysql && git commit -m "feat(agent): atomically append approved lots"`

### Task 12: 将审计、Outbox 和浏览器 Apply API 接到执行器

**Files:**

- Modify: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/agent_session_http_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/server/registry.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/service/errors.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/observability/metrics.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/agent_approval_outbox_test.go`

**Interfaces:**

```text
POST /api/admin/agent/sessions/{sessionId}/apply
Idempotency-Key: <key>
{ snapshotVersion, contentHash, selectedCandidateIds, orderedCandidateIds }
-> { executionId, createdLotIds, queuedLots: [{ lotId, queuePosition }] }
```

- [ ] **Step 1: 写失败测试**

  覆盖 `lot.queue` 权限、包含 NEW 候选时额外要求 `lot.create`、跨租户、过期快照、候选变化、图片无效、整批失败、已应用幂等响应；验证响应不包含 SQL、Prompt、Provider 或密钥；验证没有任何路径调用 `StartLot`。

- [ ] **Step 2: 运行失败测试**

  Run: `go test ./app/auction/service/internal/server ./app/auction/service/internal/data -run 'TestAgentApply|TestAgentApprovalOutbox'`

- [ ] **Step 3: 注册服务、稳定错误码、Trace 和指标**

  增加 `proposal.apply.transaction` span；指标包含 apply 成功/回滚/幂等命中/快照冲突和确认到 Outbox 可见延迟。事件 payload 只含 session/execution/lot IDs、room ID、positions 和 trace ID。

- [ ] **Step 4: 运行 Go 全量测试**

  Run: `go test ./...`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-backend/app/auction/service/internal && git commit -m "feat(agent): expose audited proposal apply"`

---

## Phase 4：React 闭环、E2E 与作品集证据

### Task 13: 实现可恢复对话与候选草稿面板

**Files:**

- Modify: `live-auction-bid-frontend/src/features/agent-console/model/types.ts`
- Modify: `live-auction-bid-frontend/src/features/agent-console/api/agentApi.ts`
- Rewrite: `live-auction-bid-frontend/src/features/agent-console/AgentConsolePage.tsx`
- Modify: `live-auction-bid-frontend/src/features/agent-console/agent-console.css`
- Modify: `live-auction-bid-frontend/src/features/agent-console/AgentConsolePage.test.tsx`

**Interfaces:**

```ts
export interface AgentApi {
  createSession(roomId: string): Promise<AgentSession>;
  getSession(id: string): Promise<AgentSessionAggregate>;
  sendMessage(id: string, input: SendMessageInput): Promise<AgentRun>;
  patchCandidate(sessionId: string, candidateId: string, input: CandidatePatchInput): Promise<AgentSessionAggregate>;
  bindCandidateImage(sessionId: string, candidateId: string, input: BindImageInput): Promise<AgentSessionAggregate>;
  createReviewSnapshot(sessionId: string, input: ReviewInput): Promise<ReviewSnapshot>;
  apply(sessionId: string, input: ApplyInput): Promise<Execution>;
}
```

- [ ] **Step 1: 写 UI 失败测试**

  覆盖首次创建/已有 session 恢复、消息历史和 Run 状态、模型失败后草稿保留、缺起拍价/时长的内嵌输入、每候选图片上传、AI 建议徽标、编辑后来源变 USER、刷新使用 session ID 恢复。

- [ ] **Step 2: 运行失败测试**

  Run: `npm test -- AgentConsolePage.test.tsx`

- [ ] **Step 3: 实现对话与草稿双栏**

  URL 使用 `/admin/agent?session=<id>` 保存恢复定位；图片调用现有 `uploadImage(file, { roomId, bizType: 'agent_lot_image' })` 后再绑定 asset ID。金额输入组件只在 UI 做元↔分换算，提交始终是整数分。

- [ ] **Step 4: 运行测试和构建**

  Run: `npm test -- AgentConsolePage.test.tsx`

  Run: `npm run build`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-frontend/src/features/agent-console && git commit -m "feat(agent): build recoverable draft workspace"`

### Task 14: 实现默认全选、取消勾选、批内排序和确认跳转

**Files:**

- Create: `live-auction-bid-frontend/src/features/agent-console/AgentReviewPanel.tsx`
- Create: `live-auction-bid-frontend/src/features/agent-console/AgentReviewPanel.test.tsx`
- Modify: `live-auction-bid-frontend/src/features/agent-console/AgentConsolePage.tsx`
- Modify: `live-auction-bid-frontend/src/features/agent-console/agent-console.css`
- Modify: `live-auction-bid-frontend/src/pages/host-console/HostConsolePage.tsx`

**Interfaces:**

```ts
type AgentReviewPanelProps = {
  session: AgentSession;
  candidates: AgentCandidate[];
  activeSnapshot?: ReviewSnapshot;
  onCreateSnapshot(input: ReviewInput): Promise<ReviewSnapshot>;
  onApply(input: ApplyInput): Promise<Execution>;
};
```

- [ ] **Step 1: 写 UI 失败测试**

  覆盖默认全选、取消部分、至少一项、批内上移/下移、EXISTING/NEW 标签、风险与字段来源、快照变化后 Apply 禁用、双击确认只发一个幂等请求、按钮文案“确认并加入队列（N 件）”。

- [ ] **Step 2: 运行失败测试**

  Run: `npm test -- AgentReviewPanel.test.tsx AgentConsolePage.test.tsx`

- [ ] **Step 3: 实现确认交互和成功跳转**

  Apply 成功后使用现有导航方式跳转 `/admin/auctions`；仅展示已分配队列位置，不调用 `startLot`，由用户在队列页手动开拍。

- [ ] **Step 4: 运行前端全量测试和构建**

  Run: `npm test`

  Run: `npm run build`

- [ ] **Step 5: 提交**

  Run: `git add live-auction-bid-frontend/src/features/agent-console live-auction-bid-frontend/src/pages/host-console/HostConsolePage.tsx && git commit -m "feat(agent): confirm selected lots into queue"`

### Task 15: 增加跨服务 E2E、Eval、运行文档和最终验证

**Files:**

- Create: `live-auction-agent-service/tests/e2e/test_conversation_approval.py`
- Create: `live-auction-agent-service/tests/evals/lot_draft_cases.json`
- Create: `live-auction-agent-service/tests/evals/test_lot_draft_eval.py`
- Modify: `live-auction-agent-service/tests/e2e/test_readonly_planning.py`
- Modify: `live-auction-agent-service/README.md`
- Modify: `live-auction-bid-backend/README.md`
- Modify: `STARTUP.md`
- Modify: `本地启动关闭命令.md`
- Create: `docs/agent-conversation-demo.md`

**Interfaces:**

```text
AGENT_E2E_BASE_URL=http://127.0.0.1:18080
AGENT_E2E_MYSQL_DSN=root:auction_root@tcp(127.0.0.1:13306)/live_auction_agent_test
AGENT_MODEL_PROVIDER=fake                     # default regression
AGENT_MODEL_PROVIDER=deepseek                 # explicit smoke only
DEEPSEEK_API_KEY=<environment secret only>
```

- [ ] **Step 1: 写 E2E 与 Eval 失败测试**

  固定场景：

  1. 现有库存生成方案，取消一项后按批内顺序追加队尾。
  2. 海洋之心示例仅追问主图，上传后确认生成一个新 lot。
  3. 缺起拍价或时长不能创建快照。
  4. 批中一个资产失效导致零新增、零入队、零部分执行记录。
  5. 两会话并发确认，队列位置唯一且批内连续。
  6. 同一 Apply 重放不重复创建。
  7. 入队后 lot 仍未开拍。
  8. Python 执行前后业务表只在最终 Go Apply 阶段发生预期变化。

- [ ] **Step 2: 运行新增测试并确认端到端路径尚未满足**

  Run: `python -m pytest tests/evals/test_lot_draft_eval.py tests/e2e/test_conversation_approval.py -q`

- [ ] **Step 3: 补齐 Compose 配置、文档和演示脚本**

  文档写明启动命令、DeepSeek key 只放 `.env`、演示输入、图片上传、确认、队列手动开拍、故障恢复和原子回滚演示。不得在文档、fixture、日志或截图中提交真实 API key。

- [ ] **Step 4: 执行分层全量验证**

  Run: `python -m pytest -q`（目录：`live-auction-agent-service`）

  Run: `go test ./...`（目录：`live-auction-bid-backend`）

  Run: `npm test`（目录：`live-auction-bid-frontend`）

  Run: `npm run build`（目录：`live-auction-bid-frontend`）

  Run: `docker compose up -d --build`（目录：`live-auction-bid-backend/deploy`）

  Run: `docker compose ps`

  Expected: backend、agent、mysql、redis、rabbitmq、consul 全部 healthy；Prometheus/Grafana started。

- [ ] **Step 5: 运行受控真实 DeepSeek 烟雾测试**

  只从环境变量读取 key，使用一条新拍品与一条库存方案用例；断言结构化结果和必填追问，不将响应原文、Authorization 或 key 写入测试产物。

- [ ] **Step 6: 自查安全与原子性证据**

  Run: `rg -n "create.*lot|queue.*lot|start.*lot" live-auction-agent-service/app/tools live-auction-agent-service/app/skills`

  Expected: Python 工具/技能中无写操作。

  Run: `git diff --check`

  Run: `git status --short`

- [ ] **Step 7: 提交**

  Run: `git add live-auction-agent-service/tests live-auction-agent-service/README.md live-auction-bid-backend/README.md docs && git commit -m "test(agent): verify conversational approval flow"`

## Final Acceptance Checklist

- [ ] 刷新或重新登录后，会话、消息、候选、上传状态和当前 Run 可恢复。
- [ ] 缺名称、起拍价、时长或主图时不能创建可执行快照。
- [ ] 模型生成的分类、介绍、标签和规则建议可编辑，并显示来源。
- [ ] 最终确认前业务 lot 表没有 NEW 候选对应记录。
- [ ] 默认全选，允许取消与批内排序，至少选择一项。
- [ ] Apply 事务失败后没有新 lot、队列位置、资产绑定、审计或 Outbox 的部分结果。
- [ ] 成功批次连续追加到旧队尾，不改变原有顺序。
- [ ] 幂等重放返回相同 execution 和 lot IDs。
- [ ] Python 只读；Go 是唯一审批写边界。
- [ ] Apply 成功后跳转队列，但不会自动开拍。
- [ ] Python、Go、React 全量测试和前端 build 全部通过。
