from __future__ import annotations

import asyncio
import os
from pathlib import Path
import sys
from uuid import uuid4

import pytest

if sys.platform == "win32":
    asyncio.set_event_loop_policy(asyncio.WindowsSelectorEventLoopPolicy())

from app.domain.models import (
    EvidenceRef,
    FinalProposalDecision,
    LotPlan,
    Observation,
    RunContext,
    SessionProposal,
    ToolCallDecision,
)
from app.generated.proto.agent.v1 import common_pb2
from app.harness.context import RunInput, RunStatus
from app.harness.harness import AuctionAgentHarness
from app.models.fake import FakeModel
from app.runtime.repository import PostgresRuntimeRepository, RunConflictError
from app.skills.registry import SkillRegistry
from app.tools.registry import ToolRegistry


pytestmark = [pytest.mark.anyio, pytest.mark.postgres]
POSTGRES_DSN = os.getenv("AGENT_TEST_POSTGRES_DSN")


@pytest.fixture
def anyio_backend():
    return "asyncio"


@pytest.fixture
async def repository():
    if not POSTGRES_DSN:
        pytest.skip("AGENT_TEST_POSTGRES_DSN is not configured")
    instance = PostgresRuntimeRepository(POSTGRES_DSN)
    await instance.setup()
    try:
        yield instance
    finally:
        await instance.close()


class InventoryToolClient:
    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        return Observation(tool="inventory.query", evidence={"lots": [{"id": "lot-1"}]})

    async def query_auction_history(self, arguments, run_context, *, timeout):
        raise AssertionError("history tool must not be called")

    async def get_session_snapshot(self, arguments, run_context, *, timeout):
        raise AssertionError("snapshot tool must not be called")


def proposal() -> SessionProposal:
    return SessionProposal(
        summary="Postgres checkpoint proposal",
        strategy="balanced",
        estimated_duration_minutes=5,
        lots=[
            LotPlan(
                lot_id="lot-1",
                sequence=1,
                role="opening",
                start_price_amount=0,
                increment_amount=0,
                duration_seconds=60,
                extension_seconds=10,
                reason="Grounded in inventory",
            )
        ],
        evidence=[
            EvidenceRef(
                tool="inventory.query",
                reference_id="lot-1",
                summary="Inventory query returned the lot",
            )
        ],
        risks=["Integration fixture has no history"],
    )


async def test_postgres_sequence_allocation_is_unique_under_concurrency(repository):
    run_id = f"run-pg-seq-{uuid4()}"
    created = await repository.create_run(
        context=RunContext(run_id=run_id), goal="Concurrent events", max_steps=4
    )
    assert created.created is True

    events = await asyncio.gather(
        *[
            repository.append_event(
                run_id, type=f"concurrent.{index}", status=RunStatus.PLANNING
            )
            for index in range(40)
        ]
    )

    assert sorted(event.seq for event in events) == list(range(2, 42))
    replay = await repository.list_events(run_id, 0)
    assert [event.seq for event in replay] == list(range(1, 42))


async def test_postgres_duplicate_run_is_idempotent_and_conflict_is_stable(repository):
    run_id = f"run-pg-duplicate-{uuid4()}"
    context = RunContext(run_id=run_id, tenant_id="tenant-1")

    first = await repository.create_run(context=context, goal="Plan", max_steps=4)
    duplicate = await repository.create_run(context=context, goal="Plan", max_steps=4)

    assert first.created is True
    assert duplicate.created is False
    assert duplicate.run.created_at_unix_ms == first.run.created_at_unix_ms
    with pytest.raises(RunConflictError):
        await repository.create_run(context=context, goal="Changed", max_steps=4)


async def test_postgres_session_turn_round_trips_input_and_terminal_result(repository):
    run_id = f"run-pg-session-{uuid4()}"
    context = RunContext(
        run_id=run_id,
        trace_id="trace-session",
        tenant_id="tenant-session",
        operator_id="operator-session",
    )
    turn_input_json = '{"message":"draft a lot","candidate_patches":[]}'
    created = await repository.create_run(
        context=context,
        goal="Continue agent session",
        max_steps=4,
        run_type=common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST,
        session_id="session-1",
        session_version=7,
        turn_input_json=turn_input_json,
    )

    assert created.run.run_type == common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST
    assert created.run.session_id == "session-1"
    assert created.run.session_version == 7
    assert created.run.turn_input_json == turn_input_json
    duplicate = await repository.create_run(
        context=context.model_copy(update={"request_id": "req-reconcile"}),
        goal="Continue agent session",
        max_steps=4,
        run_type=common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST,
        session_id="session-1",
        session_version=7,
        turn_input_json=turn_input_json,
    )
    assert duplicate.created is False
    with pytest.raises(RunConflictError):
        await repository.create_run(
            context=context,
            goal="Continue agent session",
            max_steps=4,
            run_type=common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST,
            session_id="session-1",
            session_version=7,
            turn_input_json='{"message":"changed"}',
        )

    turn_result_json = (
        '{"assistant_message":"Draft ready","candidate_patches":[],'
        '"required_inputs":[],"review_readiness":"READY_FOR_REVIEW","warnings":[]}'
    )
    completed = await repository.transition_run(
        run_id,
        status=RunStatus.COMPLETED,
        type="run.completed",
        turn_result_json=turn_result_json,
    )
    assert completed.run.turn_result_json == turn_result_json
    persisted = await repository.get_run(run_id)
    assert persisted is not None
    assert persisted.turn_result_json == turn_result_json
    assert persisted.turn_input_json == turn_input_json


async def test_postgres_completed_session_turn_rejects_invalid_result(repository):
    run_id = f"run-pg-invalid-turn-{uuid4()}"
    context = RunContext(run_id=run_id, tenant_id="tenant-session", operator_id="operator-session")
    await repository.create_run(
        context=context, goal="Continue agent session", max_steps=4,
        run_type=common_pb2.RUN_TYPE_LOT_DRAFT_ASSIST, session_id="session-1",
        session_version=7, turn_input_json='{"message":"draft a lot"}',
    )

    terminal = await repository.transition_run(
        run_id, status=RunStatus.COMPLETED, type="run.completed", turn_result_json="{}",
    )

    assert terminal.run.status is RunStatus.FAILED
    assert terminal.run.error_code == "INVALID_TURN_RESULT"
    assert terminal.run.turn_result_json == ""


async def test_postgres_reconciled_run_allows_new_request_id(repository):
    run_id = f"run-pg-reconcile-{uuid4()}"
    original = RunContext(
        run_id=run_id,
        request_id="req-original",
        trace_id="trace-stable",
        tenant_id="tenant-stable",
        operator_id="operator-stable",
    )
    reconciled = original.model_copy(update={"request_id": "req-reconcile"})

    first = await repository.create_run(
        context=original, goal="Plan", max_steps=4
    )
    replay = await repository.create_run(
        context=reconciled, goal="Plan", max_steps=4
    )

    assert first.created is True
    assert replay.created is False
    assert replay.run.run_id == first.run.run_id == run_id
    assert replay.run.created_at_unix_ms == first.run.created_at_unix_ms
    assert replay.run.context.request_id == "req-original"
    events = await repository.list_events(run_id, 0)
    assert [event.type for event in events] == ["run.created"]
    with pytest.raises(RunConflictError):
        await repository.create_run(
            context=reconciled.model_copy(update={"trace_id": "trace-changed"}),
            goal="Plan",
            max_steps=4,
        )
    with pytest.raises(RunConflictError):
        await repository.create_run(
            context=reconciled,
            goal="Plan",
            max_steps=5,
        )


async def test_postgres_cancel_commit_atomically_wins_over_later_completion(repository):
    run_id = f"run-pg-cancel-race-{uuid4()}"
    await repository.create_run(
        context=RunContext(run_id=run_id), goal="Plan", max_steps=4
    )
    await repository.request_cancel(run_id)

    terminal = await repository.transition_run(
        run_id,
        status=RunStatus.COMPLETED,
        type="run.completed",
        proposal_json='{"should":"not persist"}',
        turn_result_json='{"should":"not persist"}',
    )

    assert terminal.run.status is RunStatus.CANCELLED
    assert terminal.run.proposal_json == ""
    assert terminal.run.turn_result_json == ""
    assert terminal.run.error_code == "RUN_CANCELLED"
    assert terminal.event is not None
    assert terminal.event.type == "run.cancelled"


async def test_postgres_restart_sweep_is_atomic_idempotent_and_preserves_terminal(repository):
    prefix = f"run-pg-restart-{uuid4()}"
    running_id = f"{prefix}-running"
    terminal_id = f"{prefix}-terminal"
    await repository.create_run(
        context=RunContext(run_id=running_id, tenant_id="tenant-restart"),
        goal="sensitive crash residue",
        max_steps=4,
    )
    await repository.transition_run(
        running_id, status=RunStatus.PLANNING, type="run.planning"
    )
    await repository.create_run(
        context=RunContext(run_id=terminal_id, tenant_id="tenant-restart"),
        goal="completed",
        max_steps=4,
    )
    await repository.transition_run(
        terminal_id, status=RunStatus.COMPLETED, type="run.completed"
    )

    first = await repository.fail_nonterminal_runs_on_restart()
    second = await repository.fail_nonterminal_runs_on_restart()

    assert sum(item.run.run_id == running_id for item in first) == 1
    assert all(item.run.run_id != running_id for item in second)
    failed = await repository.get_run(running_id)
    completed = await repository.get_run(terminal_id)
    assert failed is not None
    assert (failed.status, failed.error_code) == (
        RunStatus.FAILED,
        "AGENT_RUNTIME_RESTARTED",
    )
    assert completed is not None and completed.status is RunStatus.COMPLETED
    events = await repository.list_events(running_id, 0)
    assert [event.type for event in events].count("run.failed") == 1


async def test_postgres_saver_setup_and_harness_checkpoint_use_run_id_thread():
    if not POSTGRES_DSN:
        pytest.skip("AGENT_TEST_POSTGRES_DSN is not configured")
    from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver

    run_id = f"run-pg-checkpoint-{uuid4()}"
    skill_root = Path(__file__).parents[2] / "app" / "skills" / "packages"
    async with AsyncPostgresSaver.from_conn_string(POSTGRES_DSN) as checkpointer:
        await checkpointer.setup()
        harness = AuctionAgentHarness(
            model=FakeModel(
                [
                    ToolCallDecision(
                        kind="tool_call",
                        tool="inventory.query",
                        arguments={"limit": 50},
                    ),
                    FinalProposalDecision(kind="final_proposal", proposal=proposal()),
                ]
            ),
            tools=ToolRegistry(InventoryToolClient()),
            skills=SkillRegistry(skill_root),
            checkpointer=checkpointer,
        )

        result = await harness.start(
            RunInput(
                context=RunContext(run_id=run_id),
                goal="Persist a checkpoint",
                max_steps=3,
            )
        )
        checkpoint = await checkpointer.aget_tuple(
            {"configurable": {"thread_id": run_id}}
        )

    assert result.status is RunStatus.COMPLETED
    assert checkpoint is not None
