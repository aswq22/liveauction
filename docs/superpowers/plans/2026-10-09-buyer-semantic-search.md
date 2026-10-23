# Buyer Semantic Search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 为约 10 万公开拍品提供支持口语和近义表达、可限定直播间且严格执行业务条件的生产级买家检索。

**Architecture:** Go 搜索服务并行执行 PostgreSQL 中文词法召回与 pgvector HNSW 召回，RRF 融合后批量读取权威业务数据并过滤。MySQL 事务写持久化索引任务，独立 worker 幂等投影到代际隔离的 PostgreSQL 索引。H5 使用独立列表接口，LLM 问答复用检索结果。

**Tech Stack:** Go 1.22 / Kratos / Proto / GORM / MySQL 8 / Redis 7 / PostgreSQL / pgvector ≥0.8 / React / TypeScript / Vitest。沿用现有 `lib/pq`、`x/text` 与 `x/sync`，不增加搜索集群。

**Spec:** `docs/superpowers/specs/2026-10-09-buyer-semantic-search-design.md`，已获用户确认。

## Global Constraints

- 规模约 10 万拍品；全平台公开搜索，可限定直播间；草稿、禁用商家与隐藏房间不得返回。
- 暂定峰值 100 QPS；正常 P95 ≤800ms、P99 ≤1500ms、系统错误率 <0.1%，均须实测。
- query 1–256 Unicode 字符；page_size 默认 20、最多 50；CNY 整数分，不接受溢出或非法状态。
- 候选每通道初始 100，RRF k=60；最多两轮、总候选最多 400；相关性门槛用独立评测集校准。
- 搜索 deadline 1500ms；Embedding 子预算 450ms，数据库与回源子预算各 200ms。
- 查询向量 TTL 24h；候选 ID TTL 10s；分页快照 TTL 2m，最多 200 个 ID。
- 语义文本不包含价格、状态或更新时间；模型、维度、模板与供应方版本固定于代际。
- 索引新鲜度 P95 ≤30s；held-out Recall@50 ≥0.90、NDCG@10 ≥0.80；ANN Recall@50 ≥0.95。
- 金额、权限、状态回源失败是依赖错误；Embedding 故障可显式词法降级；无关商品不能填充结果。
- Proto 为协议源；业务错误统一 `reply.result`；不增加旧协议兼容分支；生产 DDL 用显式迁移。
- 跨层测试创建于 `app/auction/service/test`；HTTP 契约测试于 `test/e2e`；不要沿袭现有 internal 中跨层测试的位置。
- 不覆盖既有未提交改动；不把用户改动加入任务提交；真实供应方费用、外部部署与生产迁移单独说明。

## Review Focus

1. “iPhone 15”或“型号 3000”不能误判成价格；“不超过三千”能解析，冲突预算返回明确错误（Task 1）。
2. 商品内容更新后旧 worker 返回成功不能复活旧向量；租约过期后旧 worker 不能确认新任务（Tasks 3–4）。
3. 缓存命中后商家封禁、商品涨价或落锤，仍须重新校验并移除不合条件项（Tasks 6、8）。
4. 热门 query 的首次客户端取消不能取消所有其他等待者；超时不能把 HYBRID 错标为健康（Tasks 7–8）。
5. 查询包含引号、冒号、否定词及全角文字时不得注入 tsquery/SQL，也不得忽略否定后声称应用筛选（Tasks 1、2、9）。

## 工作区与文件地图

以下路径相对 `ByteDanceliveauctioni`。用 B 表示 `live-auction-bid-backend`，H 表示 `live-auction-user-h5`，I 表示 `live-auction-bid-backend/app/auction/service/internal`；命令中显式进入对应目录。文件集合花括号表示该目录内列出的实际文件名。

- 新增 `B/app/auction/service/internal/biz/buyersearch/`：types、query、tokenizer、ranking、service、cursor 各自一项职责。
- 修改 `B/app/auction/service/internal/searchindex/{types,embedding,pgvector,worker}.go`；新增 lexical、generation、worker、retry 文件；被正式替代的旧轮询路径删除。
- 新增 `B/app/auction/service/internal/data/search_{models,jobs,source,read,cache,reconcile}.go`，事务写入点仅增加搜索任务钩子。
- 新增 `B/app/auction/service/internal/service/buyer_search.go` 和 `server/buyer_search.go`，接入既有 service/HTTP 注册。
- 新增 `B/deploy/postgres/migrations/20261009_buyer_search.sql`、`B/deploy/mysql/migrations/20261009_buyer_search.sql`；新增 `B/cmd/search_admin/main.go`。
- 新增 `H/src/features/search/{api/searchApi.ts,model/searchTypes.ts,model/useBuyerSearch.ts}`，改造 SearchPage。
- 新增 `B/app/auction/service/test/buyersearch/`、`B/test/e2e/buyer_search_contract_test.go`、搜索评测/压测工具与运行手册。

重叠文件包括 I 下的 `service/auction.go`、`server/http.go`、`data/room_state_repo.go`、`data/agent_approval_repo.go`，以及 H 的 `src/features/auction/api/auctionApi.ts`、`src/shared/api/types.ts`、`src/shared/api/adapters.ts` 和 `src/app/styles.css`。执行前记录各自 diff；实现尽量用新搜索文件，注册/事务必要改动逐块整合。不能用 stash、reset 或 checkout 丢弃现有内容。隔离工作区必须保留本次依赖的未提交实现，不能直接从 HEAD 开始而漏掉这些实现。

每个 Task 的提交仅包含本任务自有改动；重叠文件含用户修改时用经审查的局部暂存，无法安全分离就暂不提交该文件。不运行 `git add .`。每轮修改后 `git diff --check`。

## 公共接口及类型契约

以下类型位于 `internal/biz/buyersearch/types.go`，后续 Task 只引用这些名字：

```go
type Query struct { Text, RoomID, Cursor string; MinPrice, MaxPrice *int64; Statuses []v1.LotStatus; PageSize int }
type Filters struct { RoomID string; MinPrice, MaxPrice *int64; Statuses []v1.LotStatus }
type ParsedQuery struct { SemanticText, CanonicalText string; Filters Filters; Tokens, Warnings []string; PageSize int }
type Generation struct { ID, Table, Model, ProviderVersion, TemplateVersion string; Dimensions int; MaxDistance float64 }
type Revision struct { Semantic, Metadata uint64 }
type Document struct { LotID, RoomID, MainAccountID, Text, Title, Category string; Tags []string; Public bool; Status v1.LotStatus; Revision Revision; Hash string; Deleted bool }
type Candidate struct { LotID string; Distance *float64; LexicalScore float64; MatchedTokens []string; Rank int }
type Fact struct { Lot *v1.Lot; Public bool; ReadAtUnixMs int64 }
type Hit struct { Fact Fact; MatchReason string }
type Page struct { Hits []Hit; Applied Filters; Warnings []string; Mode, DegradedReason, NextCursor string }
type Index interface { Active(context.Context) (Generation,error); Vector(context.Context,Generation,[]float64,Filters,int) ([]Candidate,error); Lexical(context.Context,Generation,ParsedQuery,int) ([]Candidate,error); Upsert(context.Context,Generation,Document,[]float64) error }
type Embedder interface { Embed(context.Context,[]string) ([][]float64,error) }
type Reader interface { ReadPublicFacts(context.Context,[]string) (map[string]Fact,error) }
type Cache interface { Vector(context.Context,string) ([]float64,bool,error); PutVector(context.Context,string,[]float64,time.Duration) error; Candidates(context.Context,string) ([]Candidate,bool,error); PutCandidates(context.Context,string,[]Candidate,time.Duration) error; Snapshot(context.Context,string) (Snapshot,bool,error); PutSnapshot(context.Context,string,Snapshot,time.Duration) error }
type Snapshot struct { QueryHash, GenerationID string; IDs []string; Mode, DegradedReason string; ExpiresAtUnixMs int64 }
type CursorPayload struct { SnapshotID, QueryHash, GenerationID string; Offset int; ExpiresAtUnixMs int64 }
```

源码必须给字段加明确序列化标签和校验，内部 Money 使用既有 proto；不同层不再创造同义 Candidate/Document 类型。

### Task 1: 查询解析与中文词法规则

**Files:** Create `B/app/auction/service/internal/biz/buyersearch/{types,query,tokenizer,ranking}.go`；Test 同包 `query_test.go`、`tokenizer_test.go`、`ranking_test.go`；Create `B/app/auction/service/test/buyersearch/testdata/query_cases.json`。

**Interfaces:** 产出 `Parse(Query) (ParsedQuery,error)`、`Tokenize(string) []string`、`Fuse([]Candidate,[]Candidate,int) []Candidate`、`SemanticHash(Document,Generation) string`；后续使用本计划公共类型。

- [ ] 编写 `TestParseBudgetAndModelNumbers`：`三千以内→300000` 分，`iPhone 15→nil`，`型号3000→nil`，显式 max 优先；`TestParseBounds`：256 字合法、257 字拒绝、min>max/负金额/冲突预算拒绝、page_size 默认20最大50。
- [ ] 编写 `TestTokenizeChineseAndOperators`：全角与半角归一、汉字二元组、单字处理、拉丁型号保留；`TestNegationWarnings` 验证未知排除条件保留原文并返回提示；`TestRRFAndSemanticHash` 验证 k=60、稳定 tie-break、价格与元数据变化不改变 Hash。
- [ ] 运行 `go test ./app/auction/service/internal/biz/buyersearch -run 'TestParse|TestTokenize|TestNegation|TestRRF'`，确认因新接口缺失失败。
- [ ] 使用 `x/text/unicode/norm`、既有金额规则和版本化词表实现接口；tokens 构造查询时不用拼接 SQL；未知约束不生成假 applied filter。
- [ ] 运行同一测试到 PASS；仅暂存该 Task 新文件并提交 `feat(search): define query parsing and lexical rules`。

### Task 2: 代际索引、HNSW 与中文词法 SQL

**Files:** Modify `I/searchindex/{types,pgvector}.go`；Create `I/searchindex/{generation,lexical}.go`、`B/deploy/postgres/migrations/20261009_buyer_search.sql`、`B/app/auction/service/test/buyersearch/index_integration_test.go`。

**Interfaces:** `PGVectorIndex` 实现公共 `Index`；`CreateGeneration(ctx context.Context,g Generation) error` 与 `ActivateGeneration(ctx context.Context,id string) error` 供管理工具使用。

- [ ] 真 PostgreSQL 测试 `TestIndexGenerationIsolation`：不同代际不混搜、未迁移启动报错、危险 Table 名拒绝；`TestChineseLexicalSearch`：中文同义词/型号可召回，query 中 `':&|!` 不报 SQL 注入或返回无关结果。
- [ ] 编写 `TestRevisionUpsert`：旧 semantic/metadata revision 分别不能覆盖新值；metadata-only 更新不清向量，内容 revision 更新先清旧向量；tombstone revision 阻止旧任务复活。
- [ ] 运行 `go test -tags=integration ./app/auction/service/test/buyersearch -run 'TestIndex|TestChinese|TestRevision'`，使用专用 `SEARCH_TEST_PG_DSN`；未提供 DSN 属于未验证，不能计为通过。
- [ ] 迁移建 generation registry、新代际文档表、HNSW cosine、GIN tsvector、pg_trgm、房间/可见性索引；表名只取 registry 白名单；正文 tokens 用 simple 全文检索，排名权重 A=标题、B=标签/分类、D=描述。
- [ ] `Vector` 返回距离，事务局部设置 iterative_scan=strict_order；初始 ef_search=100、max_scan_tuples=20000；局部公开候选 ≤2000 时精确排序。留配置覆盖，Task 12 校准；测试 PASS 后提交 `feat(search): add generation-safe hybrid indexes`。

### Task 3: 持久化任务及变更捕获

**Files:** Create `I/data/search_{models,jobs}.go`、`B/deploy/mysql/migrations/20261009_buyer_search.sql`；Modify `I/data/{lot_repo,room_state_repo,agent_approval_repo,user_repo,rbac_repo,runtime_projection_shard_repo}.go`；Test `B/app/auction/service/test/buyersearch/jobs_integration_test.go`。

**Interfaces:** `EnqueueSearchChange(ctx context.Context,tx *gorm.DB,lotID string,kind string) error`、`EnqueueSearchScope(ctx context.Context,tx *gorm.DB,scopeType,scopeID string) error`。`ClaimSearchJobs(ctx context.Context,owner string,limit int,lease time.Duration) ([]SearchJob,error)`、`AckSearchJob(ctx context.Context,id,token string) (bool,error)`、`RetrySearchJob(ctx context.Context,id,token,code string,next time.Time,dead bool) (bool,error)`。

`SearchJob` 在 search_models.go 定义 ID、LotID、GenerationID、Kind、Revision、Attempt、LeaseToken、LeaseUntil；revision 从持久化搜索源记录事务递增，不以毫秒时间/价格 version 替代。

- [ ] `TestSearchJobsAtomicRollback` 验证商品失败不留任务、成功必留任务；`TestSearchLeaseClaim` 验证 MySQL SKIP LOCKED 多 worker 分离、过期租约可恢复、旧 token ACK 无效。
- [ ] `TestCaptureAllSearchMutations` 覆盖创建/编辑/审批创建/入队/开拍/取消/成交与用户状态/角色变化；纯出价价变不产生 semantic 任务；运行态终结投影必须捕获 metadata 变化。
- [ ] 先运行真实 MySQL 集成测试到 FAIL，再实现同事务任务钩子；用户状态更新当前非事务路径要纳入事务。范围任务持久化 keyset 进度，每次最多 500 拍品；同任务 lease 与 revision 不使用简单 ID 覆盖。
- [ ] 对房间状态写入点执行 `rg -n 'AuctionRoomModel|Update.*status' app/auction/service/internal/data`；所有真实写入统一调范围钩子，若无修改房间 API则不为此新增产品功能。
- [ ] 运行 `go test -tags=integration ./app/auction/service/test/buyersearch -run 'TestSearchJobs|TestSearchLease|TestCapture'`，`SEARCH_TEST_MYSQL_DSN` 必须专用库；PASS 后仅提交自有 hunks，消息 `feat(search): capture durable indexing jobs`。

### Task 4: 可靠索引 worker、Embedding 校验与恢复

**Files:** Modify `I/searchindex/{embedding,worker}.go`；Create `I/searchindex/retry.go`、`I/data/search_{source,reconcile}.go`；Test `B/app/auction/service/test/buyersearch/worker_integration_test.go` 与 `I/searchindex/embedding_test.go`。

**Interfaces:** `ReadSearchDocument(ctx context.Context,lotID string) (Document,error)`、`ReconcileSearchPage(ctx context.Context,cursor ReconcileCursor,limit int) (ReconcileCursor,bool,error)`；`ReconcileCursor{UpdatedAtUnixMs int64; LotID string}`。`SyncWorker.RunOnce(ctx context.Context) error` 消费 Task 3 jobs、Task 2 Index 和 Embedder。

- [ ] `TestEmbeddingResponseValidation`：重复/丢失 index、维度不符、NaN/Inf/全零拒绝；记录安全供应方错误与 Retry-After；HTTP 响应长度上限保留。
- [ ] `TestWorkerReplayAndOrdering`：PG提交后MySQL ACK前宕机可重放；旧 worker 晚返回不能覆盖新内容；worker 崩溃后租约恢复；隐藏商品不向外部发送正文。
- [ ] `TestReconcileSameTimestampAndDeletion`：≥501 个相同 updated_at 全部补任务、cursor 可重启恢复、删除 tombstone 和禁用商家范围完整收敛。
- [ ] 运行指定测试到 FAIL，替换旧内存游标轮询；任务每轮最多50、worker初始4、租约30s并每10s续期，重试初始1s最大60s最多8次带抖动；429遵守Retry-After，鉴权/配置错误暂停供应方并告警。语义内容 Hash 不变复用向量，元数据独立投影。
- [ ] 上述测试 PASS，验证 worker shutdown 可取消并释放 goroutine；删除被替代旧 `ListLotSearchDocuments` 轮询接线；提交 `feat(search): make indexing restart-safe`。

### Task 5: 混合召回与质量门槛

**Files:** Create `I/biz/buyersearch/{service,service_test}.go`；Modify `I/biz/buyersearch/ranking.go`。

**Interfaces:** `NewService(index Index,embedder Embedder,reader Reader,cache Cache,cfg Config) *Service`、`(*Service).Search(ctx context.Context,q Query) (Page,error)`；`Config` 明确总/子超时、候选限制、cursor密钥、缓存TTL、每代质量配置；质量门槛读 Task 2 Generation.MaxDistance。

- [ ] `TestHybridSemanticRecall` 用确定性两通道 stub 证明同义语义召回与型号词法召回都进入结果；`TestUnrelatedQueryEmpty` 证明没有词法证据且距离超过门槛时不能靠LIVE加分出现。
- [ ] `TestSearchCandidateBounds` 验证两轮、最多400唯一ID、相同融合分按lotID稳定；明确预算在最后过滤，不能依靠旧索引价提前排除候选。
- [ ] 运行 `go test ./app/auction/service/internal/biz/buyersearch -run 'TestHybrid|TestUnrelated|TestSearchCandidate'` 到 FAIL。
- [ ] 实现并行通道、RRF、距离门槛、词法证据、有限补取。门槛在校准前仅用于开发且搜索标记未生产就绪；Task 11产出的质量配置是上线必需文件。
- [ ] 同测试 PASS，race 检测该包；提交 `feat(search): combine semantic and lexical retrieval`。

### Task 6: 批量权威读取与硬条件校验

**Files:** Create `I/data/search_read.go`；Modify `I/biz/buyersearch/service.go`；Test `B/app/auction/service/test/buyersearch/facts_integration_test.go`。

**Interfaces:** `(*Store).ReadPublicFacts(ctx context.Context,ids []string) (map[string]Fact,error)` 实现 Reader。一批MySQL关联读取商品/房间/主账号/owner角色，再 Redis Pipeline 读取现有 runtimeStateKey，以既有 runtimeStateToLot 叠加，不做每拍品独立 round-trip。

- [ ] `TestSearchFactsVisibilityAndBudget`：公开跨商家允许、关系错配/隐藏房间/禁用商家/草稿拒绝；价格≤上限与0元合法；预算以Redis最新值判断，mysql旧价格不得通过；显式状态与房间严格过滤。
- [ ] `TestSearchFactsDependencyError`：LIVE缺runtime/Redis超时、数据库超时返回稳定依赖错误，不当空结果；QUEUED仅在既有领域规则确认无需运行态时使用业务起拍价。
- [ ] `TestSearchFactsBulkRead`：100候选数据访问轮次随批数增长，而非逐拍品N+1；未知ID忽略，已有ID损坏字段失败。
- [ ] 运行真实 MySQL/Redis 测试到 FAIL，再实现Reader与后过滤；不能仅因预算查询需要Redis就允许非预算查询显示无法确定的LIVE状态。
- [ ] `go test -tags=integration ./app/auction/service/test/buyersearch -run TestSearchFacts` 到 PASS；提交 `feat(search): enforce live facts and visibility`。

### Task 7: 缓存、限流、超时与显式降级

**Files:** Create `I/data/search_cache.go`、`I/searchindex/query_embedder.go`、`I/server/buyer_search.go`；Modify `I/biz/buyersearch/service.go`；Test `B/app/auction/service/test/buyersearch/governance_test.go`。

**Interfaces:** Store实现 Cache；`NewQueryEmbedder(inner Embedder,cache Cache,cfg QueryEmbedderConfig) Embedder`。QueryEmbedderConfig 初始并发16、最多64等待者、共享请求最大450ms、熔断连续5失败开30s后单探针；客户端初始每IP每秒5次burst10，部署配置覆盖并计量。

- [ ] `TestQueryCacheKeysAndTTL`：模型/代际/范围/全部条件影响相应key，向量24h、候选10s；候选缓存命中仍调用Reader。
- [ ] `TestSharedEmbeddingCancellation`：首客户端取消不取消其他等待者，全部等待者离开后停止共享工作；缓存关闭仍有并发与排队上限。
- [ ] `TestExplicitLexicalDegradation`：Embedding429/超时/熔断返回LEXICAL和原因；Pg不可用返回SEARCH_UNAVAILABLE；总deadline耗尽/用户取消不会重启无界降级查询。
- [ ] 运行测试到 FAIL，再实现缓存、限流与deadline传播；单飞共享context由组件生命周期与有界deadline控制，不继承首客户context。vector/lexical并行，数据库连接池与worker分配上限通过配置固定。
- [ ] `go test -race ./app/auction/service/test/buyersearch -run 'TestQueryCache|TestShared|TestExplicit'` 到 PASS；提交 `feat(search): bound latency and expose degradation`。

### Task 8: 可恢复且有界分页

**Files:** Create `I/biz/buyersearch/{cursor,cursor_test}.go`；Modify `I/biz/buyersearch/service.go` 与 `I/data/search_cache.go`；Test分页行为新增 `B/app/auction/service/test/buyersearch/paging_test.go`。

**Interfaces:** `EncodeCursor(CursorPayload,[]byte) (string,error)`、`DecodeCursor(string,[]byte,time.Time) (CursorPayload,error)`，使用版本化base64url载荷和HMAC-SHA256；secret至少32字节。

- [ ] `TestCursorBindingAndExpiry`：query/filters/room/generation不符、篡改、超长、offset负值与超范围、过期均拒绝；TTL=2m，快照最多200ID。
- [ ] `TestPagingRevalidatesLiveFacts`：缓存后涨价/封禁/落锤从下一页移除；次页不可重复首屏已消耗ID，offset按候选消费数推进；页可变短，不编造total。
- [ ] `TestCursorCacheUnavailable`：签名有效但快照不存在返回CURSOR_EXPIRED；首次搜索Redis不可用可返回无cursor的真实单页和分页不可用提示。
- [ ] 运行指定单元与跨层测试到 FAIL，再实现签名、快照及再校验；分页会话不跳过限流，也不接受调用方snapshot内容。
- [ ] 测试 PASS 后提交 `feat(search): add signed revalidated pagination`。

### Task 9: Proto、HTTP 与问答复用

**Files:** Modify `B/api/auction/service/v1/auction.proto` 及同目录 `auction.pb.go`、`auction_grpc.pb.go`、`auction_http.pb.go`；Create `I/service/buyer_search.go`；Modify `I/service/{auction,ai}.go`、`I/server/http.go`、`B/app/auction/service/cmd/server/main.go`；Create `B/scripts/generate-search-api.ps1`、`B/test/e2e/buyer_search_contract_test.go`。

**Interfaces:** `SearchBuyerLots(ctx context.Context,*v1.SearchBuyerLotsRequest) (*v1.SearchBuyerLotsReply,error)`，HTTP POST `/api/buyer/lots/search`；Reply包含公共Page的字段、proto可选金额/距离字段、warnings及reply.result，JSON金额沿用既有Money契约。

- [ ] `TestBuyerSearchHTTPContract`：匿名公开查询允许，非公开状态拒绝，金额presence区分0和未传，成功/依赖失败统一reply.result；256/257字符、非法cursor、非法JSON、超大body都有稳定错误。
- [ ] 先跑HTTP契约到FAIL；Windows生成脚本发现现有protoc/plugin并明确检查版本，执行Makefile相同proto输入及source_relative选项；不手改生成文件。
- [ ] service将Query映射Page，注册正式HTTP路由与optional auth；没有生成式模型也返回真实列表；AI问答候选调用同一搜索服务，移除旧全房间遍历/伪排名检索路径，保留问答候选白名单。
- [ ] 搜索启用但配置缺失fail fast；未启用端点明确SEARCH_NOT_CONFIGURED；配置校验与依赖健康分开。审核重叠文件diff，验证原有Agent与拍品注册不变。
- [ ] 运行 `go test ./...` 与 `go test -tags=e2e ./test/e2e -run TestBuyerSearchHTTPContract`（启动专用测试环境）到PASS；提交 `feat(search): expose buyer search contract`。

### Task 10: H5 搜索接入

**Files:** Create `H/src/features/search/api/{searchApi.ts,searchApi.test.ts}`、`H/src/features/search/model/{searchTypes.ts,useBuyerSearch.ts,useBuyerSearch.test.ts}`、`H/src/pages/SearchPage.search.test.tsx`；Modify `H/src/pages/SearchPage.tsx`、`H/src/features/search/model/searchAIState.ts`、`H/src/shared/api/httpClient.ts` 与 `H/src/app/styles.css`。

**Interfaces:** `searchBuyerLots(input: BuyerSearchInput,signal?:AbortSignal): Promise<BuyerSearchReply>`；`useBuyerSearch()` 暴露 query、results、filters、warnings、mode、error、loading、search(input)、loadMore()；共享request须传递signal。

- [ ] `SearchPage.search.test.tsx` 验证口语输入显示真实商品、筛选与LEXICAL降级；无结果不展示静态商品；warnings保留；金额/必需字段损坏显示契约错误。
- [ ] `useBuyerSearch.test.ts` 验证快速两次搜索旧请求取消、旧结果不覆盖、cursor过期重搜、加载更多不重复；从直播间返回恢复查询并重新验证数据。
- [ ] `npm test -- src/features/search src/pages/SearchPage.search.test.tsx` 到FAIL，再按现有hooks与 `src/shared/api/httpClient.ts` 的 apiRequest 模式实现；给 ApiRequestOptions 增加可选signal并透传fetch，使用既有auth/retry边界。
- [ ] 普通搜索按钮接新接口；已有对话式问答如保留则独立入口，不让搜索等待生成回答；用现有样式展示价格变化与实际筛选条件。
- [ ] 上述测试、`npm run build`、`npm run lint` 到PASS；与已有H5改动逐hunk整合，提交 `feat(h5): connect semantic lot search`。

### Task 11: 真实质量评测与模型门槛校准

**Files:** Create `B/cmd/search_eval/{main,metrics}.go`、`B/app/auction/service/test/buyersearch/testdata/relevance/{development,heldout}.json`、`B/docs/search-quality.md`、`B/deploy/search/quality.json`。

**Interfaces:** `search_eval --dataset <path> --base-url <url> --mode hybrid|lexical|ann --output <path>`；评测记录 queryID、eligible judged lotIDs、等级0–3、候选top50、返回top10、模型代际与参数。真实标注来源须记录，不把自动生成标签当人工标注。

- [ ] `TestRankingMetrics` 验证已知样例Recall/NDCG、无相关商品不纳入Recall分母但统计错误推荐率、去重/缺结果处理；ANN与人工相关性分别计算。
- [ ] 先写与运行metric测试到FAIL再实现工具；建立≥200真实中文查询、包含Spec各子集，人工核对 relevance。测试语料可生成但真实标注验收依赖人工审核，未审核则明确未验收。
- [ ] development调模型距离/词法门槛、HNSW参数；heldout独立验证Recall@50≥0.90、NDCG@10≥0.80，各子集单报；ANN对exact top50 Recall≥0.95。不能靠无限降低门槛牺牲无结果查询质量。
- [ ] 质量配置绑定generation、dataset hash、模型版本；无有效质量配置不允许生产启用。真实API配额/密钥不存在时保留工具与测试，记录外部验证缺项，不伪造结果。
- [ ] 测试PASS且报告真实记录；提交工具与非敏感数据 `test(search): add quality evaluation gates`。

### Task 12: 部署管理、可观测性、规模与恢复验收

**Files:** Create `B/cmd/search_admin/main.go`、`B/cmd/search_load/main.go`、`B/deploy/prometheus/rules/buyer-search.yml`、`B/deploy/grafana/dashboards/buyer-search.json`、`B/docs/infra/buyer-semantic-search-runbook.md`、`B/app/auction/service/test/buyersearch/recovery_integration_test.go`；Modify `B/deploy/{docker-compose.yml,prod/docker-compose.yml,prometheus/prometheus.yml,.env.example}`、`I/observability/metrics.go`、`I/server/operation.go`、`B/app/auction/service/cmd/server/main.go`。

**Interfaces:** `search_admin migrate|create-generation|backfill|reconcile|status|activate|rollback|replay-dead`，每个写命令要求明确generation/专用DSN；activate校验回填/任务/质量状态，registry切换一次事务。`search_load --base-url <url> --dataset <path> --qps 100 --duration 30m --output <path>`；原始冷/热缓存与mode计数完整记录。

- [ ] `TestSearchRecoveryAcceptance`：数据库恢复、租约重启、断网、429、乱序任务、dead重放、隐藏删除、代际切换与回滚均能收敛；搜索故障时竞价/核心readyz仍正常，搜索健康如实变差。
- [ ] 实现worker生命周期、正式migration与管理命令；Pin已验证pgvector ≥0.8镜像tag+digest，不填写虚构digest；startup校验extension/schema/dimension；searchAdmin仅对显式目标操作，生成新代际不清旧业务库。
- [ ] 指标覆盖阶段耗时、mode、空结果、候选/淘汰、缓存、供应方状态/费用估算、任务最老年龄/死信/冲突；标签仅固定枚举。告警：最老任务>30s持续5m、死信>0、降级率>5%持续5m、系统错误率>0.1%持续5m；运行手册列备份恢复、密钥轮换、扩容和回滚演练。
- [ ] 在独立压测数据集回填10万文档，真实库集成测试PASS后运行100QPS 30m、200QPS 5m，冷/热各报告硬件、配额、成本、模型版本与EXPLAIN；100QPS P95≤800ms/P99≤1500ms/错误<0.1%，索引新鲜度P95≤30s。超额时调整配额/池/硬件或记录不达标，不能改写为成功。
- [ ] 最终验证后端 `go test ./...`、搜索包race、真库integration/e2e、H5 test/build/lint、质量指标、恢复与压测报告；仅自有改动提交 `ops(search): add production verification and runbook`。生产部署目标与凭据未提供则交付已测试代码和运行命令，不擅自部署现有服务器。

## 自审与执行交接

覆盖映射：Spec 1/5→Tasks1、8–10；Spec6→Tasks1–2、4；Spec7→Tasks3–4、12；Spec8→Tasks5–6；Spec9→Tasks7–8、12；Spec10→Tasks11–12；Spec11→工作区规则与Tasks9–12。Review Focus五项均有具名测试；不同层共享类型与方法名统一。

明确外部验收依赖：真实供应方密钥/配额、人工标注审核、生产硬件与目标环境。代码/确定性测试通过不能替代这些验收。所有计划checkbox初始未完成，当前只完成设计和计划文档。

等待用户审阅本计划并选择执行方式：本会话由主代理逐任务实施，或使用子代理逐任务实施/审查。推荐主代理直接实施，因为任务共享类型且有大量既有未提交重叠文件；选择子代理后才调度代理。执行前读取相应执行skill并按工作区规则保留当前实现。
