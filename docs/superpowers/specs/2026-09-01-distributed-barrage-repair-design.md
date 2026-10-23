# Distributed Barrage Repair Design

Date: 2026-09-01
Status: proposed for implementation

## 1. Objective

Repair the current barrage prototype so that it preserves authoritative auction traffic under load and can serve one live room through multiple WebSocket gateways. Barrage delivery remains deliberately lossy: system stability, bounded cost, and a smooth viewer experience take precedence over complete delivery or durable storage of every comment.

The target data path is:

```text
HTTP gateway admission
  -> room-sharded Redis Stream
  -> shard-owner window processor
  -> Redis Pub/Sub BARRAGE_BATCH
  -> every gateway
  -> gateway-local room broadcast
```

Auction events do not use this path and retain their existing delivery semantics.

## 2. Scope

### Included

- Correct the reversed authoritative-event and barrage connection queues.
- Validate that a room exists before spending barrage capacity.
- Apply both global per-user and per-room per-user rate limits.
- Bind a real moderation implementation for mute checks.
- Preserve the identity of paid and administrative messages during aggregation.
- Reject unsafe window configurations and malformed room identifiers.
- Replace full sorting with bounded Top-K selection.
- Introduce Redis Streams for sharded ingress and Redis Pub/Sub for gateway fan-out.
- Add correctness, multi-instance, overload, and performance tests.
- Add metrics for admission, queueing, filtering, sampling, fan-out, drops, and lag.

### Excluded

- Persisting all ordinary barrages in MySQL.
- Guaranteed delivery, replay, or client acknowledgements for ordinary barrages.
- Historical barrage search.
- Exactly-once processing.
- Replacing the existing authoritative auction-event transport.

## 3. Design Principles

1. Every queue and in-memory collection is bounded.
2. Barrage overload must never disconnect a viewer or consume the authoritative auction-event queue.
3. A room has one active window processor at a time, so its display budget is global rather than multiplied by the number of gateways.
4. Paid and administrative messages have reserved capacity but remain bounded.
5. Redis failures degrade barrage service without failing bids or room state updates.
6. Gateways broadcast only pre-serialized batches and only to locally connected viewers.

## 4. Gateway Admission

The endpoint remains `POST /api/realtime/rooms/{roomId}/barrages`.

Admission order:

1. Enforce HTTP method and 1 KiB request-body limit.
2. Authenticate the access token.
3. Validate the room identifier: 1-64 characters from `[A-Za-z0-9_-]`.
4. Resolve the room through a `PublicRoomValidator`; reject missing or unavailable rooms before rate-limit allocation.
5. Validate UTF-8 content, trim whitespace, and enforce the rune limit.
6. Apply a global per-user token bucket.
7. Apply a per-room per-user token bucket.
8. Query the mute cache through `BarrageModeration`.
9. Apply sensitive-word filtering.
10. Append to the selected ingress stream with a short timeout.

Public callers cannot choose message priority. Paid and administrative messages enter through trusted internal methods and carry server-derived identity and priority.

Failure policy:

- Invalid input or sensitive content: HTTP 400.
- Missing room: HTTP 404.
- Muted user: HTTP 403.
- Rate limit: HTTP 429 with `Retry-After`.
- Redis timeout, stream backlog limit, or admission saturation: HTTP 503 with `Retry-After`.

Mute lookup uses a local bounded TTL cache backed by the existing room/user data service. A cache miss that cannot be resolved within its timeout fails closed for barrage admission with HTTP 503; it does not affect auction APIs.

## 5. Sharded Ingress Queue

Use a configurable number of Redis Streams, default 64:

```text
auction:barrage:ingress:{00..63}
```

The shard is `stableHash(roomId) % shardCount`. Stream entries contain the normalized barrage payload and server timestamp. `XADD` uses approximate `MAXLEN`, configured per shard, so memory remains bounded. Ordinary messages are not retried indefinitely when append fails.

Each shard has one active processor selected with the repository's existing lease mechanism. The lease key is:

```text
auction:lease:barrage-window:{shard}
```

Only the lease owner reads and acknowledges that shard. On lease loss it stops reading before another processor takes ownership. Redis consumer-group pending entries are reclaimed after a configured idle timeout. At-least-once input is acceptable because aggregation is lossy and duplicate content is merged.

The shard count is immutable for a deployment generation. Changing it requires draining the old stream set before switching producers.

## 6. Window Processing and Sampling

Each processor maintains state only for rooms observed in its owned shard during the current window. Defaults:

- Window: 100 ms, minimum accepted configuration: 1 ms.
- Maximum unique ordinary content per room/window: 5,000.
- Output budget: 30 messages per room/window.
- Reserved administrative budget: 3.
- Reserved paid budget: 7.
- Remaining budget: ordinary messages.

Aggregation keys use normalized text. An aggregate tracks count and the selected representative message. If a higher-priority duplicate arrives, it replaces the representative identity, ID, timestamp, and display fields; the accumulated count remains.

Selection uses a bounded min-heap rather than sorting the full candidate set. Administrative and paid candidates are selected within their reserved budgets first. Unused reserved capacity flows to the next priority tier. Ordinary candidates receive a deterministic per-window score with quality penalties for very short and repetitive content. The heap keeps only the best `K`, producing `O(n log K)` time and `O(K)` selection memory.

The processor acknowledges ingress entries after the corresponding batch has been published. If Pub/Sub publish fails, entries remain pending for bounded retry. After the retry/age limit, they are acknowledged and counted as dropped; old barrage batches are not useful enough to block the shard.

## 7. Batch Fan-Out

Processors publish batches to Redis Pub/Sub channels partitioned by room hash:

```text
auction:barrage:batches:{00..63}
```

This is Pub/Sub, not a consumer group: every gateway instance subscribes to all batch partitions. A gateway inspects the batch room ID and discards it immediately when it has no local connections for that room.

The wire payload remains pre-serialized JSON:

```json
{
  "type": "BARRAGE_BATCH",
  "roomId": "room-123",
  "windowMs": 100,
  "messages": []
}
```

No gateway re-samples a batch. This guarantees a single global display budget for a room and identical batch contents across gateways.

## 8. WebSocket Queue Isolation

Each connection has two outbound queues:

- `send`: authoritative auction events and snapshots.
- `barrageSend`: lossy barrage batches, capacity two by default.

Authoritative delivery writes only to `send` and retains the existing slow-client policy. Barrage delivery writes only to `barrageSend`. When `barrageSend` is full, the new batch is dropped for that viewer; the viewer stays connected and its authoritative queue is untouched.

The writer always checks `send` before blocking on the combined select so a continuously ready barrage queue cannot starve authoritative state. Payload bytes are shared read-only across connections.

Room membership must not allocate a new 100,000-element slice for every batch. The Hub will maintain a read-optimized room snapshot or iterate fixed connection shards so broadcast allocation is proportional to changed membership rather than batch frequency.

## 9. Configuration

Configuration is parsed and normalized at startup. Invalid safety-critical values fail startup rather than silently falling back.

Required additions include:

- `AUCTION_BARRAGE_BUS=redis`
- `AUCTION_BARRAGE_STREAM_PREFIX`
- `AUCTION_BARRAGE_BATCH_CHANNEL_PREFIX`
- `AUCTION_BARRAGE_SHARDS` (default 64)
- `AUCTION_BARRAGE_STREAM_MAXLEN`
- `AUCTION_BARRAGE_WINDOW` (minimum 1 ms)
- `AUCTION_BARRAGE_MAX_PER_ROOM_WINDOW`
- `AUCTION_BARRAGE_ADMIN_RESERVED`
- `AUCTION_BARRAGE_PAID_RESERVED`
- `AUCTION_BARRAGE_GLOBAL_USER_RATE`
- `AUCTION_BARRAGE_GLOBAL_USER_BURST`
- `AUCTION_BARRAGE_ROOM_USER_RATE`
- `AUCTION_BARRAGE_ROOM_USER_BURST`

Local development may use an explicit `memory` mode. Production startup rejects `memory` mode when more than one gateway instance is configured.

## 10. Observability

Metrics avoid unbounded `roomId` labels. Required metrics:

- Admission totals by result.
- Stream append latency and failures.
- Stream length, pending count, and oldest pending age by shard.
- Window input, unique, selected, aggregated, filtered, and dropped histograms.
- Pub/Sub publish failures.
- Gateway batch receives and no-local-room discards.
- Per-viewer barrage queue drops.
- Window processing duration and lease ownership.

Alerts cover sustained admission overload, shard lag exceeding one second, repeated publish failure, and increased authoritative WebSocket drops.

## 11. Testing Strategy

All behavioral repairs follow red-green TDD.

### Unit tests

- Authoritative events use `send`; barrage batches use `barrageSend`.
- Full `barrageSend` drops only barrage and does not close the connection.
- Invalid and unknown room IDs are rejected before consuming rate tokens.
- Changing room IDs cannot bypass the global user limiter.
- Muted users and moderation lookup failures follow the defined policy.
- A paid duplicate replaces the representative identity while preserving count.
- Window values below 1 ms are rejected without panic.
- Reserved priority budgets and overflow behavior are deterministic.
- Top-K output matches a reference full-sort implementation for fixed inputs.

### Integration tests

- Two gateway Hubs subscribe to one batch bus; viewers on both receive the same batch.
- Only one processor owns a shard and applies one room-level output budget.
- Lease handoff reclaims pending entries without indefinite duplication.
- Redis interruption causes barrage degradation while a bid path remains healthy.

### Performance tests

- Admission benchmark with parallel distinct users.
- 50,000 messages in one 100 ms hot-room window.
- Top-K benchmark with allocation reporting.
- Gateway broadcast benchmark with simulated room sizes up to 100,000 connections using a write-free sink.
- Soak test verifies bounded heap, stream, limiter-cache, and room-membership memory.

The initial acceptance gates are:

- No regression in existing realtime, server, and bid tests.
- No test file excluded by `.gitignore`.
- Hot-window processing completes within 50 ms at the agreed test hardware baseline.
- Barrage saturation produces no authoritative queue drops in the isolation test.
- Cross-gateway integration produces one identical batch per window on every gateway.

## 12. Rollout

1. Ship correctness fixes and metrics with barrage admission disabled.
2. Enable Redis ingress and processing in a staging environment.
3. Run multi-gateway load and failure tests.
4. Enable a small percentage of rooms, observing lag, drops, Redis memory, and bid latency.
5. Increase room coverage gradually.
6. Keep a kill switch that disables public barrage admission and batch subscription without restarting auction processing.

Rollback disables barrage admission and processors. It does not require auction-event rollback or database migration.
