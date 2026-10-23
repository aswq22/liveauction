# 直播互动竞拍系统后端

当前阶段：V1 核心业务闭环 + 基础设施主路径落地。

## 项目最强纲领

最高需求基线是用户提供的《抖音电商AI全栈课题-直播竞拍全栈系统（宣讲版）》PDF。后续产品、架构、后端、前端、测试和答辩都必须按该课题评分点推进，不再自由发挥。

必须闭合主流程：商品上架 → 规则配置 → 主播开拍 → 用户实时出价 → WebSocket 同步价格/排名/倒计时 → 自动延时/封顶价自动成交/主播异常取消 → 竞拍结束 → 自动生成订单 → 用户查看结果/模拟支付。

P0 硬点：0 元起拍、固定加价、封顶价自动成交、10-30 秒自动延时、异常取消、订单管理、移动 H5 观众端、WebSocket 心跳重连/快照恢复、被超越/领先/延时/结束提醒、100+ 并发出价一致性。

端划分说明：后端仍提供完整竞拍、出价、用户、WebSocket 契约；但当前兄弟前端 `live-auction-bid-frontend` 只作为商家/主播/运营后台 Web。用户 H5 竞拍端和小程序端后续应作为独立项目接入同一后端契约，不在当前后台前端仓库中实现入口或页面。

工程铁律和完整纲领见根文档：[`PROJECT_CHARTER.md`](./PROJECT_CHARTER.md)。

## 已落地的主链路

- 核心业务：创建拍品、开拍、出价、排行榜、信任卡揭示、Duel、落锤、快照、WebSocket 事件广播。
- 数据主路径：GORM + MySQL 保存拍品、出价和持久幂等键；Redis 缓存出价幂等查询，缓存失败后以 MySQL 记录为准。
- 本地基础设施：Docker Compose 启动 backend / MySQL / Redis / RabbitMQ / Consul。
- 服务注册：启动时向 Consul 注册 `auction-backend`，使用 `/readyz` 作为 Consul HTTP health check。
- 事件流：伴随拍品/出价/支付状态变更的领域事件与业务状态写入同一个 MySQL 事务；事务提交后投递 RabbitMQ，投递确认/错误回写 MySQL，后台 outbox worker 会重推未确认事件，WebSocket 服务作为 RabbitMQ 消费者推送直播间前端。
- 用户系统：自建 username/password 账号，用户 ID 使用雪花字符串；JWT access token + refresh session 支持注册、登录、刷新、登出、me 和 RBAC 团队账号管理。
- 鉴权权限：公开读接口可匿名访问；出价走 `bid.place`，后台操作按 `lot.*`、`auction.control`、`order.manage`、`team.user.*` 等权限码判断，不再使用旧角色枚举做运行时鉴权。
- 健康检查：`/healthz` 存活检查，`/readyz` 检查 MySQL + Redis、事件 outbox worker 最近修复状态、Consul 注册可观测状态。
- 统一响应：service 对外只用 reply.result 表达业务成功/失败；Go `error` 不再承载可预期业务错误，避免前端同时解析 body 和 transport error。

被替代的 `MemoryStore`、`database/sql` repo、手写 `schema.go` 主路径已删除，不保留 fallback 或双实现开关。

## 本地运行

```bash
cd deploy
cp .env.example .env
# 默认使用 local 图片存储，无需填写 TOS 配置。
docker compose up --build
```

服务默认监听：`http://127.0.0.1:18080`。

Docker Compose 默认使用 `AUCTION_STORAGE_PROVIDER=local`，图片保存在宿主机 `deploy/local-assets`，本地启动无需 TOS。只有显式将 `AUCTION_STORAGE_PROVIDER` 切换为 `tos` 时，才需要填写 endpoint、region、bucket、access key 和 secret key；任一必填项缺失时 Compose 或后端会 fail fast，不会等到 upload 接口才返回 `storage not configured`。

Docker Compose 默认创建本地主账号：

```text
username: main
password: main_dev_password
```

常用检查：

```bash
curl http://127.0.0.1:18080/healthz
curl http://127.0.0.1:18080/readyz
```

### 只读规划 Agent Compose profile

本地规划链路由商家端调用 Go HTTP/JWT Agent Run API；POST 必须携带 UUID `Idempotency-Key`，MySQL 以 `(tenant, operator, key)` 唯一约束处理并发与响应丢失重试，同 key 不同规范化 goal 返回 409。Go 从 JWT claims 派生 `tenant_id` 和 `operator_id`，再调用 Python runtime。`StartRun` 的 Unavailable、Deadline、Canceled、Unknown 等结果未知错误不会写本地失败终态；Run 保持可投影，projector 遇到 Python NotFound 时以相同 Run ID、goal、tenant/operator 和 trace 幂等重放 StartRun。只有容量、参数、权限等明确拒绝才本地终结。每个未完成 Run 只有一个受应用生命周期管理的后台 projector；它按独立持久化的 Python source sequence 断点续传，把事件写入 Go durable log，并在同一事务内落终态、proposal 与安全错误。浏览器 SSE 只 replay/poll Go 日志，不直接占 Python subscriber。Python 只能通过 Go 暴露的 `inventory.query`、`auction.history.query`、`session.snapshot.get` 三个 gRPC 只读工具取证，不能直接写拍品、出价、订单或支付业务表。最终 proposal 的 lot ID 必须来自该主账号的 inventory evidence。

```bash
cd deploy
cp .env.example .env
docker compose --profile agent up -d --build
docker compose --profile agent ps
```

该 profile 增加 `auction-agent` 和 `pgvector`。本地端口如下：

| 服务 | 容器地址 | 宿主地址 |
| --- | --- | --- |
| Go HTTP API | `auction-backend:18080` | `127.0.0.1:18080` |
| Python Agent runtime gRPC | `auction-agent:19081` | Not published; Compose network only |
| Go read-only tool gRPC | `auction-backend:19082` | Not published; Compose network only |
| PostgreSQL / pgvector | `pgvector:5432` | `127.0.0.1:15432` |

关键配置为 `AUCTION_AGENT_RUNTIME_ADDR`、`AUCTION_AGENT_TOOL_GRPC_ADDR`、`AUCTION_TOOL_GRPC_ADDR`、`AUCTION_INTERNAL_SERVICE_TOKEN`、`AGENT_DATABASE_DSN` 和 `AGENT_MODEL_PROVIDER`。Compose 默认 `AGENT_MODEL_PROVIDER=mock`，无需模型密钥；`.env.example` 的共享 token 只适合本地开发，其他环境必须替换且不得提交真实 secret。

默认登录仍为 `main / main_dev_password`。安全停止且保留 containers/volumes：

```bash
docker compose --profile agent stop
```

故障隔离是有意设计：backend readiness 不依赖 Agent。单独执行 `docker compose --profile agent stop auction-agent` 后，`GET /readyz` 仍返回 `{"ok":true}`，普通拍品与竞价链路继续工作，只有新建 Agent Run 稳定返回 `AGENT_RUNTIME_UNAVAILABLE`。恢复使用 `docker compose --profile agent start auction-agent`。

真实只读验收：

```bash
cd ../../live-auction-agent-service
python -m pytest tests/e2e/test_readonly_planning.py -v
```

测试通过真实 Go 登录/JWT/ownership 边界创建 Run、等待 proposal，并验证 lot ID 全属于登录主账号。

### 对话式生成、确认与入队

商家后台 `/admin/agent` 支持可恢复会话：自然语言生成 EXISTING/NEW 候选项，缺名称、起拍价、时长或主图时只追问必要字段；字段和来源可编辑。确认页默认全选，允许取消和批内排序。不可变快照确认后，Go `Apply` 才在一个 MySQL 事务内创建 NEW lot、绑定资产、写审计/Outbox，并从旧队尾分配连续位置。成功入队不会自动开拍。

Python Agent 没有创建、修改、入队或开拍工具。失效资产、候选版本或快照冲突会整批回滚；相同幂等键重放返回相同 execution 与 lot IDs。

使用项目固定的 Python 3.12 测试镜像，不要直接使用当前宿主机 Python 3.14：

```powershell
cd D:\Go\Auc\ByteDanceliveauctioni
docker build -t live-auction-agent-service:test -f live-auction-agent-service\Dockerfile.test .
docker run --rm --add-host host.docker.internal:host-gateway `
  -v "${PWD}:/workspace" `
  -v /var/run/docker.sock:/var/run/docker.sock `
  -w /workspace/live-auction-agent-service `
  -e AUCTION_E2E_BASE_URL=http://host.docker.internal:18080 `
  live-auction-agent-service:test `
  python -m pytest tests/e2e/test_conversation_approval.py -v
```

E2E 注册独立测试主账号和房间，只清理自己的数据；不会清空共享数据库。完整人工演示与原子回滚步骤见 `..\docs\agent-conversation-demo.md`。

只读验证不是固定表清单或 `COUNT(*)`：登录和 seed 完成后，测试动态枚举当前 MySQL schema，仅排除 `agent_runs` / `agent_run_events` 两张 Agent 编排表；在 REPEATABLE READ 一致性事务内按列序、列名、类型、NULL/字节长度/HEX 值和稳定行序抓取所有其余表的完整快照。因此 UPDATE、等量增删以及 products/SKU、room state 等表的变化都会失败。

Agent 故障隔离由宿主机单独编排，不把 Docker socket 挂进生产 agent 容器：

```bash
cd ../../live-auction-agent-service
python -m pytest tests/e2e/test_failure_isolation.py -v
```

该测试只停止 `auction-agent`，验证 readiness 和普通 lot list 后，创建带唯一前缀的独立 merchant/bidder、LIVE lot、地址与 mock 保证金，通过真实 HTTP/JWT 成功接受一笔竞价，再验证新 Agent Run 稳定失败为 `AGENT_RUNTIME_UNAVAILABLE`；`finally` 始终启动 agent 并等待 healthy。测试保留 volumes，不执行 `down -v`。

### 旧数据库 volume 与 migration

全新数据库由 GORM `AutoMigrate` 按当前 model 创建表和索引；如果本地不需要保留旧数据，建议先清理旧 volume 后再启动：

```bash
cd deploy
docker compose down -v
docker compose up --build
```

如果继续使用旧 Docker volume，需要手动在 MySQL 中执行以下 migration，完成订单/支付表、出价幂等索引、`auction_bids.idempotency_key NOT NULL` 和 RBAC 全量替换：

```bash
deploy/mysql/migrations/20260523_audit_fix.sql
deploy/mysql/migrations/20260523_p3_0_bid_idempotency_required.sql
deploy/mysql/migrations/20260531_full_rbac_replace.sql
```

旧库的索引和 `auction_users.role` 删除不会只靠重新启动自动补齐；必须执行 migration 或清库重建。

### 火山引擎 TOS 图片上传配置

前端添加拍品页统一调用 `POST /api/uploads/images`，后端通过 `StorageProvider` 接火山引擎 TOS。AK/SK 只放运行环境变量，不进入前端或仓库。

Docker Compose 默认使用 local 图片存储。仅在需要接入火山引擎 TOS 时，才将 provider 切换为 `tos` 并填写以下配置：

```bash
cd deploy
cp .env.example .env
# 编辑 deploy/.env，将 AUCTION_STORAGE_PROVIDER 改为 tos，并填写下面的 TOS 变量。
```

配置示例只展示字段名，真实值只放本地 `deploy/.env`：

```env
AUCTION_STORAGE_PROVIDER=tos
AUCTION_TOS_ENDPOINT=<tos-endpoint>
AUCTION_TOS_REGION=<tos-region>
AUCTION_TOS_BUCKET=<tos-bucket>
AUCTION_TOS_PUBLIC_BASE_URL=<public-base-url>
AUCTION_TOS_USE_SSL=true
AUCTION_TOS_ACCESS_KEY=<tos-access-key>
AUCTION_TOS_SECRET_KEY=<tos-secret-key>
```

上传接口会：校验权限与图片类型 → 生成 `{bizType}/{yyyy}/{mm}/{assetId}.{ext}` 对象键 → 上传 TOS → 写入 `asset_files` → 返回 `asset.imageUrl` 给前端预览和 `createLot.imageUrl` 使用。

用户与出价示例：

```bash
curl -X POST http://127.0.0.1:18080/api/users/register \
  -H 'Content-Type: application/json' \
  -d '{"username":"buyer1","password":"password123","nickname":"买家一号"}'

curl -X POST http://127.0.0.1:18080/api/lots/{lot_id}/bid \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer <access_token>' \
  -d '{"amount":{"amount":11000,"currency":"CNY"},"idempotency_key":"buyer1-11000"}'
```

### 公开直播间可见性规则

`GET /api/rooms` 只返回真实可进入的公开直播间：

- room 必须是 `ACTIVE`。
- room 必须至少有 1 个拍品处于 `QUEUED`、`LIVE` 或 `EXTENDED`。
- 没有拍品，或拍品只处于 `DRAFT`、`READY`、`SETTLED`、`CANCELLED`、`FAILED` 时，不对 H5 公开。
- `/api/admin/rooms` 不受这个过滤影响，仍然返回主账号自己的后台房间列表。

该规则由 `RoomQuery.PublicVisibleOnly` 承载，避免改 proto/schema。

### 实时排行榜 TopN

Redis 仍保存完整竞价排行榜；实时热路径只返回 TopN，避免高并发下每次出价/快照/WS 事件拉全量 ranking。

```env
AUCTION_REALTIME_RANKING_LIMIT=50
```

未设置或设置为非法值时默认 `50`。完整出价历史仍通过订单/出价历史等非热路径分页接口查询。

### 并发出价烟测

需要先启动后端及 MySQL/Redis，然后执行：

```bash
CONCURRENCY=100 node scripts/load-bid-hot-path.mjs
CONCURRENCY=300 node scripts/load-bid-hot-path.mjs
```

如果本机配置了 HTTP 代理，建议显式绕过 localhost：

```bash
NO_PROXY=127.0.0.1,localhost CONCURRENCY=100 node scripts/load-bid-hot-path.mjs
```

可选参数：

```env
BASE_URL=http://127.0.0.1:18080
AUCTION_REALTIME_RANKING_LIMIT=50
MERCHANT_USERNAME=
MERCHANT_PASSWORD=
RUN_ID=
```

脚本会创建/复用商家，创建拍品并排队开拍，确认 `/api/rooms` 可见，注册 N 个买家并并发出价，最后报告 `total/accepted/rejected/errors/P50/P95/P99/finalPrice/leader/rankingLength`，并断言最终价与领先者来自最高有效出价、ranking 已排序且长度不超过 TopN、幂等重放不重复生成出价，封顶成交时买家订单只出现一次。

没有本地 MySQL/Redis/HTTP 服务时，可以先跑业务层并发一致性烟测：

```bash
go test ./app/auction/service/test -run TestConcurrentBidSmokeMaintainsLeaderRankingLimitIdempotencyAndCapOrder -count=1 -v
```

该用例固定 100 个买家并发出价到封顶价，验证公开房间可见、最终成交价/领先者、实时榜 TopN、幂等重放不重复入库、封顶订单只创建一次。它不能替代上面的 Redis Lua HTTP 压测。

公共注册和重置密码保持当前 demo 友好策略，后端不在这一阶段收紧。

如果宿主机安装了 Go：

```bash
go test ./...
go build ./app/auction/service/cmd/server
```

HTTP 黑盒契约测试位于 `test/e2e`，默认未设置后端地址时会跳过：

```bash
go test ./test/e2e
LIVE_AUCTION_E2E_BASE_URL=http://127.0.0.1:18080 go test ./test/e2e -count=1 -v
```

详细说明见 [`docs/infra/e2e-contract-tests.md`](docs/infra/e2e-contract-tests.md)。

## 文档

主要文档位于 `docs/openclaw/v1/`。


### 统一响应与乐观锁冲突语义

当多实例或并发请求触发 lot expected-version 冲突时，data 层统一返回稳定哨兵错误，service 层包装进 reply.result：`code=409001`，`message=lot state changed, please refresh and retry`。前端应刷新拍品快照后提示用户重试，不应按普通 500 处理。

### 工程设计原则

- **统一返回模式（Result Envelope）**：service 层把可预期业务错误收敛到 `ReplyResult`，对前端形成单一解析入口；transport error 只留给不可包装的系统/链路故障。
- **Repository + Unit of Work**：data 层持有 GORM/Redis/事务边界，lot/bid/event 在必要场景进入同一个 MySQL transaction，biz 层只依赖 repo 接口。
- **Transactional Outbox**：业务状态与事件先在 MySQL 同事务落库，再由 outbox worker 推 RabbitMQ，接受 at-least-once，消费侧按 event id 幂等。
- **Registry + Health Check**：server 层负责 Consul 注册与 `/readyz` 聚合观测，不让治理基础设施进入 biz。
- **测试隔离**：跨层业务测试保留在 `app/auction/service/test`；HTTP 黑盒契约测试放在 `test/e2e`；同包内部测试只在确需覆盖私有转换、worker、repo、hub 等内部行为时保留。
