# LiveAuction Load Test

This directory contains an isolated, repeatable load-test setup for the auction backend.

## Safety Model

The load-test target must not be the normal local backend.

- Normal local backend: `http://127.0.0.1:18080`
- Load-test backend: `http://127.0.0.1:18081`
- Normal database: `live_auction`
- Load-test database: `live_auction_loadtest`
- Normal Redis: `127.0.0.1:6379`
- Load-test Redis: `127.0.0.1:6380`
- Normal RabbitMQ exchange/queue: `auction.events` / `auction.realtime.websocket`
- Load-test RabbitMQ exchange/queue: `auction.loadtest.events` / `auction.loadtest.realtime.websocket`

The scripts refuse to prepare or run against `:18080`.

## Start Docker-Isolated Backend

From `live-auction-bid-backend`:

```powershell
docker compose -p live-auction-loadtest -f .\deploy\docker-compose.loadtest.yml up -d --build
```

For a clean database and broker volume:

```powershell
docker compose -p live-auction-loadtest -f .\deploy\docker-compose.loadtest.yml down -v
docker compose -p live-auction-loadtest -f .\deploy\docker-compose.loadtest.yml up -d --build
```

Stop it without deleting volumes:

```powershell
docker compose -p live-auction-loadtest -f .\deploy\docker-compose.loadtest.yml down
```

Docker load-test ports:

- Backend: `http://127.0.0.1:18081`
- MySQL: `127.0.0.1:23306`
- Redis: `127.0.0.1:26379`
- RabbitMQ AMQP: `127.0.0.1:25672`
- RabbitMQ Management: `http://127.0.0.1:25673`

## Prepare Data

From `live-auction-bid-backend`:

```powershell
$env:LOADTEST_CONFIRM='YES'
$env:LOADTEST_MODE='mixed'
$env:BUYERS='1000'
node .\loadtest\scripts\prepare-auction-data.mjs
```

The script writes a data file under:

```text
loadtest/.runs/<runId>.json
```

Modes:

- `bid`: creates a started lot and buyers.
- `payment`: creates a started lot, settles it through a cap bid, and creates one order.
- `mixed`: prepares both bidding and payment data.

## Run k6

Install k6 first if it is not available:

```powershell
winget install k6.k6
```

Then run scenarios with the generated data file:

```powershell
$env:DATA_FILE='D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\loadtest\.runs\<runId>.json'
$env:VUS='100'
$env:DURATION='1m'
k6 run .\loadtest\k6\payment-idempotency.js
```

Recommended ramp:

```powershell
$env:VUS='100';  $env:DURATION='1m';  k6 run .\loadtest\k6\payment-idempotency.js
$env:VUS='300';  $env:DURATION='3m';  k6 run .\loadtest\k6\payment-idempotency.js
$env:VUS='500';  $env:DURATION='5m';  k6 run .\loadtest\k6\payment-idempotency.js
$env:VUS='1000'; $env:DURATION='10m'; k6 run .\loadtest\k6\payment-idempotency.js
```

Bid hot path:

```powershell
$env:VUS='300'
$env:DURATION='3m'
k6 run .\loadtest\k6\auction-bid-hot-path.js
```

Mixed live auction:

```powershell
$env:VIEWER_VUS='500'
$env:BIDDER_VUS='100'
$env:PAYER_VUS='50'
$env:DURATION='5m'
k6 run .\loadtest\k6\mixed-live-auction.js
```

WebSocket watcher pressure:

```powershell
$env:WS_VUS='300'
$env:DURATION='3m'
k6 run .\loadtest\k6\websocket-realtime.js
```

## Verify Result

After a run:

```powershell
$env:DATA_FILE='D:\Go\Auc\ByteDanceliveauctioni\live-auction-bid-backend\loadtest\.runs\<runId>.json'
node .\loadtest\scripts\verify-load-result.mjs
```

The verifier checks:

- backend readiness
- order status when an order exists
- pending Outbox count
- payment rows for the prepared order
- distinct payment IDs and idempotency keys

## Production-Level Route

The preferred production-style runner is:

```powershell
$env:LOADTEST_CONFIRM='YES'
node .\loadtest\scripts\run-production-loadtest.mjs
```

Default standard profile:

- bid baseline/stage pressure: `50,100,200,500 VUs`, `30s` each, fresh lot per stage
- bid stability pressure: `200 VUs`, `10m`
- payment idempotency: `100 VUs`, `1m`, same order and same idempotency key
- websocket watcher pressure: `100 VUs`, `1m`
- eventual consistency wait window: `60s`

Quick smoke profile:

```powershell
$env:LOADTEST_CONFIRM='YES'
$env:LOADTEST_PROFILE='quick'
node .\loadtest\scripts\run-production-loadtest.mjs
```

Useful overrides:

```powershell
$env:BID_STAGE_VUS='50,100,200,500'
$env:BID_STAGE_DURATION='30s'
$env:SOAK_VUS='200'
$env:SOAK_DURATION='10m'
$env:PAYMENT_VUS='100'
$env:PAYMENT_DURATION='1m'
$env:WEBSOCKET_VUS='100'
$env:WEBSOCKET_DURATION='1m'
$env:CONSISTENCY_WAIT_SECONDS='60'
```

The runner writes:

```text
loadtest/.runs/reports/<runId>.md
loadtest/.runs/reports/<runId>.json
loadtest/.runs/reports/<runId>-<stage>.json
```

The report includes:

- QPS, p95, p99, max latency, HTTP failure rate
- payment idempotency result
- bid idempotency uniqueness result
- eventual consistency catch-up result
- MySQL slow query and InnoDB lock wait counters
- Redis timeout metrics
- RabbitMQ queue depth
- Docker CPU/memory snapshot

1. Use a dedicated load-test environment, not dev or production.
2. Prepare data with a clear `runId`.
3. Run small baseline first: 100 VUs for 1 minute.
4. Increase gradually: 300, 500, 1000 VUs.
5. Watch backend `/metrics`, MySQL connections, Redis latency, RabbitMQ queue depth, and process CPU/memory.
6. Run `verify-load-result.mjs` after every stage.
7. For payment idempotency, the key acceptance rule is: many concurrent retries for the same order and same idempotency key must produce one payment result.
8. For RabbitMQ/Outbox, the key acceptance rule is: `auction_events.streamed_at_unix_ms = 0` must not keep growing after traffic stops.

## Acceptance Targets

Local machine targets are weaker than production targets:

- HTTP failed rate `< 1%` for payment idempotency.
- HTTP failed rate `< 2%` for bid hot path.
- p95 `< 800ms` for bid and `< 600ms` for payment on local baseline.
- Outbox pending eventually returns to `0`.
- Same-order same-key payment retries create one durable payment row.
