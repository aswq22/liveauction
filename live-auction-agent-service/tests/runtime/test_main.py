from __future__ import annotations

import asyncio
from dataclasses import replace
import json
from pathlib import Path

import pytest
from langgraph.checkpoint.memory import InMemorySaver

import app.main as main_module
from app.config import ModelProvider, Settings
from app.domain.models import AgentContext, Observation, RunContext
from app.domain.models import FinalProposalDecision, ToolCallDecision
from app.harness.context import RunInput, RunStatus, RunType
from app.harness.harness import AuctionAgentHarness
from app.main import create_model_factory, normalize_grpc_address
from app.models.openai_compat import TokenLimitField
from app.skills.registry import SkillRegistry
from app.tools.registry import ToolRegistry


pytestmark = pytest.mark.anyio


@pytest.fixture
def anyio_backend():
    return "asyncio"


def mock_settings() -> Settings:
    return Settings(
        grpc_addr=":19081",
        tool_grpc_addr="localhost:19082",
        database_dsn="postgresql://agent@localhost/auction",
        internal_service_token="internal-token",
        model_provider="mock",
        model_base_url="",
        model_name="",
        model_api_key="",
        model_timeout_seconds=30,
        max_steps=8,
        token_budget=8192,
    )


def context(observations: list[Observation]) -> AgentContext:
    return AgentContext(
        goal="Plan the live auction",
        skill_name="plan_live_session",
        skill_instructions="Use inventory evidence only for ownership.",
        observations=observations,
    )


async def test_mock_model_factory_is_independent_and_deterministic_per_run():
    factory = create_model_factory(mock_settings())
    first, second = factory(), factory()

    assert first is not second
    first_inventory, second_inventory = await asyncio.gather(
        first.decide(context([])), second.decide(context([]))
    )
    assert isinstance(first_inventory, ToolCallDecision)
    assert isinstance(second_inventory, ToolCallDecision)
    assert first_inventory.tool == second_inventory.tool == "inventory.query"

    inventory = Observation(
        tool="inventory.query",
        evidence={
            "lots": [
                {
                    "id": "lot-owned",
                    "title": "Jade vase",
                    "start_price": {"amount": 250, "currency": "CNY"},
                }
            ]
        },
    )
    first_history, second_history = await asyncio.gather(
        first.decide(context([inventory])), second.decide(context([inventory]))
    )
    assert isinstance(first_history, ToolCallDecision)
    assert isinstance(second_history, ToolCallDecision)
    assert first_history.tool == second_history.tool == "auction.history.query"

    history = Observation(tool="auction.history.query", evidence={"items": []})
    first_final, second_final = await asyncio.gather(
        first.decide(context([inventory, history])),
        second.decide(context([inventory, history])),
    )
    assert isinstance(first_final, FinalProposalDecision)
    assert isinstance(second_final, FinalProposalDecision)
    assert first_final.proposal.lots[0].lot_id == "lot-owned"
    assert second_final.proposal.lots[0].lot_id == "lot-owned"
    assert first_final.proposal.evidence
    assert first_final.proposal.risks


async def test_default_mock_factory_executes_runtime_lot_draft_assist_path():
    class NoToolClient:
        def __getattr__(self, name):
            raise AssertionError(f"lot draft assistance must not call {name}")

    factory = create_model_factory(mock_settings())
    harness = AuctionAgentHarness(
        model=factory(),
        tools=ToolRegistry(NoToolClient()),
        skills=SkillRegistry(Path(main_module.__file__).parent / "skills" / "packages"),
        checkpointer=InMemorySaver(),
    )
    message = "海洋之心，价格10000，时长50分钟，封顶100000"
    run = RunInput(
        context=RunContext(run_id="run-default-mock-lot", tenant_id="tenant-1"),
        goal=message,
        max_steps=4,
        run_type=RunType.LOT_DRAFT_ASSIST,
        turn_input_json=json.dumps({"message": message, "candidate_patches": []}),
    )

    summary = await harness.start(run)

    assert summary.status is RunStatus.COMPLETED
    assert summary.turn_result is not None
    assert summary.turn_result.candidate_patches[0].source_type == "NEW"
    fields = summary.turn_result.candidate_patches[0].fields
    assert {name: fields[name] for name in (
        "title", "startPriceAmount", "durationSeconds", "capPriceAmount"
    )} == {
        "title": "海洋之心",
        "startPriceAmount": 1_000_000,
        "durationSeconds": 3_000,
        "capPriceAmount": 10_000_000,
    }
    assert fields["minIncrementAmount"] == 10_000
    assert fields["stock"] == 1
    assert (
        fields["antiSnipeWindowSeconds"],
        fields["antiSnipeExtendSeconds"],
        fields["maxExtendCount"],
    ) == (30, 15, 5)
    assert summary.turn_result.required_inputs == ["MAIN_IMAGE"]


async def test_nonmock_model_factory_constructs_fresh_adapter_per_run(monkeypatch):
    constructed: list[object] = []

    class FakeOpenAICompatibleModel:
        def __init__(self, **configuration) -> None:
            self.configuration = configuration
            constructed.append(self)

    monkeypatch.setattr(
        main_module, "OpenAICompatibleModel", FakeOpenAICompatibleModel
    )
    settings = replace(
        mock_settings(),
        model_provider="openai-compatible",
        model_base_url="https://model.invalid/v1",
        model_name="auction-planner",
        model_api_key="test-api-key",
    )

    factory = create_model_factory(settings)
    first, second = factory(), factory()

    assert first is not second
    assert constructed == [first, second]
    assert first.configuration == second.configuration == {
        "model": "auction-planner",
        "timeout_seconds": 30,
        "base_url": "https://model.invalid/v1",
        "api_key": "test-api-key",
        "token_limit_field": TokenLimitField.MAX_COMPLETION_TOKENS,
    }


async def test_deepseek_factory_passes_explicit_max_tokens_contract(monkeypatch):
    constructed: list[object] = []

    class FakeOpenAICompatibleModel:
        def __init__(self, **configuration) -> None:
            self.configuration = configuration
            constructed.append(self)

    monkeypatch.setattr(main_module, "OpenAICompatibleModel", FakeOpenAICompatibleModel)
    settings = replace(
        mock_settings(),
        model_provider=ModelProvider.DEEPSEEK,
        model_base_url="https://api.deepseek.example/v1",
        model_name="deepseek-chat",
        model_api_key="test-api-key",
        model_token_limit_field=TokenLimitField.MAX_TOKENS,
    )

    model = create_model_factory(settings)()

    assert model.configuration["token_limit_field"] is TokenLimitField.MAX_TOKENS


@pytest.mark.parametrize(
    ("configured", "expected"),
    [(":19081", "[::]:19081"), ("127.0.0.1:19081", "127.0.0.1:19081")],
)
async def test_grpc_listen_address_normalization(configured: str, expected: str):
    assert normalize_grpc_address(configured) == expected
