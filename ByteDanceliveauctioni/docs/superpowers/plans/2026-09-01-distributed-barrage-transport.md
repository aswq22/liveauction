# Distributed Barrage Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move accepted barrages through room-sharded Redis Streams and fan selected batches to every WebSocket gateway through Redis Pub/Sub.

**Architecture:** Gateways append normalized messages to one of 64 room-stable streams. A lease-owning processor is the only reader for each shard, applies the global room window, publishes one batch, and acknowledges input. All gateways subscribe to batch partitions and broadcast only to rooms with local viewers.

**Tech Stack:** Go 1.22, go-redis/v9 Streams and Pub/Sub, existing Redis lease provider, Prometheus, Docker Compose integration Redis.

**Spec:** `docs/superpowers/specs/2026-09-01-distributed-barrage-repair-design.md`

## Global Constraints

- Complete `2026-09-01-barrage-correctness-repair.md` first.
- Default shard count is 64 and room hashing is stable across processes.
- Ingress uses approximate stream `MAXLEN`; batch output uses Pub/Sub, never a consumer group.
- A room is processed by one shard owner at a time.
- Redis failures degrade only barrage service.
- Do not persist ordinary barrages in MySQL.
- Do not stage or modify unrelated dirty-worktree files.

---

## File Structure

- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_types.go`: wire messages and stable hashing.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_ingress.go`: ingress and batch transport interfaces.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_redis_ingress.go`: Redis Stream producer.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_redis_ingress_test.go`: producer command tests.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_worker.go`: shard lease lifecycle, pending recovery, windows, publish/ack.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_worker_test.go`: deterministic worker tests with queue/lease fakes.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_pubsub.go`: Redis batch publisher/subscriber.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_pubsub_test.go`: multi-gateway transport tests.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_integration_test.go`: build-tagged real-Redis integration.
- `live-auction-bid-backend/app/auction/service/internal/observability/barrage_metrics.go`: stream, lease, lag, and fan-out metrics.
- `live-auction-bid-backend/app/auction/service/cmd/server/main.go`: mode selection and lifecycle wiring.
- `live-auction-bid-backend/deploy/docker-compose.yml`: production-like Redis mode configuration.
- `live-auction-bid-backend/deploy/docker-compose.loadtest.yml`: load-test worker and gateway settings.
- `live-auction-bid-backend/loadtest/scripts/load-barrage-hot-room.mjs`: HTTP/WebSocket saturation scenario.

### Task 1: Define Stable Transport Contracts

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_types.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_ingress.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_transport_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go`

**Interfaces:**
- Produces: `BarrageIngress.Append(context.Context, Barrage) error`.
- Produces: `BarrageShardQueue.Read`, `ClaimStale`, `Ack`, and `Len`.
- Produces: `BarrageBatchPublisher.PublishBatch(context.Context, BarrageBatch) error`.
- Produces: `BarrageBatchSubscriber.Start(context.Context, func(BarrageBatch)) error`.
- Produces: `BarrageShard(roomID string, shardCount int) int`.

```go
type BarrageIngress interface {
    Append(context.Context, Barrage) error
}
type BarrageShardRecord struct {
    ID string
    Barrage Barrage
    EnqueuedAtMs int64
}
type BarrageShardQueue interface {
    EnsureGroup(context.Context, int) error
    Read(context.Context, int, string, int64, time.Duration) ([]BarrageShardRecord, error)
    ClaimStale(context.Context, int, string, time.Duration, int64) ([]BarrageShardRecord, error)
    Ack(context.Context, int, ...string) error
    Len(context.Context, int) (int64, error)
}
type BarrageBatchPublisher interface {
    PublishBatch(context.Context, BarrageBatch) error
}
type BarrageBatchSubscriber interface {
    Start(context.Context, func(BarrageBatch)) error
    Close() error
}
```

- [ ] **Step 1: Write stable-hash and serialization tests**

```go
func TestBarrageShardIsStableAndBounded(t *testing.T) {
    first := BarrageShard("room-123", 64)
    for i := 0; i < 100; i++ { if got := BarrageShard("room-123", 64); got != first { t.Fatal(got) } }
    if first < 0 || first >= 64 { t.Fatal(first) }
}

func TestBarrageBatchRoundTrip(t *testing.T) {
    input := BarrageBatch{Type:"BARRAGE_BATCH", RoomID:"room-1", WindowMs:100, Messages:[]Barrage{{ID:"b1", Content:"hi"}}}
    raw, _ := json.Marshal(input); var output BarrageBatch; _ = json.Unmarshal(raw, &output)
    if output.Type != input.Type || output.RoomID != input.RoomID || output.WindowMs != input.WindowMs || len(output.Messages) != 1 || output.Messages[0].ID != "b1" { t.Fatalf("got %+v", output) }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run 'TestBarrageShard|TestBarrageBatchRoundTrip'`

Expected: missing type/function compilation failure.

- [ ] **Step 3: Implement the contracts**

```go
type BarrageIngress interface { Append(context.Context, Barrage) error }
type BarrageBatch struct {
    Type string `json:"type"`
    RoomID string `json:"roomId"`
    WindowMs int64 `json:"windowMs"`
    Messages []Barrage `json:"messages"`
}
func BarrageShard(roomID string, n int) int { return int(xxhash.Sum64String(roomID) % uint64(n)) }
```

Use the already indirect `xxhash/v2` module only if it is a direct import elsewhere; otherwise use the repository's FNV implementation and freeze it with golden test values.

- [ ] **Step 4: Change public submission to append through `BarrageIngress`**

Replace the service's concrete channel with an injected ingress. Keep `MemoryBarrageIngress` for explicit local mode and unit tests.

- [ ] **Step 5: Run tests and commit**

Run: `go test -count=1 ./app/auction/service/internal/realtime`

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/barrage_types.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_ingress.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_transport_test.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go
git commit -m "refactor: define barrage transport contracts"
```

### Task 2: Implement Room-Sharded Redis Stream Ingress

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_redis_ingress.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_redis_ingress_test.go`

**Interfaces:**
- Consumes: `BarrageIngress`, `BarrageShard`.
- Produces: `NewRedisBarrageIngress(RedisBarrageConfig) (*RedisBarrageIngress, error)`.

- [ ] **Step 1: Write command-boundary tests**

Inject:

```go
type streamAppender interface { XAdd(context.Context, *redis.XAddArgs) *redis.StringCmd }
```

The fake captures arguments. Assert room `room-123` selects the golden shard stream, `MaxLen` and `Approx` are set, the payload field is valid JSON, and append timeout returns `ErrBarrageOverloaded`.

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run RedisBarrageIngress`

Expected: missing constructor/type failure.

- [ ] **Step 3: Implement normalized configuration and append**

```go
type RedisBarrageConfig struct {
    Addr, Password, StreamPrefix, BatchChannelPrefix, Consumer, InstanceID string
    Shards int
    StreamMaxLen, ReadCount int64
    AppendTimeout, Block, PendingIdle, LeaseTTL, RenewInterval time.Duration
}
```

`Append` creates a timeout child context, marshals one normalized message, and calls `XADD` on `prefix:{shard:02d}` with approximate max length.

- [ ] **Step 4: Run tests and commit**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run RedisBarrageIngress`

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/barrage_redis_ingress.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_redis_ingress_test.go
git commit -m "feat: enqueue barrages in sharded redis streams"
```

### Task 3: Implement the Lease-Owned Window Worker

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_worker.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_worker_test.go`

**Interfaces:**
- Consumes: `auction.LeaseProvider`, `BarrageShardQueue`, `BarrageBatchPublisher`, normalized `BarrageConfig`.
- Produces: `NewBarrageWorker(config, queue, publisher, leases) *BarrageWorker` and `Run(context.Context)`.

- [ ] **Step 1: Write failing single-owner and single-budget tests**

Use two workers, one fake lease provider, and 50,000 messages for one room. Advance a fake clock 100 ms. Assert exactly one worker reads the shard, exactly one batch is published, and selected length does not exceed the room budget.

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run 'TestBarrageWorkerSingleOwner|TestBarrageWorkerGlobalBudget'`

Expected: missing worker failure.

- [ ] **Step 3: Implement shard lease lifecycle**

For each configured shard, periodically acquire `auction:lease:barrage-window:{shard}`. Start a shard loop only after acquisition; renew before TTL/2; cancel the shard loop immediately on failed renew; release on shutdown.

- [ ] **Step 4: Implement read, stale claim, window flush, publish, and ack**

Create the consumer group with `XGROUP CREATE ... MKSTREAM`. On ownership acquisition, claim entries idle longer than `PendingIdle`, then read `>` entries. Group decoded messages by room, flush on the configured ticker, publish, and acknowledge only after publish success. Track retry age; after the configured maximum age, acknowledge and record a drop.

- [ ] **Step 5: Add lease-loss and publish-failure tests**

Assert lease loss stops reads before the second fake owner begins. Assert failed publish leaves IDs pending and successful retry acknowledges them once.

- [ ] **Step 6: Run tests and commit**

Run:

```powershell
go test -count=1 ./app/auction/service/internal/realtime -run BarrageWorker
go test -count=20 ./app/auction/service/internal/realtime -run BarrageWorker
```

Expected: PASS without timing flakes.

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/barrage_worker.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_worker_test.go
git commit -m "feat: process barrage windows by shard lease"
```

### Task 4: Fan Batches to Every Gateway

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_pubsub.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_pubsub_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/hub.go`

**Interfaces:**
- Consumes: `BarrageBatchPublisher`, `BarrageBatchSubscriber`, `Hub.deliverPublicBatch`.
- Produces: `RedisBarragePubSub.Start(ctx, handler)` and `PublishBatch(ctx, batch)`.

- [ ] **Step 1: Write failing multi-subscriber tests**

Create an in-memory Pub/Sub contract fake with two subscribers representing two Hubs. Publish one batch and assert both receive byte-identical payloads. Add a Hub with no local room connection and assert it discards the batch without allocating a room.

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run 'TestBarragePubSubAllGateways|TestBarragePubSubNoLocalRoom'`

Expected: missing Pub/Sub implementation failure.

- [ ] **Step 3: Implement Redis partition subscriptions**

Subscribe each gateway connection to all `batchPrefix:{00..shards-1}` channels. Validate decoded type, room ID, and message count before calling the Hub. Publisher chooses the channel from `BarrageShard(batch.RoomID, shards)`.

- [ ] **Step 4: Add local-presence fast rejection**

Add `Hub.HasRoomConnections(roomID string) bool`. Subscriber calls it before JSON re-marshalling or connection iteration. Incoming bytes remain shared read-only for local sends.

- [ ] **Step 5: Run tests and commit**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run BarragePubSub`

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/barrage_pubsub.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_pubsub_test.go live-auction-bid-backend/app/auction/service/internal/realtime/hub.go
git commit -m "feat: fan barrage batches to every gateway"
```

### Task 5: Wire Redis Mode, Metrics, and Lifecycle

**Files:**
- Modify: `live-auction-bid-backend/app/auction/service/cmd/server/main.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/observability/barrage_metrics.go`
- Modify: `live-auction-bid-backend/deploy/.env.example`
- Modify: `live-auction-bid-backend/deploy/docker-compose.yml`
- Modify: `live-auction-bid-backend/deploy/docker-compose.loadtest.yml`

**Interfaces:**
- Consumes: Redis ingress, worker, Pub/Sub, lease provider, Hub.
- Produces: `newBarrageRuntimeFromEnv(...) (io.Closer, error)` with explicit `memory`, `redis`, and `disabled` modes.

- [ ] **Step 1: Write configuration-mode tests**

Assert `redis` requires Redis address and valid shards, `memory` is rejected in production multi-instance mode, `disabled` registers no HTTP admission, and invalid window/shard settings fail startup.

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/cmd/server -run BarrageRuntimeConfig`

Expected: missing runtime factory failure.

- [ ] **Step 3: Implement lifecycle factory**

The factory creates one Redis ingress, one Pub/Sub client, and one worker manager; binds Hub dependencies; starts subscribers and workers with the application context; closes all Redis resources on shutdown. HTTP admission becomes unavailable when mode is `disabled`.

- [ ] **Step 4: Add shard/lag/fan-out metrics**

Extend metrics with stream append duration/failure, per-shard pending count and oldest age, owned lease gauge, batch publish failure, gateway receive, and no-local-room discard. Shard labels are bounded integers; room IDs are never labels.

- [ ] **Step 5: Update deployment configuration**

Set `AUCTION_BARRAGE_BUS=redis`, 64 shards, bounded max length, prefixes, and lease timings in both Compose files. Document the shard-count migration warning in `.env.example`.

- [ ] **Step 6: Run tests and commit**

Run:

```powershell
go test -count=1 ./app/auction/service/cmd/server ./app/auction/service/internal/realtime ./app/auction/service/internal/observability
```

Expected: PASS.

```powershell
git add live-auction-bid-backend/app/auction/service/cmd/server/main.go live-auction-bid-backend/app/auction/service/internal/observability/barrage_metrics.go live-auction-bid-backend/deploy/.env.example live-auction-bid-backend/deploy/docker-compose.yml live-auction-bid-backend/deploy/docker-compose.loadtest.yml
git commit -m "feat: enable distributed barrage runtime"
```

### Task 6: Add Real-Redis Integration and Hot-Room Load Tests

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_integration_test.go`
- Create: `live-auction-bid-backend/loadtest/scripts/load-barrage-hot-room.mjs`
- Modify: `live-auction-bid-backend/package.json` if the backend load-test scripts expose npm commands; otherwise modify the existing load-test README/launcher used by this repository.

**Interfaces:**
- Consumes: running Redis from `deploy/docker-compose.loadtest.yml`, two gateway runtimes, one worker owner.
- Produces: repeatable integration command and JSON load report.

- [ ] **Step 1: Write build-tagged integration tests**

Under `//go:build integration`, connect to `AUCTION_TEST_REDIS_ADDR`, create unique stream/channel prefixes, start two gateway subscribers and two competing workers, append messages, and assert both gateways receive one identical bounded batch. Add lease handoff and Redis interruption cases. Cleanup only the unique test prefixes.

- [ ] **Step 2: Verify RED before completing transport wiring**

Run: `go test -tags=integration -count=1 ./app/auction/service/internal/realtime -run DistributedBarrage`

Expected before the final integration code: failure showing missing propagation or lease handoff behavior.

- [ ] **Step 3: Implement the hot-room load script**

The script authenticates a configurable user pool, opens configurable WebSockets across gateway URLs, sends 50,000 barrage requests in one second, and records HTTP acceptance/rejection, batch rate, batch size, duplicate counts, WebSocket disconnects, and bid-probe latency. Output one JSON report and exit non-zero when authoritative disconnects occur or room output exceeds the configured budget.

- [ ] **Step 4: Run integration and load gates**

Run:

```powershell
docker compose -f deploy/docker-compose.loadtest.yml up -d --build
$env:AUCTION_TEST_REDIS_ADDR='127.0.0.1:16379'
go test -tags=integration -count=1 ./app/auction/service/internal/realtime -run DistributedBarrage
node loadtest/scripts/load-barrage-hot-room.mjs --requests 50000 --duration-ms 1000
```

Expected: both gateways receive identical bounded batches, one shard owner processes the room, no authoritative viewer disconnect is reported, and the script exits zero.

- [ ] **Step 5: Run the final verification gate**

Run:

```powershell
go test -count=1 ./app/auction/service/internal/realtime ./app/auction/service/internal/server ./app/auction/service/internal/data ./app/auction/service/internal/observability ./app/auction/service/cmd/server
go test -count=20 ./app/auction/service/internal/realtime
go test -run '^$' -bench 'BenchmarkSelectBarragesFiveThousand|BenchmarkBarrageBroadcast100K' -benchmem ./app/auction/service/internal/realtime
git diff --check
```

Expected: all tests PASS, repeated tests PASS, benchmarks remain within the phase-one allocation envelope, and diff check is clean.

- [ ] **Step 6: Commit**

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/barrage_integration_test.go live-auction-bid-backend/loadtest/scripts/load-barrage-hot-room.mjs
git commit -m "test: verify distributed barrage load path"
```
