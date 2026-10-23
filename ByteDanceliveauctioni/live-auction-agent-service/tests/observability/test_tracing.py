from __future__ import annotations

from pathlib import Path

import pytest
from langgraph.checkpoint.memory import InMemorySaver
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from app.domain.models import (
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
from app.models.fake import FakeModel
from app.observability.tracing import Tracing, extract_trace_context
from app.skills.registry import SkillRegistry
from app.tools.registry import ToolRegistry


pytestmark = pytest.mark.anyio
TRACE_ID = "11111111111111111111111111111111"
PARENT_SPAN_ID = "2222222222222222"
TRACEPARENT = f"00-{TRACE_ID}-{PARENT_SPAN_ID}-01"
SAFE_ATTRIBUTE_KEYS = {
    "run_id",
    "skill",
    "tool",
    "status",
    "latency_ms",
    "input_tokens",
    "output_tokens",
    "total_tokens",
}


@pytest.fixture
def anyio_backend():
    return "asyncio"


class InventoryToolClient:
    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        return Observation(
            tool="inventory.query", evidence={"lots": [{"id": "lot-1"}]}
        )

    async def query_auction_history(self, arguments, run_context, *, timeout):
        raise AssertionError("history tool must not be called")

    async def get_session_snapshot(self, arguments, run_context, *, timeout):
        raise AssertionError("snapshot tool must not be called")


class FailingInventoryToolClient(InventoryToolClient):
    async def query_inventory(self, arguments, run_context, *, timeout):
        del arguments, run_context, timeout
        raise RuntimeError("full-private-tool-result")


def proposal() -> SessionProposal:
    return SessionProposal(
        summary="Grounded proposal",
        strategy="balanced",
        estimated_duration_minutes=5,
        lots=[
            LotPlan(
                lot_id="lot-1",
                sequence=1,
                role="opening",
                start_price_amount=10,
                increment_amount=1,
                duration_seconds=60,
                extension_seconds=10,
                reason="Inventory evidence",
            )
        ],
        evidence=[
            EvidenceRef(
                tool="inventory.query",
                reference_id="lot-1",
                summary="Lot belongs to inventory",
            )
        ],
        risks=["Limited demand evidence"],
    )


def tracing_fixture(parent_context):
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    tracing = Tracing(
        tracer=provider.get_tracer("test-agent"), parent_context=parent_context
    )
    return tracing, exporter


async def test_harness_continues_valid_traceparent_with_root_and_children():
    parent_context = extract_trace_context((("traceparent", TRACEPARENT),))
    tracing, exporter = tracing_fixture(parent_context)
    skill_root = Path(__file__).parents[2] / "app" / "skills" / "packages"
    harness = AuctionAgentHarness(
        model=FakeModel(
            [
                FinalProposalDecision(kind="final_proposal", proposal=proposal()),
            ]
        ),
        tools=ToolRegistry(InventoryToolClient()),
        skills=SkillRegistry(skill_root),
        checkpointer=InMemorySaver(),
        tracing=tracing,
    )

    result = await harness.start(
        RunInput(
            context=RunContext(run_id="run-traced"),
            goal="Plan a grounded auction",
            max_steps=3,
        )
    )

    assert result.status is RunStatus.COMPLETED
    spans = exporter.get_finished_spans()
    assert [span.name for span in spans].count("agent.run") == 1
    assert [span.name for span in spans].count("agent.model") == 1
    assert [span.name for span in spans].count("agent.tool") == 1
    root = next(span for span in spans if span.name == "agent.run")
    assert f"{root.context.trace_id:032x}" == TRACE_ID
    assert root.parent is not None
    assert f"{root.parent.span_id:016x}" == PARENT_SPAN_ID
    children = [span for span in spans if span.name != "agent.run"]
    assert all(span.parent is not None for span in children)
    assert all(span.parent.span_id == root.context.span_id for span in children)
    assert all(set(span.attributes).issubset(SAFE_ATTRIBUTE_KEYS) for span in spans)
    assert root.attributes["status"] == "COMPLETED"


@pytest.mark.parametrize(
    "metadata",
    [
        (("traceparent", "not-a-traceparent"),),
        (("traceparent", "00-00000000000000000000000000000000-2222222222222222-01"),),
        (("traceparent", TRACEPARENT), ("traceparent", TRACEPARENT)),
    ],
)
async def test_invalid_or_ambiguous_traceparent_starts_safe_new_root(metadata):
    tracing, exporter = tracing_fixture(extract_trace_context(metadata))

    with tracing.span("agent.run", {"run_id": "run-new"}, root=True):
        pass

    root = exporter.get_finished_spans()[0]
    assert root.parent is None
    assert root.context.is_valid
    assert f"{root.context.trace_id:032x}" != TRACE_ID


async def test_span_attribute_allowlist_drops_sensitive_content():
    tracing, exporter = tracing_fixture(extract_trace_context(()))

    with tracing.span(
        "agent.tool",
        {
            "run_id": "run-1",
            "tool": "inventory.query",
            "status": "completed",
            "prompt": "seller confidential prompt",
            "model_content": "private model output",
            "api_key": "sk-secret",
            "internal_token": "runtime-test-token",
            "tool_result": "full inventory response",
        },
        root=True,
    ):
        pass

    attributes = exporter.get_finished_spans()[0].attributes
    assert set(attributes).issubset(SAFE_ATTRIBUTE_KEYS)
    serialized = repr(attributes)
    for secret in (
        "seller confidential prompt",
        "private model output",
        "sk-secret",
        "runtime-test-token",
        "full inventory response",
    ):
        assert secret not in serialized


async def test_failed_model_boundary_records_safe_status_without_exception_detail():
    tracing, exporter = tracing_fixture(extract_trace_context(()))
    skill_root = Path(__file__).parents[2] / "app" / "skills" / "packages"
    harness = AuctionAgentHarness(
        model=FakeModel([]),
        tools=ToolRegistry(InventoryToolClient()),
        skills=SkillRegistry(skill_root),
        checkpointer=InMemorySaver(),
        tracing=tracing,
    )

    result = await harness.start(
        RunInput(
            context=RunContext(run_id="run-model-failure"),
            goal="Plan an auction",
            max_steps=2,
        )
    )

    assert result.status is RunStatus.FAILED
    spans = exporter.get_finished_spans()
    model_span = next(span for span in spans if span.name == "agent.model")
    assert model_span.attributes["status"] == "failed"
    assert not model_span.events
    assert "decision queue is exhausted" not in repr(model_span.attributes)


async def test_failed_tool_boundary_records_safe_status_without_result_detail():
    tracing, exporter = tracing_fixture(extract_trace_context(()))
    skill_root = Path(__file__).parents[2] / "app" / "skills" / "packages"
    harness = AuctionAgentHarness(
        model=FakeModel(
            [
                ToolCallDecision(
                    kind="tool_call", tool="inventory.query", arguments={"limit": 50}
                )
            ]
        ),
        tools=ToolRegistry(FailingInventoryToolClient()),
        skills=SkillRegistry(skill_root),
        checkpointer=InMemorySaver(),
        tracing=tracing,
    )

    result = await harness.start(
        RunInput(
            context=RunContext(run_id="run-tool-failure"),
            goal="Plan an auction",
            max_steps=2,
        )
    )

    assert result.status is RunStatus.FAILED
    spans = exporter.get_finished_spans()
    tool_span = next(span for span in spans if span.name == "agent.tool")
    assert tool_span.attributes["status"] == "failed"
    assert not tool_span.events
    assert "full-private-tool-result" not in repr(tool_span.attributes)
