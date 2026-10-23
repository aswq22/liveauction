"""Authenticated asynchronous gRPC service for durable agent execution."""

from __future__ import annotations

import asyncio
from dataclasses import dataclass
import hmac
import inspect
import json
from typing import Callable

import grpc
from opentelemetry.context import Context
from pydantic import ValidationError

from app.domain.models import RunContext
from app.generated.proto.agent.v1 import common_pb2, runtime_pb2, runtime_pb2_grpc
from app.harness.context import AgentRunSummary, RunInput, RunStatus, RunType
from app.observability.tracing import extract_trace_context
from app.runtime.repository import (
    RunConflictError,
    RunMutationResult,
    RunNotFoundError,
    RuntimeEvent,
    RuntimeRepository,
    RuntimeRun,
)


_PROTO_STATUS = {
    RunStatus.CREATED: common_pb2.RUN_STATUS_CREATED,
    RunStatus.PLANNING: common_pb2.RUN_STATUS_PLANNING,
    RunStatus.COLLECTING_CONTEXT: common_pb2.RUN_STATUS_COLLECTING_CONTEXT,
    RunStatus.GENERATING_PROPOSAL: common_pb2.RUN_STATUS_GENERATING_PROPOSAL,
    RunStatus.VERIFYING: common_pb2.RUN_STATUS_VERIFYING,
    RunStatus.COMPLETED: common_pb2.RUN_STATUS_COMPLETED,
    RunStatus.FAILED: common_pb2.RUN_STATUS_FAILED,
    RunStatus.CANCELLED: common_pb2.RUN_STATUS_CANCELLED,
}
_TERMINAL = frozenset(
    {RunStatus.COMPLETED, RunStatus.FAILED, RunStatus.CANCELLED}
)


class HarnessProtocol:
    async def start(self, run: RunInput) -> AgentRunSummary: ...


HarnessFactory = Callable[[Context], HarnessProtocol]


@dataclass(frozen=True, slots=True)
class _SlowConsumer:
    pass


_SLOW_CONSUMER = _SlowConsumer()


class _SubscriberLimitError(RuntimeError):
    pass


class _RunLimitError(RuntimeError):
    pass


@dataclass(frozen=True, slots=True)
class _Subscription:
    run_id: str
    queue: asyncio.Queue


class _EventBroadcaster:
    def __init__(
        self, queue_size: int, *, max_global: int = 256, max_per_run: int = 16
    ) -> None:
        if queue_size <= 0:
            raise ValueError("subscriber queue size must be positive")
        self._queue_size = queue_size
        if max_global <= 0 or max_per_run <= 0:
            raise ValueError("subscriber limits must be positive")
        self._max_global = max_global
        self._max_per_run = max_per_run
        self._subscriptions: dict[str, set[asyncio.Queue]] = {}
        self._lock = asyncio.Lock()
        self._count = 0

    @property
    def count(self) -> int:
        return self._count

    async def subscribe(self, run_id: str) -> _Subscription:
        queue: asyncio.Queue = asyncio.Queue(maxsize=self._queue_size)
        async with self._lock:
            run_count = len(self._subscriptions.get(run_id, ()))
            if self._count >= self._max_global or run_count >= self._max_per_run:
                raise _SubscriberLimitError("event subscriber limit reached")
            self._subscriptions.setdefault(run_id, set()).add(queue)
            self._count += 1
        return _Subscription(run_id=run_id, queue=queue)

    async def unsubscribe(self, subscription: _Subscription) -> None:
        async with self._lock:
            queues = self._subscriptions.get(subscription.run_id)
            if queues is None or subscription.queue not in queues:
                return
            queues.remove(subscription.queue)
            self._count -= 1
            if not queues:
                self._subscriptions.pop(subscription.run_id, None)

    async def publish(self, event: RuntimeEvent | None) -> None:
        if event is None:
            return
        async with self._lock:
            queues = tuple(self._subscriptions.get(event.run_id, ()))
            for queue in queues:
                try:
                    queue.put_nowait(event)
                except asyncio.QueueFull:
                    while not queue.empty():
                        queue.get_nowait()
                    queue.put_nowait(_SLOW_CONSUMER)
                    self._subscriptions[event.run_id].remove(queue)
                    self._count -= 1
            if event.run_id in self._subscriptions and not self._subscriptions[event.run_id]:
                self._subscriptions.pop(event.run_id, None)


class AgentRuntimeService(runtime_pb2_grpc.AgentRuntimeServiceServicer):
    def __init__(
        self,
        *,
        repository: RuntimeRepository,
        harness_factory: HarnessFactory,
        internal_token: str,
        max_steps: int,
        subscriber_queue_size: int = 128,
        max_active_runs_global: int = 64,
        max_active_runs_per_tenant: int = 8,
        max_subscribers_global: int = 256,
        max_subscribers_per_run: int = 16,
    ) -> None:
        normalized_token = internal_token.strip()
        if not normalized_token:
            raise ValueError("internal token must be nonblank")
        if max_steps <= 0:
            raise ValueError("max_steps must be positive")
        if max_active_runs_global <= 0 or max_active_runs_per_tenant <= 0:
            raise ValueError("active run limits must be positive")
        self._repository = repository
        self._harness_factory = harness_factory
        self._authorization = f"Bearer {normalized_token}"
        self._max_steps = max_steps
        self._broadcaster = _EventBroadcaster(
            subscriber_queue_size,
            max_global=max_subscribers_global,
            max_per_run=max_subscribers_per_run,
        )
        self._max_active_runs_global = max_active_runs_global
        self._max_active_runs_per_tenant = max_active_runs_per_tenant
        self._tasks: dict[str, asyncio.Task] = {}
        self._task_tenants: dict[str, str | None] = {}
        self._closing = False
        self._starting = 0
        self._lifecycle = asyncio.Condition()

    @property
    def tracked_task_count(self) -> int:
        return len(self._tasks)

    @property
    def subscriber_count(self) -> int:
        return self._broadcaster.count

    async def StartRun(self, request, context):
        metadata = tuple(context.invocation_metadata())
        await self._authorize(metadata, context)
        await _validate_correlated_run(request.run_id, request.context.run_id, context)
        try:
            run_input = _start_input(request, self._max_steps)
        except (TypeError, ValueError, ValidationError):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "invalid run request")
        async with self._lifecycle:
            if self._closing:
                await context.abort(
                    grpc.StatusCode.UNAVAILABLE, "runtime is shutting down"
                )
            self._starting += 1
            try:
                existing = await self._repository.get_run(run_input.context.run_id)
                if existing is None:
                    global_count = await self._repository.count_nonterminal_runs()
                    tenant_count = 0
                    if run_input.context.tenant_id is not None:
                        tenant_count = await self._repository.count_nonterminal_runs(
                            tenant_id=run_input.context.tenant_id
                        )
                    local_count = sum(not task.done() for task in self._tasks.values())
                    local_tenant_count = sum(
                        not self._tasks[run_id].done()
                        and tenant_id == run_input.context.tenant_id
                        for run_id, tenant_id in self._task_tenants.items()
                        if run_id in self._tasks
                    )
                    if (
                        max(global_count, local_count)
                        >= self._max_active_runs_global
                        or max(tenant_count, local_tenant_count)
                        >= self._max_active_runs_per_tenant
                    ):
                        raise _RunLimitError("active agent run limit reached")
                created = await self._repository.create_run(
                    context=run_input.context,
                    goal=run_input.goal,
                    max_steps=run_input.max_steps,
                    run_type=request.run_type,
                    session_id=request.session_id,
                    session_version=request.session_version,
                    turn_input_json=run_input.turn_input_json,
                )
            except RunConflictError:
                await context.abort(
                    grpc.StatusCode.ALREADY_EXISTS,
                    "run ID conflicts with an existing run",
                )
            except _RunLimitError:
                await context.abort(
                    grpc.StatusCode.RESOURCE_EXHAUSTED,
                    "active agent run limit reached",
                )
            except Exception:
                await context.abort(grpc.StatusCode.INTERNAL, "runtime operation failed")
            else:
                # Registration immediately follows the known durable commit while the
                # lifecycle lock still serializes starts and shutdown.
                parent_context = extract_trace_context(metadata)
                self._register_task(created.run, parent_context)
            finally:
                self._starting -= 1
                self._lifecycle.notify_all()
        if created.created:
            await self._broadcaster.publish(created.event)
        return runtime_pb2.StartRunReply(
            run=_run_proto(created.run)
        )

    async def GetRun(self, request, context):
        await self._authorize(tuple(context.invocation_metadata()), context)
        await _validate_correlated_run(request.run_id, request.context.run_id, context)
        try:
            run = await self._repository.get_run(request.run_id)
        except Exception:
            await context.abort(grpc.StatusCode.INTERNAL, "runtime operation failed")
        if run is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
        return runtime_pb2.GetRunReply(run=_run_proto(run))

    async def CancelRun(self, request, context):
        await self._authorize(tuple(context.invocation_metadata()), context)
        await _validate_correlated_run(request.run_id, request.context.run_id, context)
        try:
            result = await self._repository.request_cancel(request.run_id)
        except RunNotFoundError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
        except Exception:
            await context.abort(grpc.StatusCode.INTERNAL, "runtime operation failed")
        await self._broadcaster.publish(result.event)
        return runtime_pb2.CancelRunReply(run=_run_proto(result.run))

    async def StreamRunEvents(self, request, context):
        await self._authorize(tuple(context.invocation_metadata()), context)
        await _validate_correlated_run(request.run_id, request.context.run_id, context)
        if request.after_seq < 0:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "after_seq must be nonnegative")
        try:
            run = await self._repository.get_run(request.run_id)
        except Exception:
            await context.abort(grpc.StatusCode.INTERNAL, "runtime operation failed")
        if run is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
        last_sequence = request.after_seq
        if run.status in _TERMINAL:
            try:
                replay = await self._repository.list_events(request.run_id, last_sequence)
            except Exception:
                await context.abort(grpc.StatusCode.INTERNAL, "runtime operation failed")
            for event in replay:
                yield _event_proto(event)
                if event.status in _TERMINAL:
                    return
            return
        try:
            subscription = await self._broadcaster.subscribe(request.run_id)
        except _SubscriberLimitError:
            await context.abort(
                grpc.StatusCode.RESOURCE_EXHAUSTED, "event subscriber limit reached"
            )
        try:
            try:
                replay = await self._repository.list_events(request.run_id, last_sequence)
            except RunNotFoundError:
                await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
            except Exception:
                await context.abort(grpc.StatusCode.INTERNAL, "runtime operation failed")
            for event in replay:
                if event.seq <= last_sequence:
                    continue
                yield _event_proto(event)
                last_sequence = event.seq
                if event.status in _TERMINAL:
                    return
            try:
                durable = await self._repository.get_run(request.run_id)
            except Exception:
                await context.abort(grpc.StatusCode.INTERNAL, "runtime operation failed")
            if durable is None:
                await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
            if durable.status in _TERMINAL:
                while True:
                    slow_consumer = False
                    while True:
                        try:
                            notification = subscription.queue.get_nowait()
                        except asyncio.QueueEmpty:
                            break
                        if notification is _SLOW_CONSUMER:
                            slow_consumer = True
                    if slow_consumer:
                        await context.abort(
                            grpc.StatusCode.RESOURCE_EXHAUSTED,
                            "event subscriber fell behind; reconnect with after_seq",
                        )
                    try:
                        terminal_replay = await self._repository.list_events(
                            request.run_id, last_sequence
                        )
                    except RunNotFoundError:
                        await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
                    except Exception:
                        await context.abort(
                            grpc.StatusCode.INTERNAL, "runtime operation failed"
                        )
                    for event in terminal_replay:
                        if event.seq <= last_sequence:
                            continue
                        yield _event_proto(event)
                        last_sequence = event.seq
                    if terminal_replay:
                        continue
                    if subscription.queue.empty():
                        return
            while True:
                notification = await subscription.queue.get()
                if notification is _SLOW_CONSUMER:
                    await context.abort(
                        grpc.StatusCode.RESOURCE_EXHAUSTED,
                        "event subscriber fell behind; reconnect with after_seq",
                    )
                try:
                    live_events = await self._repository.list_events(
                        request.run_id, last_sequence
                    )
                except RunNotFoundError:
                    await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
                except Exception:
                    await context.abort(
                        grpc.StatusCode.INTERNAL, "runtime operation failed"
                    )
                for event in live_events:
                    if event.seq <= last_sequence:
                        continue
                    yield _event_proto(event)
                    last_sequence = event.seq
                    if event.status in _TERMINAL:
                        return
                if not live_events:
                    try:
                        durable = await self._repository.get_run(request.run_id)
                    except Exception:
                        await context.abort(
                            grpc.StatusCode.INTERNAL, "runtime operation failed"
                        )
                    if durable is None:
                        await context.abort(grpc.StatusCode.NOT_FOUND, "run not found")
                    if durable.status in _TERMINAL:
                        return
        finally:
            await self._broadcaster.unsubscribe(subscription)

    async def shutdown(self) -> None:
        async with self._lifecycle:
            self._closing = True
            await self._lifecycle.wait_for(lambda: self._starting == 0)
            tasks = tuple(self._tasks.items())
        for run_id, _ in tasks:
            try:
                cancellation = await self._repository.request_cancel(run_id)
            except Exception:
                continue
            try:
                await self._broadcaster.publish(cancellation.event)
            except Exception:
                continue
        for _, task in tasks:
            task.cancel()
        if tasks:
            await asyncio.gather(*(task for _, task in tasks), return_exceptions=True)
        for run_id, _ in tasks:
            try:
                await self._persist_cancelled(run_id)
            except Exception:
                continue

    def _register_task(self, run: RuntimeRun, parent_context: Context) -> bool:
        if run.status in _TERMINAL:
            return False
        current = self._tasks.get(run.run_id)
        if current is not None and not current.done():
            return False
        if current is not None:
            self._tasks.pop(run.run_id, None)
        run_input = RunInput(
            context=run.context,
            goal=run.goal,
            max_steps=run.max_steps,
            run_type=_run_type_from_proto(run.run_type),
            turn_input_json=run.turn_input_json,
        )
        task = asyncio.create_task(
            self._execute_run(run_input, parent_context),
            name=f"agent-run-{run.run_id}",
        )
        self._tasks[run.run_id] = task
        self._task_tenants[run.run_id] = run.context.tenant_id
        task.add_done_callback(
            lambda completed, run_id=run.run_id: self._observe_task(
                run_id, completed
            )
        )
        return True

    async def _execute_run(self, run: RunInput, parent_context: Context) -> None:
        try:
            try:
                planning = await self._repository.transition_run(
                    run.context.run_id,
                    status=RunStatus.PLANNING,
                    type="run.planning",
                )
                await self._broadcaster.publish(planning.event)
                async def event_sink(event_type: str, payload: dict) -> None:
                    durable = await self._repository.get_run(run.context.run_id)
                    if durable is None or durable.status in _TERMINAL:
                        return
                    event = await self._repository.append_event(
                        run.context.run_id,
                        type=event_type,
                        status=durable.status,
                        payload_json=_safe_payload_json(event_type, payload),
                    )
                    await self._broadcaster.publish(event)

                harness = self._create_harness(parent_context, event_sink)
                summary = await harness.start(run)
                if summary.run_id != run.context.run_id:
                    raise RuntimeError("harness returned a different run ID")
                await self._persist_summary(summary)
            except asyncio.CancelledError:
                await self._persist_cancelled(run.context.run_id)
        except Exception:
            await self._persist_failed(run.context.run_id)
        finally:
            current = asyncio.current_task()
            if self._tasks.get(run.context.run_id) is current:
                self._tasks.pop(run.context.run_id, None)
                self._task_tenants.pop(run.context.run_id, None)

    async def _persist_summary(self, summary: AgentRunSummary) -> None:
        proposal_json = summary.proposal.model_dump_json() if summary.proposal else ""
        turn_result_json = (
            summary.turn_result.model_dump_json()
            if summary.status is RunStatus.COMPLETED and summary.turn_result
            else ""
        )
        event_type = {
            RunStatus.COMPLETED: "run.completed",
            RunStatus.CANCELLED: "run.cancelled",
            RunStatus.FAILED: "run.failed",
        }.get(summary.status, "run.failed")
        status = summary.status if summary.status in _TERMINAL else RunStatus.FAILED
        error_code = summary.error_code
        error_message = summary.error_message
        if status is RunStatus.FAILED and summary.status not in _TERMINAL:
            error_code = "INTERNAL_RUNTIME_ERROR"
            error_message = "agent runtime execution failed"
        result = await self._repository.transition_run(
            summary.run_id,
            status=status,
            type=event_type,
            proposal_json=proposal_json,
            turn_result_json=turn_result_json,
            error_code=error_code,
            error_message=error_message,
        )
        await self._broadcaster.publish(result.event)

    async def _persist_cancelled(self, run_id: str) -> None:
        result = await self._repository.transition_run(
            run_id,
            status=RunStatus.CANCELLED,
            type="run.cancelled",
            error_code="RUN_CANCELLED",
            error_message="run cancelled",
        )
        await self._broadcaster.publish(result.event)

    async def _persist_failed(self, run_id: str) -> None:
        result = await self._repository.transition_run(
            run_id,
            status=RunStatus.FAILED,
            type="run.failed",
            error_code="INTERNAL_RUNTIME_ERROR",
            error_message="agent runtime execution failed",
        )
        await self._broadcaster.publish(result.event)

    async def _authorize(self, metadata, context) -> None:
        authorization_values = [
            value
            for key, value in _metadata_pairs(metadata)
            if key.lower() == "authorization"
        ]
        valid = len(authorization_values) == 1 and hmac.compare_digest(
            authorization_values[0], self._authorization
        )
        if not valid:
            await context.abort(
                grpc.StatusCode.UNAUTHENTICATED, "invalid internal authorization"
            )

    def _observe_task(self, run_id: str, task: asyncio.Task) -> None:
        if self._tasks.get(run_id) is task:
            self._tasks.pop(run_id, None)
            self._task_tenants.pop(run_id, None)
        try:
            task.exception()
        except asyncio.CancelledError:
            pass

    def _create_harness(self, parent_context: Context, event_sink):
        parameters = tuple(inspect.signature(self._harness_factory).parameters.values())
        accepts_two = any(
            parameter.kind is inspect.Parameter.VAR_POSITIONAL
            for parameter in parameters
        ) or len(parameters) >= 2
        if accepts_two:
            return self._harness_factory(parent_context, event_sink)
        return self._harness_factory(parent_context)


def _start_input(request, max_steps: int) -> RunInput:
    request_run_id = request.run_id.strip()
    context_run_id = request.context.run_id.strip()
    if not request_run_id or request_run_id != context_run_id:
        raise ValueError("request.run_id must equal context.run_id")
    return RunInput(
        context=RunContext(
            run_id=request_run_id,
            request_id=_optional(request.context.request_id),
            trace_id=_optional(request.context.trace_id),
            tenant_id=_optional(request.context.tenant_id),
            operator_id=_optional(request.context.operator_id),
        ),
        goal=request.goal,
        max_steps=max_steps,
        run_type=_run_type_from_proto(request.run_type),
        turn_input_json=request.turn_input_json,
    )


async def _validate_correlated_run(request_run_id: str, context_run_id: str, context) -> None:
    normalized_request_run_id = request_run_id.strip()
    normalized_context_run_id = context_run_id.strip()
    if (
        not normalized_request_run_id
        or not normalized_context_run_id
        or request_run_id != normalized_request_run_id
        or context_run_id != normalized_context_run_id
        or request_run_id != context_run_id
    ):
        await context.abort(
            grpc.StatusCode.INVALID_ARGUMENT,
            "request.run_id must equal context.run_id",
        )


def _optional(value: str) -> str | None:
    normalized = value.strip()
    return normalized or None


def _run_proto(run: RuntimeRun) -> runtime_pb2.AgentRun:
    return runtime_pb2.AgentRun(
        run_id=run.run_id,
        goal=run.goal,
        status=_PROTO_STATUS[run.status],
        proposal_json=run.proposal_json,
        error_code=run.error_code,
        error_message=run.error_message,
        created_at_unix_ms=run.created_at_unix_ms,
        updated_at_unix_ms=run.updated_at_unix_ms,
        run_type=run.run_type,
        session_id=run.session_id,
        session_version=run.session_version,
        turn_result_json=run.turn_result_json,
    )


def _run_type_from_proto(value: int) -> RunType:
    if value == common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST:
        return RunType.LOT_DRAFT_ASSIST
    if value in (common_pb2.RUN_TYPE_UNSPECIFIED, common_pb2.RUN_TYPE_SESSION_PLAN):
        return RunType.SESSION_PLAN
    raise ValueError("unsupported run type")


def _event_proto(event: RuntimeEvent) -> runtime_pb2.AgentRunEvent:
    return runtime_pb2.AgentRunEvent(
        run_id=event.run_id,
        seq=event.seq,
        type=event.type,
        status=_PROTO_STATUS[event.status],
        payload_json=event.payload_json,
        created_at_unix_ms=event.created_at_unix_ms,
    )


def _metadata_pairs(metadata):
    for item in metadata:
        if hasattr(item, "key") and hasattr(item, "value"):
            yield str(item.key), str(item.value)
        else:
            key, value = item
            yield str(key), str(value)


_SAFE_EVENT_KEYS = frozenset(
    {"step", "status", "tool", "duration_bucket", "count"}
)
_SAFE_PROGRESS_EVENTS = frozenset(
    {
        "model.started",
        "model.completed",
        "tool.started",
        "tool.completed",
        "proposal.validated",
    }
)
_SAFE_TOOL_NAMES = frozenset(
    {"inventory.query", "auction.history.query", "session.snapshot.get"}
)
_SAFE_EVENT_STATUSES = frozenset({"started", "completed", "failed", "cancelled"})
_SAFE_DURATION_BUCKETS = frozenset({"lt_100ms", "lt_1s", "lt_10s", "gte_10s"})


def _safe_payload_json(event_type: str, payload: dict) -> str:
    if event_type not in _SAFE_PROGRESS_EVENTS:
        raise ValueError("unsafe runtime event type")
    if not isinstance(payload, dict) or not set(payload).issubset(_SAFE_EVENT_KEYS):
        raise ValueError("unsafe runtime event payload")
    if "status" in payload and payload["status"] not in _SAFE_EVENT_STATUSES:
        raise ValueError("unsafe runtime event payload")
    if "tool" in payload and payload["tool"] not in _SAFE_TOOL_NAMES:
        raise ValueError("unsafe runtime event payload")
    if (
        "duration_bucket" in payload
        and payload["duration_bucket"] not in _SAFE_DURATION_BUCKETS
    ):
        raise ValueError("unsafe runtime event payload")
    for key in ("step", "count"):
        if key in payload and (
            not isinstance(payload[key], int)
            or isinstance(payload[key], bool)
            or payload[key] < 0
        ):
            raise ValueError("unsafe runtime event payload")
    return json.dumps(payload, ensure_ascii=True, separators=(",", ":"), sort_keys=True)
