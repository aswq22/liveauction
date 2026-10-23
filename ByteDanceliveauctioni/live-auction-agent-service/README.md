# Live Auction Agent Service

Python 3.12 gRPC runtime for merchant-facing, read-only live-auction planning and conversational lot drafting. It produces structured candidates and required questions for human review; it does not execute or mutate auction business operations.

## Architecture and trust boundary

1. A merchant logs in to the Go backend and calls `POST /api/admin/agent/runs` with a goal and a UUID `Idempotency-Key`.
2. Go derives the tenant and operator from trusted JWT claims, durably claims `(tenant, operator, key)`, persists the Run envelope, and calls `auction-agent:19081` over authenticated gRPC at most once for that claim.
3. The agent may call exactly three tools on the Go backend at `auction-backend:19082`: `inventory.query`, `auction.history.query`, and `session.snapshot.get`.
4. Go re-authorizes every tool call against the persisted Run owner and returns read-only evidence.
5. The model returns a strict Pydantic proposal. Every proposal lot ID must be grounded in the tenant's `inventory.query` result.

Agent Run records, events, and checkpoints are orchestration state. Planning must not write auction lots, bids, events, orders, payments, deposits, or other business facts.

Go maintains one lifecycle-bound projector for each unfinished Run. It reconnects to `StreamRunEvents` from the last committed Python source sequence, stores that source sequence independently from the browser-facing event sequence, and commits the terminal event plus Run status/proposal/error atomically. Backend startup resumes unfinished projections and shutdown cancels and joins them. If `StartRun` commits but its response is lost, or a projector observes `NOT_FOUND`, Go repeats the same durable Run ID/specification through Python's idempotent `StartRun` before resuming the stream; ambiguous transport outcomes never create a false terminal failure. Browser SSE reads the Go durable event log, not Python, so Python subscriber limits apply only to projectors. Python `RESOURCE_EXHAUSTED` during an unambiguous `StartRun` rejection becomes a durable failed Run and HTTP 429; stream handoff/resource errors are retried with bounded backoff.

## Docker Compose

From the repository root:

```bash
cd live-auction-bid-backend/deploy
cp .env.example .env
docker compose --profile agent up -d --build
docker compose --profile agent ps
```

The repository and Compose default provider is the deterministic `mock` model (`AGENT_MODEL_PROVIDER=${AGENT_MODEL_PROVIDER:-mock}`), so local startup requires no external model credential. A value in the ignored `deploy/.env` explicitly overrides that default. The Compose health check waits for the runtime gRPC channel on port `19081`. Relevant ports are:

| Endpoint | Container | Host |
| --- | --- | --- |
| Agent runtime gRPC | `auction-agent:19081` | Not published; Compose network only |
| Go read-only tool gRPC | `auction-backend:19082` | Not published; Compose network only |
| Go HTTP/JWT API | `auction-backend:18080` | `127.0.0.1:18080` |
| PostgreSQL runtime store | `pgvector:5432` | `127.0.0.1:15432` |

Default local merchant login: `main / main_dev_password`.

Stop without deleting containers or volumes:

```bash
docker compose --profile agent stop
```

## Configuration

| Variable | Compose value | Purpose |
| --- | --- | --- |
| `AUCTION_AGENT_GRPC_ADDR` | `:19081` | Agent runtime listen address |
| `AUCTION_AGENT_RUNTIME_ADDR` | `auction-agent:19081` | Go-to-agent target |
| `AUCTION_AGENT_TOOL_GRPC_ADDR` | `:19082` | Go read-only tool listen address |
| `AUCTION_TOOL_GRPC_ADDR` | `auction-backend:19082` | Agent-to-Go tool target |
| `AUCTION_INTERNAL_SERVICE_TOKEN` | local example in `.env.example` | Shared internal gRPC bearer token |
| `AGENT_DATABASE_DSN` | `postgresql://...@pgvector:5432/live_auction_search` | Durable Run event and checkpoint store |
| `AGENT_MODEL_PROVIDER` | `mock` | `mock`, `openai-compatible`, or `deepseek`; this explicitly selects the provider request contract |
| `AGENT_MODEL_TOKEN_LIMIT_FIELD` | provider default | Optional explicit Chat Completions output-cap field: `max_completion_tokens` or `max_tokens`; defaults to `max_tokens` for `deepseek` and `max_completion_tokens` otherwise |
| `AGENT_TOKEN_BUDGET` | `8192` | Cumulative per-Run model input/output budget; exact provider usage is preferred and deterministic conservative estimates are used otherwise |
| `AGENT_MAX_ACTIVE_RUNS_GLOBAL` | `64` | Maximum durable non-terminal Runs accepted by one runtime process |
| `AGENT_MAX_ACTIVE_RUNS_PER_TENANT` | `8` | Maximum durable non-terminal Runs for one tenant |
| `AGENT_MAX_SUBSCRIBERS_GLOBAL` | `256` | Maximum live event-stream subscribers in the process |
| `AGENT_MAX_SUBSCRIBERS_PER_RUN` | `16` | Maximum live subscribers for one Run |

The example token is only for local development. Never commit a production token, DSN credential, or model API key.

### Restart and progress-event semantics

Phase 1 deliberately does not resume interrupted graphs. Before the gRPC server starts listening, every durable non-terminal runtime mirror is atomically changed to `FAILED` with error code `AGENT_RUNTIME_RESTARTED` and exactly one terminal `run.failed` event. Repeating the startup sweep is idempotent, already-terminal Runs are never overwritten, and reconnecting clients can replay the terminal event from PostgreSQL. Existing checkpoints are retained only as inert diagnostic state; a new StartRun must use a new Run ID.

The durable event stream includes safe progress metadata such as `model.started`, `model.completed`, `tool.started`, `tool.completed`, and `proposal.validated`. Payloads are restricted to step, status, tool name, count, and coarse duration buckets. Goals, prompts, skill/model text, tool arguments/results, credentials, tokens, and DSNs are never included.

The token budget is cumulative across the complete Run. The adapter passes the remaining allowance through the explicitly selected provider contract: DeepSeek uses `max_tokens`, while OpenAI-compatible endpoints safely default to `max_completion_tokens`. A custom endpoint that implements the legacy contract must set `AGENT_MODEL_TOKEN_LIMIT_FIELD=max_tokens`; base-URL or model-name substring guessing is never used. Responses with usage metadata are charged exactly, while mock or usage-free responses use a deterministic UTF-8 byte estimate. Exhaustion fails closed with `AGENT_TOKEN_BUDGET_EXCEEDED` and never persists a partial proposal. The step budget remains an independent limit.

## Verification

With the Compose profile healthy:

```bash
python -m pytest tests/e2e/test_readonly_planning.py -v
```

The conversational approval E2E covers required-field questioning, draft-only NEW lots, image binding, immutable review snapshots, atomic mixed-batch rollback, tail ordering, concurrent allocation, idempotent replay, and queued-not-started state:

```bash
python -m pytest tests/evals/test_lot_draft_eval.py tests/e2e/test_conversation_approval.py -q
```

Every E2E merchant, room, lot, session and asset has a unique marker. Cleanup targets only that tenant. The default model is deterministic `mock`; no provider key is needed. A real DeepSeek smoke is opt-in only and never stores response text. Set `AGENT_RUN_REAL_MODEL_SMOKE=1`, provide the key through the process environment or ignored `deploy/.env`, and run the eval selected with `-k deepseek`.

Python's registered tools and packaged skills remain a read-only closed set. The Go Apply endpoint is the sole authority that creates NEW lots, binds assets and appends queue positions in one MySQL transaction.

The E2E uses real Go HTTP login/JWT ownership, waits for a real gRPC-backed Run to complete, and checks that proposal lots are non-empty and tenant-owned. Login and fixture seeding happen before the read-only boundary snapshot. The snapshot dynamically covers every current MySQL table except `agent_runs` and `agent_run_events`; one REPEATABLE READ consistent transaction records column order/name/type plus unambiguous NULL or byte-length/HEX values in stable row order. Exact before/after comparison therefore detects UPDATEs and equal-count replacements as well as inserts/deletes.

Run the complete pinned Python 3.12 suite in Docker. The repository mount supplies tests and Compose configuration; the Docker socket lets the E2E inspect the already-running local profile:

```bash
docker build -t live-auction-agent-service:test -f live-auction-agent-service/Dockerfile.test .
docker run --rm \
  -v "${PWD}:/workspace" \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -w /workspace/live-auction-agent-service \
  -e AUCTION_E2E_BASE_URL=http://host.docker.internal:18080 \
  live-auction-agent-service:test \
  python -m pytest -q --ignore=tests/e2e/test_failure_isolation.py
```

The Docker run covers the complete Python 3.12 suite except the host-orchestrated lifecycle acceptance below. That acceptance intentionally requires the host's canonical Compose paths and must not control a sibling host container through a path-suffix match from inside the test container.

On Windows/PowerShell, run the cross-service approval E2E in the pinned Python 3.12 image instead of the host Python (3.14 is unsupported):

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

## Failure isolation

Backend readiness deliberately does not depend on the agent. If `auction-agent` is stopped, `/readyz` remains `{"ok":true}` and normal lot/bid APIs remain available; only new Agent Runs fail with the stable `AGENT_RUNTIME_UNAVAILABLE` code. Restore it with:

```bash
cd live-auction-bid-backend/deploy
docker compose --profile agent start auction-agent
```

Run the failure-isolation acceptance from the host so Docker lifecycle control stays outside the production agent container:

```bash
python -m pytest tests/e2e/test_failure_isolation.py -v
```

The test serializes lifecycle changes with a cross-process lock in the host temporary directory. The lock identity includes the exact canonical Compose file, project and container ID, and the selected container must also match the expected service, project, config hash and canonical Compose labels; checkouts with the same path suffix cannot match. The lock inode may remain for reuse, but no repository lock file is created. The test preserves the agent's initial running/stopped state even when stop or assertions fail.

Every fixture records unique account, lot, idempotency and Run-goal markers before sending a request. If a server commits registration, lot creation or Run creation but the response is lost or malformed, cleanup resolves IDs only by exact marker equality and then removes dependent rows. An accepted bid's Redis idempotency record supplies its exact bid, runtime event and stream IDs. Cleanup waits for the corresponding shard offset and MySQL projection, cancels a still-LIVE lot, deletes dependency-ordered MySQL rows, removes only the five lot-scoped runtime keys plus the tagged and legacy exact idempotency keys, and uses `XDEL` only for the stream entry whose tenant, lot, bidder, idempotency key and event ID all match the fixture. It never clears a stream or rewinds a consumer cursor.

Every business table must match its complete before-snapshot after cleanup; the shared `auction_runtime_projection_shard_offsets` cursor is excluded because fixture events may legitimately advance it. Final MySQL checks plus Redis `SCAN`/`XRANGE` checks must report zero fixture markers. Cleanup failures are reported without replacing the original test error. The acceptance never deletes volumes or runs `down -v`; the production `auction-agent` service never receives a Docker socket mount.
