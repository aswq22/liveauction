from __future__ import annotations

import asyncio
from collections.abc import AsyncIterator
import json
from pathlib import Path

import grpc
import pytest
from langgraph.checkpoint.memory import InMemorySaver

from app.domain.models import (
    CandidatePatch,
    Observation,
    RequiredInput,
    RunContext,
    TurnResult,
)
from app.generated.proto.agent.v1 import common_pb2, runtime_pb2
from app.harness.context import AgentRunSummary, RunStatus, RunType
from app.harness.harness import AuctionAgentHarness
from app.models.fake import DeterministicPlanningModel
from app.runtime.repository import InMemoryRuntimeRepository, RunConflictError
from app.runtime.service import AgentRuntimeService
from app.skills.registry import SkillRegistry
from app.tools.registry import ToolRegistry


pytestmark = pytest.mark.anyio
TOKEN = "runtime-test-token"
AUTH = (("authorization", f"Bearer {TOKEN}"),)


@pytest.fixture
def anyio_backend():
    return "asyncio"


class RpcAbort(Exception):
    def __init__(self, code: grpc.StatusCode, details: str) -> None:
        super().__init__(details)
        self.code = code
        self.details = details


class FakeGrpcContext:
    def __init__(self, metadata=AUTH) -> None:
        self._metadata = tuple(metadata)

    def invocation_metadata(self):
        return self._metadata

    async def abort(self, code: grpc.StatusCode, details: str):
        raise RpcAbort(code, details)


class BlockingHarness:
    def __init__(self, *, result: AgentRunSummary | None = None) -> None:
        self.started = asyncio.Event()
        self.release = asyncio.Event()
        self.calls = 0
        self.inputs = []
        self.result = result or AgentRunSummary(
            run_id="run-1",
            status=RunStatus.COMPLETED,
            proposal=None,
            error_code="",
            error_message="",
        )

    async def start(self, run):
        self.calls += 1
        self.inputs.append(run)
        self.started.set()
        await self.release.wait()
        return self.result.model_copy(update={"run_id": run.context.run_id})


class ExplodingHarness:
    async def start(self, run):
        del run
        raise RuntimeError("database-password-and-prompt-must-not-leak")


class EmittingHarness:
    def __init__(self, sink) -> None:
        self._sink = sink

    async def start(self, run):
        await self._sink("model.started", {"step": 1, "status": "started"})
        await self._sink(
            "model.completed",
            {"step": 1, "status": "completed", "duration_bucket": "lt_100ms"},
        )
        return AgentRunSummary(
            run_id=run.context.run_id,
            status=RunStatus.FAILED,
            proposal=None,
            error_code="MODEL_ERROR",
            error_message="model decision failed",
        )


class PlanningToolClient:
    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        return Observation(
            tool="inventory.query",
            evidence={"lots": [{"id": "lot-1", "start_price": {"amount": 100}}]},
        )

    async def query_auction_history(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        return Observation(tool="auction.history.query", evidence={"items": []})

    async def get_session_snapshot(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        raise AssertionError("snapshot tool must not be called")


class GatedReadRepository(InMemoryRuntimeRepository):
    def __init__(self) -> None:
        super().__init__()
        self.read_started = asyncio.Event()
        self.release_read = asyncio.Event()

    async def list_events(self, run_id: str, after_seq: int):
        self.read_started.set()
        await self.release_read.wait()
        return await super().list_events(run_id, after_seq)


class PostReadGatedRepository(InMemoryRuntimeRepository):
    """Captures one replay result, then pauses before returning that snapshot."""

    def __init__(self) -> None:
        super().__init__()
        self.replay_captured = asyncio.Event()
        self.release_replay = asyncio.Event()
        self._gate_next_read = True

    async def list_events(self, run_id: str, after_seq: int):
        events = await super().list_events(run_id, after_seq)
        if self._gate_next_read:
            self._gate_next_read = False
            self.replay_captured.set()
            await self.release_replay.wait()
        return events


class FailingGetRepository(InMemoryRuntimeRepository):
    async def get_run(self, run_id: str):
        del run_id
        raise RuntimeError("postgresql://user:secret-password@private-host/database")


class OutOfOrderPublishRepository(InMemoryRuntimeRepository):
    def __init__(self) -> None:
        super().__init__()
        self.cancel_committed = asyncio.Event()
        self.release_cancel_return = asyncio.Event()

    async def request_cancel(self, run_id: str):
        result = await super().request_cancel(run_id)
        self.cancel_committed.set()
        await self.release_cancel_return.wait()
        return result


class CancelCreatedPublishBroadcaster:
    """Models RPC cancellation in the first await after the durable commit."""

    def __init__(self) -> None:
        self.cancelled_created_publish = False

    @property
    def count(self) -> int:
        return 0

    async def publish(self, event):
        if (
            event is not None
            and event.type == "run.created"
            and not self.cancelled_created_publish
        ):
            self.cancelled_created_publish = True
            raise asyncio.CancelledError


class GatedTerminalPublishBroadcaster:
    """Delegates all operations but pauses terminal notification publication."""

    def __init__(self, delegate) -> None:
        self._delegate = delegate
        self.terminal_publish_started = asyncio.Event()
        self.release_terminal_publish = asyncio.Event()

    @property
    def count(self) -> int:
        return self._delegate.count

    async def subscribe(self, run_id: str):
        return await self._delegate.subscribe(run_id)

    async def unsubscribe(self, subscription) -> None:
        await self._delegate.unsubscribe(subscription)

    async def publish(self, event) -> None:
        if event is not None and event.status in {
            RunStatus.COMPLETED,
            RunStatus.FAILED,
            RunStatus.CANCELLED,
        }:
            self.terminal_publish_started.set()
            await self.release_terminal_publish.wait()
        await self._delegate.publish(event)


class FailFirstCancelRepository(InMemoryRuntimeRepository):
    def __init__(self) -> None:
        super().__init__()
        self.failed = False

    async def request_cancel(self, run_id: str):
        if run_id == "run-1" and not self.failed:
            self.failed = True
            raise RuntimeError("transient repository cleanup failure")
        return await super().request_cancel(run_id)


def request_context(
    run_id: str = "run-1",
    *,
    request_id: str = "request-1",
    trace_id: str = "trace-1",
    tenant_id: str = "tenant-1",
    operator_id: str = "operator-1",
) -> common_pb2.RequestContext:
    return common_pb2.RequestContext(
        request_id=request_id,
        trace_id=trace_id,
        run_id=run_id,
        tenant_id=tenant_id,
        operator_id=operator_id,
    )


def start_request(
    *,
    run_id: str = "run-1",
    context_run_id: str | None = None,
    goal: str = "Plan lots",
    request_id: str = "request-1",
    trace_id: str = "trace-1",
    tenant_id: str = "tenant-1",
    operator_id: str = "operator-1",
) -> runtime_pb2.StartRunRequest:
    return runtime_pb2.StartRunRequest(
        run_id=run_id,
        context=request_context(
            context_run_id or run_id,
            request_id=request_id,
            trace_id=trace_id,
            tenant_id=tenant_id,
            operator_id=operator_id,
        ),
        goal=goal,
    )


async def wait_for_status(
    repository: InMemoryRuntimeRepository,
    run_id: str,
    status: RunStatus,
    *,
    attempts: int = 100,
):
    for _ in range(attempts):
        run = await repository.get_run(run_id)
        if run is not None and run.status is status:
            return run
        await asyncio.sleep(0)
    raise AssertionError(f"run {run_id} did not reach {status}")


async def collect_n(stream: AsyncIterator, count: int):
    return [await anext(stream) for _ in range(count)]


async def test_start_persists_created_then_returns_before_harness_completes():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )

    reply = await service.StartRun(start_request(), FakeGrpcContext())

    assert reply.run.run_id == "run-1"
    assert reply.run.status == common_pb2.RUN_STATUS_CREATED
    await harness.started.wait()
    persisted = await repository.get_run("run-1")
    assert persisted is not None
    assert persisted.status is RunStatus.PLANNING
    harness.release.set()
    await wait_for_status(repository, "run-1", RunStatus.COMPLETED)
    await service.shutdown()


@pytest.mark.parametrize(
    ("run_type", "session_id", "session_version", "turn_input_json"),
    [
        (
            common_pb2.RUN_TYPE_SESSION_PLAN,
            "session-plan-1",
            7,
            '{"message":"plan lots"}',
        ),
        (
            common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST,
            "session-draft-1",
            8,
            '{"candidate":"lot-1"}',
        ),
    ],
)
async def test_turn_contract_echoes_session_fields_from_start_request(
    run_type: int, session_id: str, session_version: int, turn_input_json: str
):
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )

    request = runtime_pb2.StartRunRequest(
        run_id="run-1",
        context=request_context("run-1"),
        goal="Plan this session turn",
        run_type=run_type,
        session_id=session_id,
        session_version=session_version,
        turn_input_json=turn_input_json,
    )
    decoded = runtime_pb2.StartRunRequest.FromString(request.SerializeToString())
    assert decoded.run_type == run_type
    assert decoded.session_id == session_id
    assert decoded.session_version == session_version
    assert decoded.turn_input_json == turn_input_json

    reply = await service.StartRun(request, FakeGrpcContext())

    assert reply.run.run_type == run_type
    assert reply.run.session_id == session_id
    assert reply.run.session_version == session_version
    await harness.started.wait()
    harness.release.set()
    await service.shutdown()


async def test_session_turn_is_durable_executes_typed_input_and_projects_result():
    repository = InMemoryRuntimeRepository()
    turn_result = TurnResult(
        assistant_message="Draft ready",
        candidate_patches=[
            CandidatePatch(
                candidate_id="candidate-1",
                source_type="NEW",
                fields={"title": "Tea set"},
                field_sources={"title": "AI"},
            )
        ],
        required_inputs=[
            RequiredInput.START_PRICE,
            RequiredInput.DURATION,
            RequiredInput.MAIN_IMAGE,
        ],
        review_readiness="NEEDS_INPUT",
        warnings=[],
    )
    harness = BlockingHarness(
        result=AgentRunSummary(
            run_id="run-session",
            status=RunStatus.COMPLETED,
            proposal=None,
            error_code="",
            error_message="",
            turn_result=turn_result,
        )
    )
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    turn_input_json = '{"message":"draft a tea set","candidate_patches":[]}'

    started = await service.StartRun(
        runtime_pb2.StartRunRequest(
            run_id="run-session",
            context=request_context("run-session"),
            goal="Continue agent session",
            run_type=common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST,
            session_id="session-1",
            session_version=7,
            turn_input_json=turn_input_json,
        ),
        FakeGrpcContext(),
    )

    assert started.run.session_id == "session-1"
    durable = await repository.get_run("run-session")
    assert durable is not None
    assert durable.run_type == common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST
    assert durable.session_id == "session-1"
    assert durable.session_version == 7
    assert durable.turn_input_json == turn_input_json
    await harness.started.wait()
    assert harness.inputs[0].run_type is RunType.LOT_DRAFT_ASSIST
    assert harness.inputs[0].turn_input_json == turn_input_json
    harness.release.set()
    completed = await wait_for_status(repository, "run-session", RunStatus.COMPLETED)

    assert completed.turn_result_json == turn_result.model_dump_json()
    reply = await service.GetRun(
        runtime_pb2.GetRunRequest(
            run_id="run-session", context=request_context("run-session")
        ),
        FakeGrpcContext(),
    )
    assert reply.run.run_type == common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST
    assert reply.run.session_id == "session-1"
    assert reply.run.session_version == 7
    assert reply.run.turn_result_json == turn_result.model_dump_json()
    await service.shutdown()


async def test_session_turn_duplicate_changed_input_conflicts_and_exact_replay_runs_once():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    request = runtime_pb2.StartRunRequest(
        run_id="run-session",
        context=request_context("run-session"),
        goal="Continue agent session",
        run_type=common_pb2.RUN_TYPE_SESSION_PLAN,
        session_id="session-1",
        session_version=3,
        turn_input_json='{"message":"plan lots"}',
    )

    first = await service.StartRun(request, FakeGrpcContext())
    replay = await service.StartRun(request, FakeGrpcContext())

    assert replay.run.created_at_unix_ms == first.run.created_at_unix_ms
    await harness.started.wait()
    assert harness.calls == 1
    changed = runtime_pb2.StartRunRequest()
    changed.CopyFrom(request)
    changed.turn_input_json = '{"message":"changed"}'
    with pytest.raises(RpcAbort) as conflict:
        await service.StartRun(changed, FakeGrpcContext())
    assert conflict.value.code is grpc.StatusCode.ALREADY_EXISTS
    harness.release.set()
    await service.shutdown()


async def test_failed_session_turn_never_persists_partial_turn_result():
    repository = InMemoryRuntimeRepository()
    partial = TurnResult(
        assistant_message="partial",
        candidate_patches=[],
        required_inputs=[],
        review_readiness="READY_FOR_REVIEW",
        warnings=[],
    )
    harness = BlockingHarness(
        result=AgentRunSummary(
            run_id="run-session",
            status=RunStatus.FAILED,
            proposal=None,
            error_code="MODEL_ERROR",
            error_message="model failed",
            turn_result=partial,
        )
    )
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(
        runtime_pb2.StartRunRequest(
            run_id="run-session",
            context=request_context("run-session"),
            goal="Continue agent session",
            run_type=common_pb2.RUN_TYPE_SESSION_PLAN,
            session_id="session-1",
            session_version=3,
            turn_input_json='{"message":"plan lots"}',
        ),
        FakeGrpcContext(),
    )
    await harness.started.wait()
    harness.release.set()

    failed = await wait_for_status(repository, "run-session", RunStatus.FAILED)
    assert failed.turn_result_json == ""
    reply = await service.GetRun(
        runtime_pb2.GetRunRequest(
            run_id="run-session", context=request_context("run-session")
        ),
        FakeGrpcContext(),
    )
    assert reply.run.turn_result_json == ""
    await service.shutdown()


async def test_completed_session_turn_without_result_is_failed_safely():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness(
        result=AgentRunSummary(
            run_id="run-session", status=RunStatus.COMPLETED, proposal=None,
            error_code="", error_message="", turn_result=None,
        )
    )
    service = AgentRuntimeService(
        repository=repository, harness_factory=lambda parent_context: harness,
        internal_token=TOKEN, max_steps=4,
    )
    await service.StartRun(
        runtime_pb2.StartRunRequest(
            run_id="run-session", context=request_context("run-session"),
            goal="Continue agent session", run_type=common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST,
            session_id="session-1", session_version=3,
            turn_input_json='{"message":"draft lots"}',
        ),
        FakeGrpcContext(),
    )
    await harness.started.wait()
    harness.release.set()

    failed = await wait_for_status(repository, "run-session", RunStatus.FAILED)
    assert failed.error_code == "INVALID_TURN_RESULT"
    assert failed.turn_result_json == ""
    await service.shutdown()


async def test_legacy_goal_starts_existing_read_only_planning():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )

    reply = await service.StartRun(
        start_request(goal="Plan existing lots"), FakeGrpcContext()
    )

    assert reply.run.goal == "Plan existing lots"
    assert reply.run.run_type == common_pb2.RUN_TYPE_UNSPECIFIED
    await harness.started.wait()
    harness.release.set()
    await service.shutdown()


async def test_duplicate_start_is_idempotent_but_changed_input_conflicts():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    context = FakeGrpcContext()

    first = await service.StartRun(start_request(), context)
    duplicate = await service.StartRun(start_request(), context)

    assert duplicate.run.created_at_unix_ms == first.run.created_at_unix_ms
    await harness.started.wait()
    assert harness.calls == 1
    events = await repository.list_events("run-1", 0)
    assert [event.type for event in events].count("run.created") == 1

    with pytest.raises(RpcAbort) as conflict:
        await service.StartRun(start_request(goal="A different goal"), context)
    assert conflict.value.code is grpc.StatusCode.ALREADY_EXISTS
    assert conflict.value.details == "run ID conflicts with an existing run"
    harness.release.set()
    await service.shutdown()


async def test_reconciled_start_allows_new_request_id_and_executes_once():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )

    first = await service.StartRun(
        start_request(request_id="req-original"), FakeGrpcContext()
    )
    replay = await service.StartRun(
        start_request(request_id="req-reconcile"), FakeGrpcContext()
    )

    assert replay.run.run_id == first.run.run_id == "run-1"
    assert replay.run.created_at_unix_ms == first.run.created_at_unix_ms
    await asyncio.wait_for(harness.started.wait(), timeout=0.5)
    assert harness.calls == 1
    persisted = await repository.get_run("run-1")
    assert persisted is not None
    assert persisted.context.request_id == "req-original"
    events = await repository.list_events("run-1", 0)
    assert [event.type for event in events].count("run.created") == 1
    harness.release.set()
    await service.shutdown()


@pytest.mark.parametrize(
    "changed",
    [
        {"trace_id": "trace-changed"},
        {"tenant_id": "tenant-changed"},
        {"operator_id": "operator-changed"},
        {"goal": "Changed goal"},
    ],
    ids=["trace", "tenant", "operator", "goal"],
)
async def test_reconciled_start_still_rejects_changed_durable_spec(changed):
    repository = InMemoryRuntimeRepository()
    await repository.create_run(
        context=RunContext(
            run_id="run-1",
            request_id="req-original",
            trace_id="trace-1",
            tenant_id="tenant-1",
            operator_id="operator-1",
        ),
        goal="Plan lots",
        max_steps=4,
    )
    context = RunContext(
        run_id="run-1",
        request_id="req-reconcile",
        trace_id=changed.get("trace_id", "trace-1"),
        tenant_id=changed.get("tenant_id", "tenant-1"),
        operator_id=changed.get("operator_id", "operator-1"),
    )

    with pytest.raises(RunConflictError):
        await repository.create_run(
            context=context,
            goal=changed.get("goal", "Plan lots"),
            max_steps=4,
        )


async def test_reconciled_start_still_rejects_changed_max_steps():
    repository = InMemoryRuntimeRepository()
    context = RunContext(run_id="run-1", request_id="req-original")
    await repository.create_run(context=context, goal="Plan lots", max_steps=4)

    with pytest.raises(RunConflictError):
        await repository.create_run(
            context=context.model_copy(update={"request_id": "req-reconcile"}),
            goal="Plan lots",
            max_steps=5,
        )


async def test_start_registers_before_cancellable_publish_and_retry_is_exactly_once():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    service._broadcaster = CancelCreatedPublishBroadcaster()

    with pytest.raises(asyncio.CancelledError):
        await service.StartRun(start_request(), FakeGrpcContext())

    retry = await service.StartRun(start_request(), FakeGrpcContext())
    await asyncio.wait_for(harness.started.wait(), timeout=0.5)
    assert retry.run.run_id == "run-1"
    assert harness.calls == 1
    assert service.tracked_task_count == 1
    harness.release.set()
    await service.shutdown()


async def test_identical_retry_recovers_untracked_nonterminal_run_once():
    repository = InMemoryRuntimeRepository()
    await repository.create_run(
        context=RunContext(
            run_id="run-1",
            request_id="request-1",
            trace_id="trace-1",
            tenant_id="tenant-1",
            operator_id="operator-1",
        ),
        goal="Plan lots",
        max_steps=4,
    )
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )

    await asyncio.gather(
        service.StartRun(start_request(), FakeGrpcContext()),
        service.StartRun(start_request(), FakeGrpcContext()),
    )

    await asyncio.wait_for(harness.started.wait(), timeout=0.5)
    assert harness.calls == 1
    assert service.tracked_task_count == 1
    harness.release.set()
    await service.shutdown()


@pytest.mark.parametrize(
    "metadata",
    [
        (),
        (("authorization", "Bearer wrong-token"),),
        (("traceparent", "00-11111111111111111111111111111111-2222222222222222-01"),),
        (("authorization", f"bearer {TOKEN}"),),
    ],
)
async def test_runtime_requires_exact_bearer_authorization(metadata):
    service = AgentRuntimeService(
        repository=InMemoryRuntimeRepository(),
        harness_factory=lambda parent_context: BlockingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )

    with pytest.raises(RpcAbort) as aborted:
        await service.StartRun(start_request(), FakeGrpcContext(metadata))

    assert aborted.value.code is grpc.StatusCode.UNAUTHENTICATED
    assert aborted.value.details == "invalid internal authorization"
    await service.shutdown()


async def test_start_rejects_mismatched_go_generated_run_ids():
    service = AgentRuntimeService(
        repository=InMemoryRuntimeRepository(),
        harness_factory=lambda parent_context: BlockingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )

    with pytest.raises(RpcAbort) as aborted:
        await service.StartRun(
            start_request(run_id="run-1", context_run_id="run-other"), FakeGrpcContext()
        )

    assert aborted.value.code is grpc.StatusCode.INVALID_ARGUMENT
    assert aborted.value.details == "request.run_id must equal context.run_id"
    await service.shutdown()


async def test_get_missing_run_maps_to_stable_not_found():
    service = AgentRuntimeService(
        repository=InMemoryRuntimeRepository(),
        harness_factory=lambda parent_context: BlockingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )
    request = runtime_pb2.GetRunRequest(context=request_context(), run_id="run-1")

    with pytest.raises(RpcAbort) as aborted:
        await service.GetRun(request, FakeGrpcContext())

    assert aborted.value.code is grpc.StatusCode.NOT_FOUND
    assert aborted.value.details == "run not found"
    await service.shutdown()


async def test_get_repository_failure_maps_to_safe_internal_status():
    service = AgentRuntimeService(
        repository=FailingGetRepository(),
        harness_factory=lambda parent_context: BlockingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )
    request = runtime_pb2.GetRunRequest(context=request_context(), run_id="run-1")

    with pytest.raises(RpcAbort) as aborted:
        await service.GetRun(request, FakeGrpcContext())

    assert aborted.value.code is grpc.StatusCode.INTERNAL
    assert aborted.value.details == "runtime operation failed"
    assert "secret-password" not in aborted.value.details
    await service.shutdown()


async def test_lookup_rejects_noncanonical_run_ids_instead_of_normalizing_them():
    service = AgentRuntimeService(
        repository=InMemoryRuntimeRepository(),
        harness_factory=lambda parent_context: BlockingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )
    request = runtime_pb2.GetRunRequest(
        context=request_context(" run-1 "), run_id=" run-1 "
    )

    with pytest.raises(RpcAbort) as aborted:
        await service.GetRun(request, FakeGrpcContext())

    assert aborted.value.code is grpc.StatusCode.INVALID_ARGUMENT
    assert aborted.value.details == "request.run_id must equal context.run_id"
    await service.shutdown()


async def test_cancel_is_durable_idempotent_and_finishes_cancelled():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness(
        result=AgentRunSummary(
            run_id="run-1",
            status=RunStatus.CANCELLED,
            proposal=None,
            error_code="RUN_CANCELLED",
            error_message="run cancelled",
        )
    )
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    request = runtime_pb2.CancelRunRequest(context=request_context(), run_id="run-1")

    first = await service.CancelRun(request, FakeGrpcContext())
    second = await service.CancelRun(request, FakeGrpcContext())

    assert first.run.status == common_pb2.RUN_STATUS_PLANNING
    assert second.run.status == common_pb2.RUN_STATUS_PLANNING
    assert await repository.is_cancelled("run-1") is True
    events = await repository.list_events("run-1", 0)
    assert [event.type for event in events].count("run.cancel_requested") == 1
    harness.release.set()
    terminal = await wait_for_status(repository, "run-1", RunStatus.CANCELLED)
    assert terminal.error_code == "RUN_CANCELLED"
    await service.shutdown()


async def test_background_exception_is_observed_and_persisted_without_detail_leak():
    repository = InMemoryRuntimeRepository()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: ExplodingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )

    await service.StartRun(start_request(), FakeGrpcContext())
    failed = await wait_for_status(repository, "run-1", RunStatus.FAILED)

    assert failed.error_code == "INTERNAL_RUNTIME_ERROR"
    assert failed.error_message == "agent runtime execution failed"
    assert "password" not in failed.error_message
    assert service.tracked_task_count == 0
    await service.shutdown()


async def test_runtime_persists_injected_safe_progress_events_before_terminal():
    repository = InMemoryRuntimeRepository()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context, sink: EmittingHarness(sink),
        internal_token=TOKEN,
        max_steps=4,
    )

    await service.StartRun(start_request(), FakeGrpcContext())
    await wait_for_status(repository, "run-1", RunStatus.FAILED)

    events = await repository.list_events("run-1", 0)
    assert [event.type for event in events] == [
        "run.created",
        "run.planning",
        "model.started",
        "model.completed",
        "run.failed",
    ]
    assert [event.seq for event in events] == [1, 2, 3, 4, 5]
    assert "Plan lots" not in "".join(event.payload_json for event in events)
    await service.shutdown()


async def test_real_mock_planning_persists_ui_consumable_tool_trajectory():
    repository = InMemoryRuntimeRepository()
    skills = SkillRegistry(Path(__file__).parents[2] / "app" / "skills" / "packages")

    def harness_factory(parent_context, sink):
        del parent_context
        return AuctionAgentHarness(
            model=DeterministicPlanningModel(),
            tools=ToolRegistry(PlanningToolClient()),
            skills=skills,
            checkpointer=InMemorySaver(),
            event_sink=sink,
            token_budget=10000,
        )

    service = AgentRuntimeService(
        repository=repository,
        harness_factory=harness_factory,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await asyncio.wait_for(asyncio.shield(service._tasks["run-1"]), timeout=2)
    completed = await repository.get_run("run-1")
    assert completed is not None and completed.status is RunStatus.COMPLETED

    events = await repository.list_events("run-1", 0)
    types = [event.type for event in events]
    assert types.index("tool.started") < types.index("tool.completed")
    assert types[-2:] == ["proposal.validated", "run.completed"]
    for event in events:
        payload = json.loads(event.payload_json)
        if event.type in {
            "model.started",
            "model.completed",
            "tool.started",
            "tool.completed",
            "proposal.validated",
        }:
            assert set(payload) <= {
                "step",
                "status",
                "tool",
                "duration_bucket",
                "count",
            }
    await service.shutdown()


async def test_global_run_limit_is_atomic_idempotent_and_releases():
    repository = InMemoryRuntimeRepository()
    harnesses: list[BlockingHarness] = []

    def factory(parent_context):
        del parent_context
        harness = BlockingHarness()
        harnesses.append(harness)
        return harness

    service = AgentRuntimeService(
        repository=repository,
        harness_factory=factory,
        internal_token=TOKEN,
        max_steps=4,
        max_active_runs_global=1,
        max_active_runs_per_tenant=1,
    )

    first = await service.StartRun(start_request(run_id="run-1"), FakeGrpcContext())
    duplicate = await service.StartRun(start_request(run_id="run-1"), FakeGrpcContext())
    assert duplicate.run.created_at_unix_ms == first.run.created_at_unix_ms
    with pytest.raises(RpcAbort) as exhausted:
        await service.StartRun(start_request(run_id="run-2"), FakeGrpcContext())
    assert exhausted.value.code is grpc.StatusCode.RESOURCE_EXHAUSTED
    while not harnesses:
        await asyncio.sleep(0)
    await harnesses[0].started.wait()
    harnesses[0].release.set()
    await wait_for_status(repository, "run-1", RunStatus.COMPLETED)
    await service.StartRun(start_request(run_id="run-2"), FakeGrpcContext())
    while len(harnesses) < 2:
        await asyncio.sleep(0)
    await harnesses[1].started.wait()
    harnesses[1].release.set()
    await service.shutdown()


async def test_per_tenant_run_limit_isolated_between_tenants():
    repository = InMemoryRuntimeRepository()
    harnesses: list[BlockingHarness] = []

    def factory(parent_context):
        del parent_context
        harness = BlockingHarness()
        harnesses.append(harness)
        return harness

    service = AgentRuntimeService(
        repository=repository,
        harness_factory=factory,
        internal_token=TOKEN,
        max_steps=4,
        max_active_runs_global=3,
        max_active_runs_per_tenant=1,
    )
    await service.StartRun(
        start_request(run_id="tenant-a-1", tenant_id="tenant-a"), FakeGrpcContext()
    )
    with pytest.raises(RpcAbort) as exhausted:
        await service.StartRun(
            start_request(run_id="tenant-a-2", tenant_id="tenant-a"),
            FakeGrpcContext(),
        )
    assert exhausted.value.code is grpc.StatusCode.RESOURCE_EXHAUSTED
    await service.StartRun(
        start_request(run_id="tenant-b-1", tenant_id="tenant-b"), FakeGrpcContext()
    )
    while len(harnesses) < 2:
        await asyncio.sleep(0)
    for harness in harnesses:
        harness.release.set()
    await service.shutdown()


async def test_subscriber_limits_reject_races_and_release_after_disconnect():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
        max_subscribers_global=1,
        max_subscribers_per_run=1,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    request = runtime_pb2.StreamRunEventsRequest(
        context=request_context(), run_id="run-1", after_seq=2
    )
    first = service.StreamRunEvents(request, FakeGrpcContext())
    pending = asyncio.create_task(anext(first))
    while service.subscriber_count == 0:
        await asyncio.sleep(0)
    second = service.StreamRunEvents(request, FakeGrpcContext())
    with pytest.raises(RpcAbort) as exhausted:
        await anext(second)
    assert exhausted.value.code is grpc.StatusCode.RESOURCE_EXHAUSTED
    pending.cancel()
    await asyncio.gather(pending, return_exceptions=True)
    assert service.subscriber_count == 0
    replacement = service.StreamRunEvents(request, FakeGrpcContext())
    replacement_pending = asyncio.create_task(anext(replacement))
    while service.subscriber_count == 0:
        await asyncio.sleep(0)
    replacement_pending.cancel()
    await asyncio.gather(replacement_pending, return_exceptions=True)
    harness.release.set()
    await service.shutdown()


async def test_stream_replays_then_delivers_live_events_in_strict_sequence():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    stream_request = runtime_pb2.StreamRunEventsRequest(
        context=request_context(), run_id="run-1", after_seq=0
    )
    stream = service.StreamRunEvents(stream_request, FakeGrpcContext())

    replay = await collect_n(stream, 2)
    cancel_request = runtime_pb2.CancelRunRequest(context=request_context(), run_id="run-1")
    await service.CancelRun(cancel_request, FakeGrpcContext())
    live = await anext(stream)

    assert [event.seq for event in [*replay, live]] == [1, 2, 3]
    assert live.type == "run.cancel_requested"
    await stream.aclose()
    assert service.subscriber_count == 0
    assert service.tracked_task_count == 1
    harness.release.set()
    await service.shutdown()


async def test_stream_subscribes_before_replay_and_deduplicates_handoff():
    repository = GatedReadRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    stream = service.StreamRunEvents(
        runtime_pb2.StreamRunEventsRequest(
            context=request_context(), run_id="run-1", after_seq=1
        ),
        FakeGrpcContext(),
    )
    pending = asyncio.create_task(anext(stream))
    await repository.read_started.wait()
    await service.CancelRun(
        runtime_pb2.CancelRunRequest(context=request_context(), run_id="run-1"),
        FakeGrpcContext(),
    )
    repository.release_read.set()

    first = await pending
    second = await anext(stream)

    assert [first.seq, second.seq] == [2, 3]
    await stream.aclose()
    harness.release.set()
    await service.shutdown()


async def test_live_stream_recovers_commit_order_when_publish_notifications_reorder():
    repository = OutOfOrderPublishRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    stream = service.StreamRunEvents(
        runtime_pb2.StreamRunEventsRequest(
            context=request_context(), run_id="run-1", after_seq=2
        ),
        FakeGrpcContext(),
    )
    first_live = asyncio.create_task(anext(stream))
    while service.subscriber_count == 0:
        await asyncio.sleep(0)
    cancel_task = asyncio.create_task(
        service.CancelRun(
            runtime_pb2.CancelRunRequest(context=request_context(), run_id="run-1"),
            FakeGrpcContext(),
        )
    )
    await repository.cancel_committed.wait()
    harness.release.set()
    await wait_for_status(repository, "run-1", RunStatus.CANCELLED)

    first = await first_live
    second = await anext(stream)

    assert [first.seq, second.seq] == [3, 4]
    repository.release_cancel_return.set()
    await cancel_task
    await stream.aclose()
    await service.shutdown()


@pytest.mark.parametrize("offset", [0, 10], ids=["at-terminal", "beyond-terminal"])
async def test_terminal_reconnect_without_newer_events_returns_promptly(offset: int):
    repository = InMemoryRuntimeRepository()
    created = await repository.create_run(
        context=RunContext(run_id="run-1"), goal="Plan lots", max_steps=4
    )
    completed = await repository.transition_run(
        "run-1", status=RunStatus.COMPLETED, type="run.completed"
    )
    assert created.event is not None
    assert completed.event is not None
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: BlockingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )
    stream = service.StreamRunEvents(
        runtime_pb2.StreamRunEventsRequest(
            context=request_context(),
            run_id="run-1",
            after_seq=completed.event.seq + offset,
        ),
        FakeGrpcContext(),
    )

    with pytest.raises(StopAsyncIteration):
        await asyncio.wait_for(anext(stream), timeout=0.5)

    assert service.subscriber_count == 0
    await service.shutdown()


async def test_terminal_handoff_beyond_after_seq_drains_notification_and_returns():
    repository = GatedReadRepository()
    await repository.create_run(
        context=RunContext(run_id="run-1"), goal="Plan lots", max_steps=4
    )
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: BlockingHarness(),
        internal_token=TOKEN,
        max_steps=4,
    )
    stream = service.StreamRunEvents(
        runtime_pb2.StreamRunEventsRequest(
            context=request_context(), run_id="run-1", after_seq=100
        ),
        FakeGrpcContext(),
    )
    next_event = asyncio.create_task(anext(stream))
    await repository.read_started.wait()
    terminal = await repository.transition_run(
        "run-1", status=RunStatus.COMPLETED, type="run.completed"
    )
    await service._broadcaster.publish(terminal.event)
    repository.release_read.set()

    with pytest.raises(StopAsyncIteration):
        await asyncio.wait_for(next_event, timeout=0.5)

    assert service.subscriber_count == 0
    await service.shutdown()


async def test_terminal_commit_after_replay_snapshot_is_caught_up_before_notification():
    repository = PostReadGatedRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    broadcaster = GatedTerminalPublishBroadcaster(service._broadcaster)
    service._broadcaster = broadcaster
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    stream = service.StreamRunEvents(
        runtime_pb2.StreamRunEventsRequest(
            context=request_context(), run_id="run-1", after_seq=2
        ),
        FakeGrpcContext(),
    )
    pending = asyncio.create_task(anext(stream))
    await repository.replay_captured.wait()

    harness.release.set()
    await broadcaster.terminal_publish_started.wait()
    committed = await repository.get_run("run-1")
    assert committed is not None and committed.status is RunStatus.COMPLETED
    repository.release_replay.set()

    try:
        terminal = await asyncio.wait_for(pending, timeout=0.5)
        assert terminal.seq == 3
        assert terminal.type == "run.completed"
        assert terminal.status == common_pb2.RUN_STATUS_COMPLETED
        with pytest.raises(StopAsyncIteration):
            await asyncio.wait_for(anext(stream), timeout=0.5)
    finally:
        broadcaster.release_terminal_publish.set()
        await service.shutdown()


async def test_reconnect_after_seq_replays_only_newer_events():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    await service.CancelRun(
        runtime_pb2.CancelRunRequest(context=request_context(), run_id="run-1"),
        FakeGrpcContext(),
    )
    stream = service.StreamRunEvents(
        runtime_pb2.StreamRunEventsRequest(
            context=request_context(), run_id="run-1", after_seq=2
        ),
        FakeGrpcContext(),
    )

    event = await anext(stream)

    assert event.seq == 3
    await stream.aclose()
    harness.release.set()
    await service.shutdown()


async def test_slow_stream_is_bounded_and_aborts_for_safe_reconnect():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
        subscriber_queue_size=1,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()
    context = FakeGrpcContext()
    stream = service.StreamRunEvents(
        runtime_pb2.StreamRunEventsRequest(
            context=request_context(), run_id="run-1", after_seq=0
        ),
        context,
    )

    first = await anext(stream)
    await service.CancelRun(
        runtime_pb2.CancelRunRequest(context=request_context(), run_id="run-1"),
        FakeGrpcContext(),
    )
    harness.release.set()
    await wait_for_status(repository, "run-1", RunStatus.CANCELLED)
    second = await anext(stream)
    assert [first.seq, second.seq] == [1, 2]
    with pytest.raises(RpcAbort) as aborted:
        await anext(stream)
    assert aborted.value.code is grpc.StatusCode.RESOURCE_EXHAUSTED
    assert aborted.value.details == "event subscriber fell behind; reconnect with after_seq"
    assert service.subscriber_count == 0
    await service.shutdown()


async def test_shutdown_cancels_awaits_and_persists_tracked_run():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())
    await harness.started.wait()

    await service.shutdown()

    run = await repository.get_run("run-1")
    assert run is not None
    assert run.status is RunStatus.CANCELLED
    assert run.error_code == "RUN_CANCELLED"
    assert service.tracked_task_count == 0


async def test_immediate_shutdown_terminalizes_task_cancelled_before_first_instruction():
    repository = InMemoryRuntimeRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(), FakeGrpcContext())

    await service.shutdown()

    run = await repository.get_run("run-1")
    assert run is not None
    assert run.status is RunStatus.CANCELLED
    assert run.error_code == "RUN_CANCELLED"
    assert service.tracked_task_count == 0


async def test_shutdown_continues_after_one_repository_cleanup_failure():
    repository = FailFirstCancelRepository()
    harness = BlockingHarness()
    service = AgentRuntimeService(
        repository=repository,
        harness_factory=lambda parent_context: harness,
        internal_token=TOKEN,
        max_steps=4,
    )
    await service.StartRun(start_request(run_id="run-1"), FakeGrpcContext())
    await service.StartRun(start_request(run_id="run-2"), FakeGrpcContext())

    try:
        await service.shutdown()
    finally:
        harness.release.set()
        await service.shutdown()

    first = await repository.get_run("run-1")
    second = await repository.get_run("run-2")
    assert first is not None and first.status is RunStatus.CANCELLED
    assert second is not None and second.status is RunStatus.CANCELLED
    assert service.tracked_task_count == 0


async def test_cancel_committed_before_completion_atomically_wins():
    repository = InMemoryRuntimeRepository()
    await repository.create_run(
        context=RunContext(run_id="run-1"), goal="Plan lots", max_steps=4
    )
    await repository.request_cancel("run-1")

    completed = await repository.transition_run(
        "run-1",
        status=RunStatus.COMPLETED,
        type="run.completed",
        proposal_json='{"should":"not persist"}',
    )

    assert completed.run.status is RunStatus.CANCELLED
    assert completed.run.proposal_json == ""
    assert completed.run.error_code == "RUN_CANCELLED"
    assert completed.event is not None
    assert completed.event.type == "run.cancelled"
    assert completed.event.status is RunStatus.CANCELLED


async def test_terminal_commit_before_cancel_is_not_semantically_rewritten():
    repository = InMemoryRuntimeRepository()
    await repository.create_run(
        context=RunContext(run_id="run-1"), goal="Plan lots", max_steps=4
    )
    completed = await repository.transition_run(
        "run-1", status=RunStatus.COMPLETED, type="run.completed"
    )

    cancellation = await repository.request_cancel("run-1")

    assert cancellation.event is None
    assert cancellation.run == completed.run
    assert cancellation.run.status is RunStatus.COMPLETED
    assert cancellation.run.cancel_requested is False


async def test_in_memory_event_sequences_are_unique_under_concurrency():
    repository = InMemoryRuntimeRepository()
    await repository.create_run(
        context=RunContext(run_id="run-1"), goal="Plan lots", max_steps=4
    )

    events = await asyncio.gather(
        *[
            repository.append_event(
                "run-1", type=f"concurrent.{index}", status=RunStatus.PLANNING
            )
            for index in range(50)
        ]
    )

    assert sorted(event.seq for event in events) == list(range(2, 52))
    persisted = await repository.list_events("run-1", 0)
    assert [event.seq for event in persisted] == list(range(1, 52))
