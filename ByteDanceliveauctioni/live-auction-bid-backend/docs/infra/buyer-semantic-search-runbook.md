# 买家拍品语义检索运行手册

本功能面向全平台公开拍品，可限定直播间。MySQL 保存业务与索引任务，PostgreSQL 保存可重建的语义索引，Redis 保存实时竞价状态及短期搜索缓存。索引和缓存都不作为权限或价格依据。

当前交付状态是代码及本机单元验证，不是生产验收。真实 MySQL 触发器、pgvector 查询计划、人工相关性、10 万拍品压测及故障恢复均须在独立环境执行。默认 `AUCTION_BUYER_SEARCH_ENABLED=false`；不提供伪造的已达标报告或镜像 digest。

## 环境与迁移

使用独立测试库先演练，备份现有数据库，再安排生产变更窗口。要求 MySQL 8 的 `SKIP LOCKED`、PostgreSQL 与 pgvector ≥0.8.0、Redis、具有配额的 Embedding 服务。镜像版本和 digest 应由运维在实际拉取并验证后固定，不使用浮动 latest。数据库连接与 API key 从环境或密钥管理系统注入，不提交到仓库。

在 backend 目录执行下列命令。迁移账户需要安装 extension、创建表、过程和触发器权限；运行账户仅保留业务读写、索引读写及所需过程执行权限。MySQL 脚本只能应用一次，DDL 会隐式提交，禁止应用到未确认的数据库。

```text
mysql --database=<明确的业务库> < deploy/mysql/migrations/20261009_buyer_search.sql
psql <明确的索引库连接> -v ON_ERROR_STOP=1 -f deploy/postgres/migrations/20261009_buyer_search.sql
```

设置 `AUCTION_SEARCH_PG_DSN`、`AUCTION_MYSQL_DSN` 和既有 `AUCTION_EMBEDDING_*` 参数（实际变量以 `searchindex/embedding.go` 为准）。生成代际时必须明确模型部署版本、维度、稳定描述模板，以及开发集校准的余弦距离上限，不把示例距离当通用标准。

```text
go run ./app/auction/service/cmd/search_admin -action create -generation g1 -table buyer_search_docs_g1 -provider-version <部署版本> -max-distance <校准值>
go run ./app/auction/service/cmd/search_admin -action backfill -generation g1
go run ./app/auction/service/cmd/search_admin -action work
go run ./app/auction/service/cmd/search_admin -action status
```

回填每页最多 500 拍品，检查点与任务同事务提交，重启后相同名称继续。完成回填不等于向量全部写入，须等待 pending/running/dead 任务处理完并检查索引覆盖。worker 为 4 个并发消费者，每个任务 30 秒租约、8 秒执行预算、最多 8 次失败重试；429 的 Retry-After 被尊重。只有描述、品类、标签变化需要新向量，价格变化不产生索引任务。隐藏或删除清空向量；旧元数据版本不能复活 tombstone。

## 验证与启用

独立测试库先执行真实集成测试。测试必须使用包含 `test` 的 MySQL 库名；PostgreSQL 测试创建独立代际并清理自身临时表。

```text
go test ./app/auction/service/test -v
go test ./...
go test -race ./app/auction/service/internal/biz/buyersearch
```

前一命令需要 `BUYER_SEARCH_MYSQL_TEST_DSN`、`BUYER_SEARCH_PG_TEST_DSN`；未配置会显示 SKIP，不能作为验收通过。竞态检测需要支持 CGO 的工具链。

收集至少 200 条真实中文买家查询，记录人工判断来源；开发集用于调整距离，留出集独立验收。查询包含品类、用途、中文预算、型号数字、否定、无相关拍品、窄直播间等子集。JSON 输入为数组，字段 `query`、可选 `roomId`、`grades`（拍品 ID 到人工等级 0–3）、可选 `expectEmpty`。检索返回但未被人工判断的拍品会导致评测失败，不能默认为不相关。保留数据集 hash、模型部署、索引参数、硬件与原始报告。

```text
go run ./app/auction/service/cmd/search_eval -mode quality -direct -generation g1 -cases <人工留出集.json>
go run ./app/auction/service/cmd/search_eval -mode ann -direct -generation g1 -cases <人工留出集.json>
go run ./app/auction/service/cmd/search_eval -mode load -generation g1 -cases <压测查询.json> -url <独立测试搜索服务> -qps 100 -duration 30m
go run ./app/auction/service/cmd/search_eval -mode load -generation g1 -cases <压测查询.json> -url <独立测试搜索服务> -qps 200 -duration 5m
```

质量门槛：Recall@50 ≥0.90、NDCG@10 ≥0.80；另在 PostgreSQL 比较 HNSW 与 exact top50，ANN recall ≥0.95。该 ANN 基准、冷/热缓存分别压测、至少 10 万拍品、价格变化/下架/封禁/删除/进程重启/数据库断连/429 恢复演练都必须真实执行。100QPS 目标 P95 ≤800ms、P99 ≤1500ms、错误率 <0.1%，索引新鲜度 P95 ≤30s；不得降低验收标准掩盖缺陷。

汇总由运维审核的 acceptance JSON，字段参见 `searchindex.AcceptanceReport`。数值门禁只验证输入数值，不能证明人工标注或外部实验真实发生，必须附带并核对原始证据。报告时效 24 小时。生产代际不能依据单元测试虚构报告启用。

```text
go run ./app/auction/service/cmd/search_admin -action validate -generation g1 -report <真实验收汇总.json>
go run ./app/auction/service/cmd/search_admin -action activate -generation g1
```

最后设置 `AUCTION_SEARCH_CURSOR_KEY`（至少 32 字节随机秘密）与 `AUCTION_BUYER_SEARCH_ENABLED=true`。启动会检查 active 代际、模型/维度和捕获触发器，不会自动创建 schema。首次引导时，在独立测试环境审核代际验证结果，不直接启用尚未验收的生产服务。

## 搜索行为与故障处理

配置 `AUCTION_EMBEDDING_PROVIDER_VERSION` 为真实模型供应方部署的稳定标识，与创建代际的 `-provider-version` 一致。请求和 worker 根据代际选择模型与维度，禁止同维度不同模型复用向量；共享队列中的 writable 代际必须使用同一供应方部署。不同部署迁移应先设计独立 worker/队列，不支持直接混用。

公开搜索和买家咨询在模型调用前限制请求体 16KiB、单客户端 IP 5QPS/突发10、单进程全局100QPS/突发200，拒绝时返回429或413。仅 `AUCTION_SEARCH_TRUSTED_PROXIES` 中的实际入口 CIDR 可提供转发 IP；默认不信任 X-Forwarded-For。客户端桶上限10000，空闲一分钟清理。100QPS压测须使用多个真实客户端，或隔离测试入口的受信代理模拟客户端，不能将单IP限流结果误认为搜索吞吐。多实例全局配额仍需入口网关统一控制。

搜索 PostgreSQL 临时不可达不会阻止核心竞拍服务启动，搜索请求明确不可用，恢复后连接池重新连接；错误 DSN、缺迁移、缺 active 代际及部署标识不匹配仍是配置错误。生产开关保持关闭直到真实验收。前端翻页使用已提交的查询，游标过期自动重搜一次并替换旧结果，不拼接两批快照。

API 为 `POST /api/buyer/lots/search`。金额使用 CNY 分，支持显式 `minPriceAmount`、`maxPriceAmount`、`roomId`、公开 `statuses`、`pageSize` 和签名 `cursor`。页大小默认20、最大50；查询1–256字符。mode 只返回 HYBRID 或显式 LEXICAL 降级。预算是硬约束，不是排序偏好；不相关查询允许零结果，不能用热门商品补满。

每次返回或翻页批量读取 MySQL 公开权限与 Redis 实时状态。LIVE/EXTENDED 缺失或异常运行状态直接返回 SEARCH_UNAVAILABLE，禁止使用旧 MySQL 价格；QUEUED 使用起拍价。快照最多200个当前符合条件的 ID，有效期2分钟，不缓存价格。游标绑定查询、过滤条件、页大小和代际；过期需要重新搜索。

模型失败时关键词仍可用，LEXICAL 与原因对外可见；MySQL/Redis 事实读取失败不能返回假空结果。查询向量缓存24小时、成功混合候选缓存10秒；缓存命中仍重读事实。单进程共享同query的 Embedding，首个用户取消不会取消其他用户；模型并发32、搜索并发128，连续5次失败熔断30秒。

监控 `auction_buyer_search_requests_total` 与 `auction_buyer_search_duration_seconds`，关注持续降级与错误。数据库另监控 `buyer_search_jobs` 的 pending/running 最老 changed_at、dead 数、lease 到期积压和索引覆盖；暂未提供完整仪表盘或全部任务/成本指标，接入后才能完成监控验收。

死信先查稳定 error_class、供应方配额和依赖恢复，再明确执行 `search_admin -action retry-dead -generation <审核目标>`，此操作重放所有 dead 任务，不局限该代际。重建使用新代际与新回填名称。可回滚到旧的已验证代际（activate），不要删除原始业务库或旧索引；改变模型前隔离 writable 代际，当前 worker 的模型不能写不匹配的代际。应在演练环境验证更新与回滚收敛，再执行生产切换。

关闭搜索功能开关可恢复核心服务独立启动，但普通搜索接口会明确不可用，不回退到房间全扫描。旋转 cursor 秘钥会使已有游标失效，用户需重新搜索。保留 MySQL 任务与 tombstone；删除或清理它们需要另行批准的保留策略，不能靠删除旧索引任务掩盖积压。
