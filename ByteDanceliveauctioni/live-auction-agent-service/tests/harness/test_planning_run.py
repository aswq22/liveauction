from __future__ import annotations

import json
from collections.abc import Iterable
from dataclasses import dataclass
from pathlib import Path

import pytest
from langgraph.checkpoint.memory import InMemorySaver

from app.domain.models import (
    AgentContext,
    EvidenceRef,
    FinalProposalDecision,
    LotPlan,
    Observation,
    RunContext,
    SessionProposal,
    ToolCallDecision,
)
from app.harness.context import RunInput, RunStatus
from app.harness.harness import AuctionAgentHarness
from app.graph.workflow import NeverCancelled, PlanningWorkflow
from app.models.fake import FakeModel
from app.observability.tracing import Tracing
from app.skills.registry import SkillRegistry
from app.tools.registry import ToolRegistry


pytestmark = pytest.mark.anyio


@pytest.fixture
def anyio_backend():
    return "asyncio"


@dataclass(frozen=True, slots=True)
class RecordedCall:
    name: str


class FakeToolClient:
    def __init__(self) -> None:
        self.calls: list[RecordedCall] = []

    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        self.calls.append(RecordedCall("inventory.query"))
        return Observation(
            tool="inventory.query",
            evidence={"lots": [{"id": "lot-owned-by-tenant", "title": "Jade vase"}]},
        )

    async def query_auction_history(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        self.calls.append(RecordedCall("auction.history.query"))
        return Observation(
            tool="auction.history.query",
            evidence={"items": [{"lot_id": "lot-history-only", "final_price": 1200}]},
        )

    async def get_session_snapshot(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        self.calls.append(RecordedCall("session.snapshot.get"))
        return Observation(
            tool="session.snapshot.get",
            evidence={"snapshot": {"queued_lots": [{"id": "lot-snapshot-only"}]}},
        )


class SequenceCancellation:
    def __init__(self, values: Iterable[bool]) -> None:
        self._values = iter(values)
        self.checks = 0

    async def is_cancelled(self, run_id: str) -> bool:
        assert run_id == "run-1"
        self.checks += 1
        return next(self._values, False)


class RecordingModel:
    def __init__(self, decision) -> None:
        self.decision = decision
        self.calls: list[AgentContext] = []

    async def decide(self, context: AgentContext):
        self.calls.append(context)
        return self.decision


class RecordingSequenceModel:
    def __init__(self, decisions) -> None:
        self._decisions = iter(decisions)
        self.calls: list[AgentContext] = []

    async def decide(self, context: AgentContext, *, max_output_tokens=None):
        del max_output_tokens
        self.calls.append(context)
        return next(self._decisions)


class EmptyInventoryToolClient(FakeToolClient):
    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        self.calls.append(RecordedCall("inventory.query"))
        return Observation(tool="inventory.query", evidence={"lots": []})


class DurationInventoryToolClient(FakeToolClient):
    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        self.calls.append(RecordedCall("inventory.query"))
        return Observation(
            tool="inventory.query",
            evidence={
                "lots": [
                    {"id": "lot-toy", "title": "玩偶", "duration_seconds": 720},
                    {"id": "lot-phone", "title": "小米手机", "duration_seconds": 660},
                    {"id": "lot-headset", "title": "耳机", "duration_seconds": 3000},
                    {"id": "lot-ocean", "title": "海洋之心", "duration_seconds": 3000},
                ]
            },
        )


class InventoryThenEmptyToolClient(FakeToolClient):
    def __init__(self) -> None:
        super().__init__()
        self.inventory_calls = 0

    async def query_inventory(self, arguments, run_context, *, timeout):
        self.inventory_calls += 1
        if self.inventory_calls == 1:
            return await super().query_inventory(
                arguments, run_context, timeout=timeout
            )
        del arguments, run_context, timeout
        self.calls.append(RecordedCall("inventory.query"))
        return Observation(tool="inventory.query", evidence={"lots": []})


class PaddedInventoryIDToolClient(FakeToolClient):
    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        self.calls.append(RecordedCall("inventory.query"))
        return Observation(
            tool="inventory.query", evidence={"lots": [{"id": " lot-owned-by-tenant "}]}
        )


@pytest.fixture
def skill_registry() -> SkillRegistry:
    skill_root = Path(__file__).parents[2] / "app" / "skills" / "packages"
    return SkillRegistry(skill_root)


def sample_proposal(lot_id: str = "lot-owned-by-tenant") -> SessionProposal:
    return SessionProposal(
        summary="Balanced live-auction plan",
        strategy="balanced",
        estimated_duration_minutes=10,
        lots=[
            LotPlan(
                lot_id=lot_id,
                sequence=1,
                role="opening lot",
                start_price_amount=100,
                increment_amount=10,
                duration_seconds=300,
                extension_seconds=30,
                reason="Supported by inventory and historical evidence",
            )
        ],
        evidence=[
            EvidenceRef(
                tool="inventory.query",
                reference_id="lot-owned-by-tenant",
                summary="Tenant inventory contains the lot",
            )
        ],
        risks=["Demand may vary"],
    )


def sample_run_input(*, max_steps: int = 8) -> RunInput:
    return RunInput(
        context=RunContext(
            run_id="run-1",
            request_id="request-1",
            trace_id="trace-1",
            tenant_id="tenant-1",
            operator_id="operator-1",
        ),
        goal="Plan the next live auction session",
        max_steps=max_steps,
    )


def reconstructed_state(*, decision=None):
    return {
        "run": sample_run_input(),
        "skill_name": "plan_live_session",
        "step_count": 0,
        "observations": [],
        "decision": decision,
        "proposal": None,
        "status": RunStatus.PLANNING,
        "error": None,
    }


async def test_harness_prefetches_inventory_before_first_model_call(
    skill_registry: SkillRegistry,
):
    tool_client = FakeToolClient()
    model = RecordingSequenceModel(
        [FinalProposalDecision(kind="final_proposal", proposal=sample_proposal())]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.COMPLETED
    assert [call.name for call in tool_client.calls] == ["inventory.query"]
    assert len(model.calls) == 1
    assert [item.tool for item in model.calls[0].observations] == ["inventory.query"]
    assert result.turn_result is not None
    assert result.turn_result.review_readiness == "READY_FOR_REVIEW"
    assert result.turn_result.candidate_patches[0].candidate_id == "lot-owned-by-tenant"
    assert result.turn_result.candidate_patches[0].source_type == "EXISTING"
    assert result.turn_result.candidate_patches[0].fields == {
        "existingLotId": "lot-owned-by-tenant"
    }
    assert result.turn_result.candidate_patches[0].field_sources == {
        "existingLotId": "EXISTING"
    }


async def test_session_plan_uses_turn_message_as_model_goal(
    skill_registry: SkillRegistry,
):
    tool_client = FakeToolClient()
    model = RecordingSequenceModel(
        [FinalProposalDecision(kind="final_proposal", proposal=sample_proposal())]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )
    run = sample_run_input().model_copy(
        update={
            "goal": "Continue agent session",
            "turn_input_json": json.dumps(
                {
                    "message": "一件低价开场，中间三件主推，一件高价压轴",
                    "candidate_patches": [],
                },
                ensure_ascii=False,
            ),
        }
    )

    result = await harness.start(run)

    assert result.status is RunStatus.COMPLETED
    assert model.calls[0].goal == "一件低价开场，中间三件主推，一件高价压轴"


async def test_session_plan_retries_when_duration_budget_or_required_lot_is_violated(
    skill_registry: SkillRegistry,
):
    def planned(lot_id: str, sequence: int, duration: int) -> LotPlan:
        return LotPlan(
            lot_id=lot_id,
            sequence=sequence,
            role="balanced",
            start_price_amount=100,
            increment_amount=10,
            duration_seconds=duration,
            extension_seconds=15,
            reason="inventory evidence",
        )

    bad = SessionProposal(
        summary="incorrect 112 minute plan missing the closest optional lot",
        strategy="balanced",
        estimated_duration_minutes=112,
        lots=[
            planned("lot-toy", 1, 720),
            planned("lot-headset", 2, 3000),
            planned("lot-ocean", 3, 3000),
        ],
        evidence=[],
        risks=[],
    )
    corrected = SessionProposal(
        summary="closest feasible 61 minute plan including 海洋之心",
        strategy="balanced",
        estimated_duration_minutes=61,
        lots=[planned("lot-phone", 1, 660), planned("lot-ocean", 2, 3000)],
        evidence=[],
        risks=["库存固定时长下最接近 60 分钟的是 61 分钟"],
    )
    model = RecordingSequenceModel(
        [
            FinalProposalDecision(kind="final_proposal", proposal=bad),
            FinalProposalDecision(kind="final_proposal", proposal=corrected),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(DurationInventoryToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )
    run = sample_run_input().model_copy(
        update={
            "turn_input_json": json.dumps(
                {
                    "message": "根据库存生成一个1小时的均衡型直播排品方案，但必须有海洋之心",
                    "candidate_patches": [],
                },
                ensure_ascii=False,
            )
        }
    )

    result = await harness.start(run)

    assert result.status is RunStatus.COMPLETED
    assert result.proposal is not None
    assert [lot.lot_id for lot in result.proposal.lots] == ["lot-phone", "lot-ocean"]
    assert result.proposal.estimated_duration_minutes == 61
    assert len(model.calls) == 2
    validation = model.calls[1].observations[-1]
    assert validation.tool == "proposal.validation"
    assert validation.evidence["target_duration_seconds"] == 3600
    assert validation.evidence["actual_duration_seconds"] == 6720
    assert validation.evidence["closest_feasible_duration_seconds"] == 3660
    assert validation.evidence["required_lot_ids"] == ["lot-ocean"]


async def test_session_plan_repairs_repeated_all_inventory_selection(
    skill_registry: SkillRegistry,
):
    def planned(lot_id: str, sequence: int, duration: int) -> LotPlan:
        return LotPlan(
            lot_id=lot_id,
            sequence=sequence,
            role="balanced",
            start_price_amount=100,
            increment_amount=10,
            duration_seconds=duration,
            extension_seconds=15,
            reason="inventory evidence",
        )

    all_inventory = SessionProposal(
        summary="model selected every inventory lot",
        strategy="balanced",
        estimated_duration_minutes=123,
        lots=[
            planned("lot-toy", 1, 720),
            planned("lot-phone", 2, 660),
            planned("lot-headset", 3, 3000),
            planned("lot-ocean", 4, 3000),
        ],
        evidence=[],
        risks=[],
    )
    harness = AuctionAgentHarness(
        model=RecordingSequenceModel(
            [
                FinalProposalDecision(kind="final_proposal", proposal=all_inventory),
            ]
        ),
        tools=ToolRegistry(DurationInventoryToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )
    run = sample_run_input().model_copy(
        update={
            "turn_input_json": json.dumps(
                {
                    "message": "根据库存生成一个1小时的均衡型直播排品方案，但必须有海洋之心",
                    "candidate_patches": [],
                },
                ensure_ascii=False,
            )
        }
    )

    result = await harness.start(run)

    assert result.status is RunStatus.COMPLETED
    assert result.proposal is not None
    assert [lot.lot_id for lot in result.proposal.lots] == [
        "lot-phone",
        "lot-ocean",
    ]
    assert [lot.sequence for lot in result.proposal.lots] == [1, 2]
    assert result.proposal.estimated_duration_minutes == 61
    assert any("Harness 已自动选择可执行组合" in risk for risk in result.proposal.risks)


async def test_session_plan_deduplicates_repeated_inventory_lots(
    skill_registry: SkillRegistry,
):
    proposal = sample_proposal()
    proposal = proposal.model_copy(
        update={"lots": [proposal.lots[0], proposal.lots[0], proposal.lots[0]]}
    )
    harness = AuctionAgentHarness(
        model=RecordingSequenceModel(
            [FinalProposalDecision(kind="final_proposal", proposal=proposal)]
        ),
        tools=ToolRegistry(FakeToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.COMPLETED
    assert result.turn_result is not None
    assert [item.candidate_id for item in result.turn_result.candidate_patches] == [
        "lot-owned-by-tenant"
    ]
    assert any("自动去重" in warning for warning in result.turn_result.warnings)


async def test_harness_empty_inventory_fails_without_calling_model(
    skill_registry: SkillRegistry,
):
    tool_client = EmptyInventoryToolClient()
    model = RecordingSequenceModel(
        [FinalProposalDecision(kind="final_proposal", proposal=sample_proposal())]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.FAILED
    assert result.error_code == "NO_ELIGIBLE_INVENTORY"
    assert result.error_message == "no eligible inventory lots are available"
    assert [call.name for call in tool_client.calls] == ["inventory.query"]
    assert model.calls == []


async def test_empty_followup_inventory_query_does_not_mask_prefetched_inventory(
    skill_registry: SkillRegistry,
):
    tool_client = InventoryThenEmptyToolClient()
    model = RecordingSequenceModel(
        [
            ToolCallDecision(
                kind="tool_call", tool="inventory.query", arguments={"limit": 1}
            ),
            FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.COMPLETED
    assert [call.name for call in tool_client.calls] == [
        "inventory.query",
        "inventory.query",
    ]


async def test_harness_retries_one_ungrounded_proposal_with_allowed_inventory_ids(
    skill_registry: SkillRegistry,
):
    model = RecordingSequenceModel(
        [
            FinalProposalDecision(
                kind="final_proposal", proposal=sample_proposal("lot-invented")
            ),
            FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(FakeToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.COMPLETED
    assert len(model.calls) == 2
    correction = model.calls[1].observations[-1]
    assert correction.tool == "proposal.validation"
    assert correction.evidence == {
        "allowed_lot_ids": ["lot-owned-by-tenant"],
        "reason": "proposal contains a lot absent from inventory observations",
    }


async def test_harness_allows_one_grounding_retry_after_step_budget_is_consumed(
    skill_registry: SkillRegistry,
):
    model = RecordingSequenceModel(
        [
            FinalProposalDecision(
                kind="final_proposal", proposal=sample_proposal("lot-invented")
            ),
            FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(FakeToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input(max_steps=1))

    assert result.status is RunStatus.COMPLETED
    assert len(model.calls) == 2


async def test_harness_rejects_tool_call_during_one_shot_grounding_retry(
    skill_registry: SkillRegistry,
):
    tool_client = FakeToolClient()
    model = RecordingSequenceModel(
        [
            FinalProposalDecision(
                kind="final_proposal", proposal=sample_proposal("lot-invented")
            ),
            ToolCallDecision(
                kind="tool_call", tool="auction.history.query", arguments={"limit": 50}
            ),
            FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.FAILED
    assert result.error_code == "UNGROUNDED_PROPOSAL"
    assert len(model.calls) == 2
    assert [call.name for call in tool_client.calls] == ["inventory.query"]


async def test_inventory_ids_are_normalized_for_grounding(
    skill_registry: SkillRegistry,
):
    harness = AuctionAgentHarness(
        model=RecordingSequenceModel(
            [FinalProposalDecision(kind="final_proposal", proposal=sample_proposal())]
        ),
        tools=ToolRegistry(PaddedInventoryIDToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.COMPLETED


async def test_harness_stops_after_one_ungrounded_proposal_retry(
    skill_registry: SkillRegistry,
):
    model = RecordingSequenceModel(
        [
            FinalProposalDecision(
                kind="final_proposal", proposal=sample_proposal("lot-invented-1")
            ),
            FinalProposalDecision(
                kind="final_proposal", proposal=sample_proposal("lot-invented-2")
            ),
            FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(FakeToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.FAILED
    assert result.error_code == "UNGROUNDED_PROPOSAL"
    assert len(model.calls) == 2


async def test_harness_collects_evidence_then_completes(skill_registry: SkillRegistry):
    tool_client = FakeToolClient()
    model = FakeModel(
        [
            ToolCallDecision(
                kind="tool_call", tool="auction.history.query", arguments={"limit": 50}
            ),
            FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
        ]
    )
    checkpointer = InMemorySaver()
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=checkpointer,
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.COMPLETED
    assert [call.name for call in tool_client.calls] == [
        "inventory.query",
        "auction.history.query",
    ]
    assert result.proposal is not None
    assert result.proposal.lots[0].lot_id == "lot-owned-by-tenant"
    checkpoint = await checkpointer.aget_tuple(
        {"configurable": {"thread_id": "run-1"}}
    )
    assert checkpoint is not None


async def test_harness_emits_safe_complete_model_tool_validation_trajectory(
    skill_registry: SkillRegistry,
):
    emitted: list[tuple[str, dict]] = []

    async def sink(event_type: str, payload: dict) -> None:
        emitted.append((event_type, payload))

    harness = AuctionAgentHarness(
        model=FakeModel(
            [
                ToolCallDecision(
                    kind="tool_call",
                    tool="auction.history.query",
                    arguments={"limit": 25},
                ),
                FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
            ]
        ),
        tools=ToolRegistry(FakeToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
        event_sink=sink,
        token_budget=10000,
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.COMPLETED
    assert [name for name, _ in emitted] == [
        "tool.started",
        "tool.completed",
        "model.started",
        "model.completed",
        "tool.started",
        "tool.completed",
        "model.started",
        "model.completed",
        "proposal.validated",
    ]
    serialized = str(emitted)
    assert "Plan the next live auction session" not in serialized
    assert '"limit": 25' not in serialized
    for _, payload in emitted:
        assert set(payload) <= {"step", "status", "tool", "duration_bucket", "count"}


async def test_small_token_budget_fails_without_partial_proposal(
    skill_registry: SkillRegistry,
):
    harness = AuctionAgentHarness(
        model=FakeModel(
            [ToolCallDecision(kind="tool_call", tool="inventory.query", arguments={})]
        ),
        tools=ToolRegistry(FakeToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
        token_budget=1,
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.FAILED
    assert result.error_code == "AGENT_TOKEN_BUDGET_EXCEEDED"
    assert result.proposal is None


async def test_tool_call_on_last_model_step_exhausts_budget_without_execution(
    skill_registry: SkillRegistry,
):
    tool_client = FakeToolClient()
    model = FakeModel(
        [
            ToolCallDecision(
                kind="tool_call", tool="auction.history.query", arguments={"limit": 50}
            ),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input(max_steps=1))

    assert result.status is RunStatus.FAILED
    assert result.error_code == "STEP_BUDGET_EXHAUSTED"
    assert [call.name for call in tool_client.calls] == ["inventory.query"]


async def test_final_proposal_on_last_model_step_is_allowed(skill_registry: SkillRegistry):
    tool_client = FakeToolClient()
    model = FakeModel(
        [
            FinalProposalDecision(kind="final_proposal", proposal=sample_proposal()),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input(max_steps=1))

    assert result.status is RunStatus.COMPLETED
    assert result.error_code == ""


async def test_finalize_rejects_ids_seen_only_in_history_or_snapshot(
    skill_registry: SkillRegistry,
):
    tool_client = FakeToolClient()
    model = FakeModel(
        [
            ToolCallDecision(
                kind="tool_call", tool="auction.history.query", arguments={"limit": 50}
            ),
            ToolCallDecision(
                kind="tool_call", tool="session.snapshot.get", arguments={"room_id": "room-1"}
            ),
            FinalProposalDecision(
                kind="final_proposal", proposal=sample_proposal("lot-history-only")
            ),
            FinalProposalDecision(
                kind="final_proposal", proposal=sample_proposal("lot-history-only")
            ),
        ]
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.FAILED
    assert result.error_code == "UNGROUNDED_PROPOSAL"
    assert result.proposal is None


async def test_skill_load_failure_uses_stable_skill_error_without_calling_model(
    tmp_path: Path,
):
    model = RecordingModel(
        FinalProposalDecision(kind="final_proposal", proposal=sample_proposal())
    )
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(FakeToolClient()),
        skills=SkillRegistry(tmp_path),
        checkpointer=InMemorySaver(),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.FAILED
    assert result.error_code == "SKILL_ERROR"
    assert model.calls == []


async def test_decide_node_reloads_skill_from_reconstructed_checkpoint_state(
    skill_registry: SkillRegistry,
):
    model = RecordingModel(
        FinalProposalDecision(kind="final_proposal", proposal=sample_proposal())
    )
    workflow = PlanningWorkflow(
        model=model,
        tools=ToolRegistry(FakeToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
        cancellation=NeverCancelled(),
        tracing=Tracing(),
    )

    result = await workflow._decide(reconstructed_state())

    assert result["status"] is RunStatus.VERIFYING
    assert len(model.calls) == 1
    assert model.calls[0].skill_name == "plan_live_session"
    assert "inventory.query" in model.calls[0].skill_instructions


async def test_tool_node_reloads_allowed_tools_from_reconstructed_checkpoint_state(
    skill_registry: SkillRegistry,
):
    tool_client = FakeToolClient()
    workflow = PlanningWorkflow(
        model=RecordingModel(None),
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
        cancellation=NeverCancelled(),
        tracing=Tracing(),
    )
    decision = ToolCallDecision(
        kind="tool_call", tool="inventory.query", arguments={"limit": 50}
    )

    result = await workflow._execute_tool(reconstructed_state(decision=decision))

    assert result["status"] is RunStatus.PLANNING
    assert [call.name for call in tool_client.calls] == ["inventory.query"]
    assert result["observations"][0].tool == "inventory.query"


@pytest.mark.parametrize(
    ("cancellation_values", "expected_model_calls", "expected_tool_calls"),
    [
        ([True], 0, 0),
        ([False, True], 0, 1),
        ([False, False, True], 0, 1),
        ([False, False, False, True], 1, 1),
        ([False, False, False, False, True], 1, 1),
        ([False, False, False, False, False, True], 1, 2),
    ],
    ids=[
        "before-inventory-tool",
        "after-inventory-tool",
        "before-model",
        "after-model",
        "before-requested-tool",
        "after-requested-tool",
    ],
)
async def test_cancellation_at_each_boundary_is_cancelled_never_failed(
    skill_registry: SkillRegistry,
    cancellation_values: list[bool],
    expected_model_calls: int,
    expected_tool_calls: int,
):
    decision = ToolCallDecision(
        kind="tool_call", tool="inventory.query", arguments={"limit": 50}
    )
    model = RecordingModel(decision)
    tool_client = FakeToolClient()
    harness = AuctionAgentHarness(
        model=model,
        tools=ToolRegistry(tool_client),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
        cancellation=SequenceCancellation(cancellation_values),
    )

    result = await harness.start(sample_run_input())

    assert result.status is RunStatus.CANCELLED
    assert result.error_code == "RUN_CANCELLED"
    assert len(model.calls) == expected_model_calls
    assert len(tool_client.calls) == expected_tool_calls
