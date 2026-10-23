# Barrage Correctness Repair Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the existing barrage path safe and correct on one process before introducing distributed transport.

**Architecture:** Keep the current in-memory window processor temporarily, but enforce admission through explicit room/moderation interfaces, separate authoritative and lossy WebSocket queues, use two rate-limit scopes, and select batches with bounded Top-K memory. Every behavior change starts with a failing regression test.

**Tech Stack:** Go 1.22, Gorilla WebSocket, GORM/MySQL room repository, go-redis/v9 mute cache, Prometheus, standard `testing` package.

**Spec:** `docs/superpowers/specs/2026-09-01-distributed-barrage-repair-design.md`

## Global Constraints

- Barrage delivery remains lossy; authoritative auction delivery must not share barrage capacity.
- Window default is 100 ms and startup rejects values below 1 ms.
- Room IDs contain 1-64 characters from `[A-Za-z0-9_-]`.
- Public clients cannot assign paid or administrative priority.
- All queues, caches, and per-window collections remain bounded.
- Do not stage or modify unrelated dirty-worktree files.

---

## File Structure

- `.gitignore`: allow Go test files and plan/spec artifacts to be tracked.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go`: normalized configuration, dual admission limiters, aggregation, and Top-K selection.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_test.go`: service regression tests and benchmarks.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http.go`: HTTP validation and stable error mapping.
- `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http_test.go`: endpoint admission-order tests.
- `live-auction-bid-backend/app/auction/service/internal/realtime/hub.go`: queue isolation and allocation-safe room broadcasting.
- `live-auction-bid-backend/app/auction/service/internal/realtime/hub_barrage_test.go`: connection-queue regression tests.
- `live-auction-bid-backend/app/auction/service/internal/data/barrage_moderation.go`: Redis-backed mute lookup and public-room adapter.
- `live-auction-bid-backend/app/auction/service/internal/data/barrage_moderation_test.go`: cache failure/mute tests using a command fake.
- `live-auction-bid-backend/app/auction/service/internal/observability/barrage_metrics.go`: bounded-cardinality barrage metrics.
- `live-auction-bid-backend/app/auction/service/cmd/server/main.go`: dependency binding and validated environment configuration.
- `live-auction-bid-backend/deploy/.env.example`: documented safety controls.

### Task 1: Make Regression Tests Trackable

**Files:**
- Modify: `.gitignore`
- Test: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_test.go`

**Interfaces:**
- Consumes: existing Go `_test.go` discovery.
- Produces: test files visible to `git status` and eligible for commits.

- [ ] **Step 1: Add an untracked probe test and verify the ignore bug**

Use the existing `barrage_test.go` and run:

```powershell
git check-ignore -v live-auction-bid-backend/app/auction/service/internal/realtime/barrage_test.go
```

Expected: output names the root `*Test*` ignore rule.

- [ ] **Step 2: Add precise exceptions after the broad rules**

Append:

```gitignore
!*_test.go
!*_test.ts
!*_test.tsx
!docs/superpowers/specs/
!docs/superpowers/specs/*.md
!docs/superpowers/plans/
!docs/superpowers/plans/*.md
```

- [ ] **Step 3: Verify test and planning files are not ignored**

Run:

```powershell
git check-ignore -q live-auction-bid-backend/app/auction/service/internal/realtime/barrage_test.go
if ($LASTEXITCODE -eq 0) { throw 'barrage_test.go is still ignored' }
```

Expected: exit code is non-zero and the script does not throw.

- [ ] **Step 4: Run the current realtime tests**

Run: `go test -count=1 ./app/auction/service/internal/realtime`

Expected: PASS from `live-auction-bid-backend`.

- [ ] **Step 5: Commit**

```powershell
git add .gitignore live-auction-bid-backend/app/auction/service/internal/realtime/barrage_test.go
git commit -m "test: track barrage regression tests"
```

### Task 2: Restore WebSocket Queue Isolation

**Files:**
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/hub.go:155-188`
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/hub_barrage_test.go`

**Interfaces:**
- Consumes: `connection.send`, `connection.barrageSend`, `outboundMessage`.
- Produces: `enqueueAuthoritative(conn, message) bool` and `enqueueBarrage(conn, message) bool` helpers used by Hub delivery.

- [ ] **Step 1: Write failing queue-routing tests**

```go
func TestDeliverRoutesAuctionEventToAuthoritativeQueue(t *testing.T) {
    hub, conn := hubWithBufferedTestConnection("room-1")
    if err := hub.deliver(context.Background(), v1.AuctionEvent{RoomId: "room-1"}); err != nil { t.Fatal(err) }
    select { case <-conn.send: default: t.Fatal("authoritative queue was empty") }
    select { case <-conn.barrageSend: t.Fatal("auction event reached barrage queue"); default: }
}

func TestDeliverPublicBatchDropsOnlyWhenBarrageQueueIsFull(t *testing.T) {
    hub, conn := hubWithBufferedTestConnection("room-1")
    conn.barrageSend <- outboundMessage{metricType: "occupied"}
    conn.barrageSend <- outboundMessage{metricType: "occupied"}
    hub.deliverPublicBatch("room-1", []byte(`{"type":"BARRAGE_BATCH"}`))
    if len(conn.send) != 0 { t.Fatal("barrage touched authoritative queue") }
    select { case <-conn.done: t.Fatal("slow barrage consumer was disconnected"); default: }
}
```

- [ ] **Step 2: Run tests and verify RED**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run 'TestDeliverRoutes|TestDeliverPublicBatch'`

Expected: first test reports an empty authoritative queue; second reports that the authoritative queue was touched.

- [ ] **Step 3: Implement minimal routing correction**

```go
func enqueueAuthoritative(conn *connection, message outboundMessage) bool {
    select {
    case conn.send <- message: return true
    case <-conn.done: return false
    default: conn.hub.leave(conn); conn.close(); return false
    }
}

func enqueueBarrage(conn *connection, message outboundMessage) bool {
    select {
    case conn.barrageSend <- message: return true
    case <-conn.done: return false
    default: return false
    }
}
```

Use `enqueueAuthoritative` from `deliver` and `enqueueBarrage` from `deliverPublicBatch`.

- [ ] **Step 4: Make the writer prefer authoritative work**

Before the blocking select, add a non-blocking authoritative check:

```go
select {
case message := <-c.send:
    if !c.writeOutbound(message) { return }
    continue
default:
}
```

Extract the duplicated write logic into `writeOutbound(outboundMessage) bool` and retain the existing metric label behavior.

- [ ] **Step 5: Run focused and package tests**

Run:

```powershell
go test -count=1 ./app/auction/service/internal/realtime -run 'TestDeliverRoutes|TestDeliverPublicBatch'
go test -count=1 ./app/auction/service/internal/realtime ./app/auction/service/internal/server
```

Expected: PASS.

- [ ] **Step 6: Commit**

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/hub.go live-auction-bid-backend/app/auction/service/internal/realtime/hub_barrage_test.go
git commit -m "fix: isolate barrage websocket capacity"
```

### Task 3: Normalize Configuration and Fix Aggregation Identity

**Files:**
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_test.go`

**Interfaces:**
- Produces: `NormalizeBarrageConfig(BarrageConfig) (BarrageConfig, error)`.
- Produces: `selectBarrages(map[string]*Barrage, int, int64) []Barrage` implemented with a bounded heap.

- [ ] **Step 1: Write failing config and representative-identity tests**

```go
func TestNormalizeBarrageConfigRejectsSubMillisecondWindow(t *testing.T) {
    cfg := DefaultBarrageConfig(); cfg.Window = 500 * time.Microsecond
    if _, err := NormalizeBarrageConfig(cfg); err == nil { t.Fatal("expected invalid window") }
}

func TestAggregateHigherPriorityDuplicateReplacesRepresentative(t *testing.T) {
    service := NewBarrageService(DefaultBarrageConfig(), nil, nil)
    rooms := map[string]map[string]*Barrage{}
    service.aggregate(rooms, Barrage{ID:"ordinary", RoomID:"r", UserID:"u1", Content:"same"})
    service.aggregate(rooms, Barrage{ID:"paid", RoomID:"r", UserID:"u2", Content:"same", Priority:BarragePaid})
    got := rooms["r"]["same"]
    if got.ID != "paid" || got.UserID != "u2" || got.Count != 2 { t.Fatalf("got %+v", got) }
}
```

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run 'TestNormalizeBarrageConfig|TestAggregateHigherPriority'`

Expected: compile failure for missing normalizer and identity assertion failure.

- [ ] **Step 3: Add strict normalization and construct only normalized services**

```go
func NormalizeBarrageConfig(cfg BarrageConfig) (BarrageConfig, error) {
    if cfg.Window > 0 && cfg.Window < time.Millisecond { return BarrageConfig{}, errors.New("barrage window must be at least 1ms") }
    // Apply current defaults for zero values; reject negative capacities and budgets.
    return cfg, nil
}
```

Change `NewBarrageService` to return `(*BarrageService, error)` and update all call sites.

- [ ] **Step 4: Replace representative metadata on priority promotion**

When `message.Priority > existing.Priority`, preserve `existing.Count`, assign `*existing = message`, then restore the incremented count.

- [ ] **Step 5: Add a reference-equivalence Top-K test, then implement heap selection**

The test builds 5,000 fixed candidates, compares IDs from the heap result with a test-only full-sort reference, and asserts the result length is 30. Add `AdminReserved` (default 3) and `PaidReserved` (default 7) to `BarrageConfig`; test that unused administrative capacity flows to paid and then ordinary candidates. Implement a `rankedBarrageHeap` min-heap capped at each tier's effective limit; sort only the final K results for wire order.

- [ ] **Step 6: Verify tests and allocation improvement**

Run:

```powershell
go test -count=1 ./app/auction/service/internal/realtime
go test -run '^$' -bench BenchmarkSelectBarragesFiveThousand -benchmem -benchtime=200x ./app/auction/service/internal/realtime
```

Expected: PASS and selection allocation materially below the previous `568702 B/op` baseline.

- [ ] **Step 7: Commit**

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_test.go
git commit -m "fix: bound barrage window selection"
```

### Task 4: Enforce Room, Global Rate, and Mute Admission

**Files:**
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http_test.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/barrage_moderation.go`
- Create: `live-auction-bid-backend/app/auction/service/internal/data/barrage_moderation_test.go`
- Modify: `live-auction-bid-backend/app/auction/service/cmd/server/main.go`

**Interfaces:**
- Produces: `type PublicRoomValidator interface { PublicRoomExists(context.Context, string) (bool, error) }`.
- Produces: `Hub.BindPublicRoomValidator(PublicRoomValidator)`.
- Produces: `data.NewBarrageModeration(store *Store) realtime.BarrageModeration`.
- Consumes: `Store.FindRoomByID` and Redis key `auction:barrage:mute:{roomId}:{userId}`.

- [ ] **Step 1: Write failing HTTP admission-order tests**

Use a valid test token and fakes that count calls. Cover invalid room syntax, missing room, room lookup error, muted user, and a user alternating room IDs. Assert missing/invalid rooms never call `SubmitPublic`; assert the sixth alternating-room request is globally rate-limited.

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/internal/realtime -run 'TestServeBarrage|TestGlobalBarrageRate'`

Expected: unknown rooms are accepted or the fake validator cannot be bound; alternating rooms bypass the current limiter.

- [ ] **Step 3: Add room syntax and existence validation**

```go
var barrageRoomIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type PublicRoomValidator interface {
    PublicRoomExists(ctx context.Context, roomID string) (bool, error)
}
```

Map `found=false` to 404 and lookup errors to 503 before calling the barrage service.

- [ ] **Step 4: Split the limiter into global and room scopes**

Add `GlobalUserBurst`, `GlobalUserRatePerSecond`, `RoomUserBurst`, and `RoomUserRatePerSecond` to `BarrageConfig`. In `SubmitPublic`, consume global key `userID` before room key `roomID + "\x00" + userID`. Return the same stable rate-limit error.

- [ ] **Step 5: Add Redis-backed mute lookup**

```go
func (m *BarrageModeration) IsMuted(ctx context.Context, roomID, userID string) (bool, error) {
    count, err := m.redis.Exists(ctx, "auction:barrage:mute:"+roomID+":"+userID).Result()
    return count > 0, err
}
```

Use a 20 ms child timeout. Add a 100,000-entry bounded local cache with a 5-second TTL; evict the oldest sampled entry when capacity is reached. Test muted, unmuted, cache hit, expiry, capacity eviction, timeout, and Redis error through a small `Exists` command interface.

- [ ] **Step 6: Bind real dependencies**

In `main.go`, bind `store` as public-room validator and `data.NewBarrageModeration(store)` instead of passing `nil`.

- [ ] **Step 7: Run tests**

Run:

```powershell
go test -count=1 ./app/auction/service/internal/realtime ./app/auction/service/internal/data ./app/auction/service/cmd/server
```

Expected: PASS.

- [ ] **Step 8: Commit**

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http_test.go live-auction-bid-backend/app/auction/service/internal/data/barrage_moderation.go live-auction-bid-backend/app/auction/service/internal/data/barrage_moderation_test.go live-auction-bid-backend/app/auction/service/cmd/server/main.go
git commit -m "fix: enforce barrage admission policy"
```

### Task 5: Remove Per-Batch Room Slice Allocation

**Files:**
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/hub.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/hub_barrage_test.go`

**Interfaces:**
- Produces: room membership split into 64 `connectionShard` values, each with `sync.RWMutex` and a connection map.
- Preserves: `join`, `leave`, `RoomPresence`, and delivery behavior.

- [ ] **Step 1: Write a broadcast allocation benchmark**

Create 100,000 lightweight test connections across 64 shards and benchmark `deliverPublicBatch` with pre-filled barrage queues drained by test goroutines. Report `-benchmem`.

- [ ] **Step 2: Record the RED baseline**

Run: `go test -run '^$' -bench BenchmarkBarrageBroadcast100K -benchmem -benchtime=20x ./app/auction/service/internal/realtime`

Expected: approximately one 100,000-pointer slice allocation per operation from `roomConnections`.

- [ ] **Step 3: Iterate fixed shards without building a global slice**

For each room shard, hold its read lock only while iterating its map and performing non-blocking channel sends. Keep connection close/removal outside the read lock by collecting only failed authoritative connections per shard.

- [ ] **Step 4: Verify behavior and benchmark**

Run:

```powershell
go test -count=1 ./app/auction/service/internal/realtime
go test -run '^$' -bench BenchmarkBarrageBroadcast100K -benchmem -benchtime=20x ./app/auction/service/internal/realtime
```

Expected: tests PASS and the 100,000-pointer allocation disappears.

- [ ] **Step 5: Commit**

```powershell
git add live-auction-bid-backend/app/auction/service/internal/realtime/hub.go live-auction-bid-backend/app/auction/service/internal/realtime/hub_barrage_test.go
git commit -m "perf: shard gateway room broadcasts"
```

### Task 6: Add Metrics, Configuration Documentation, and Phase Gate

**Files:**
- Create: `live-auction-bid-backend/app/auction/service/internal/observability/barrage_metrics.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go`
- Modify: `live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http.go`
- Modify: `live-auction-bid-backend/deploy/.env.example`
- Modify: `live-auction-bid-backend/app/auction/service/cmd/server/main.go`

**Interfaces:**
- Produces: `RecordBarrageAdmission(result string)`, `ObserveBarrageWindow(input, unique, selected int, duration time.Duration)`, and `RecordBarrageViewerDrop()`.

- [ ] **Step 1: Write a metrics registration/gather test**

Exercise one accepted, one rate-limited, and one overloaded admission; gather the default Prometheus registry and assert the stable metric names and result labels exist without `room_id` labels.

- [ ] **Step 2: Verify RED**

Run: `go test -count=1 ./app/auction/service/internal/observability ./app/auction/service/internal/realtime -run BarrageMetric`

Expected: missing metric/function failure.

- [ ] **Step 3: Add bounded-cardinality metrics and calls**

Define counters/histograms named `auction_barrage_admission_total`, `auction_barrage_window_input`, `auction_barrage_window_selected`, `auction_barrage_window_duration_ms`, and `auction_barrage_viewer_dropped_total`. Labels are limited to stable result/reason values.

- [ ] **Step 4: Document environment controls**

Add every phase-one variable with defaults to `deploy/.env.example`, including the two limiter scopes and the 1 ms minimum window.

- [ ] **Step 5: Run the phase-one verification gate**

Run:

```powershell
go test -count=1 ./app/auction/service/internal/realtime ./app/auction/service/internal/server ./app/auction/service/internal/data ./app/auction/service/internal/observability ./app/auction/service/cmd/server
go test -count=20 ./app/auction/service/internal/realtime
go test -run '^$' -bench 'BenchmarkSelectBarragesFiveThousand|BenchmarkBarrageBroadcast100K' -benchmem ./app/auction/service/internal/realtime
git diff --check
```

Expected: all tests PASS, repeated realtime test PASS, benchmarks print bounded allocations, and diff check is clean.

- [ ] **Step 6: Commit**

```powershell
git add live-auction-bid-backend/app/auction/service/internal/observability/barrage_metrics.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage.go live-auction-bid-backend/app/auction/service/internal/realtime/barrage_http.go live-auction-bid-backend/deploy/.env.example live-auction-bid-backend/app/auction/service/cmd/server/main.go
git commit -m "feat: observe barrage degradation"
```
