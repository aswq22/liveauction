from __future__ import annotations

import logging
from types import SimpleNamespace

import httpx
import pytest
from openai import APITimeoutError, OpenAIError
from pydantic import TypeAdapter, ValidationError

from app.domain.models import (
    AgentContext,
    AgentDecision,
    CandidatePatch,
    EvidenceRef,
    FinalProposalDecision,
    LotPlan,
    RequiredInput,
    SessionProposal,
    ToolCallDecision,
    TurnResult,
)
from app.models.base import ModelOutputError, ModelProviderError, ModelTimeoutError
from app.models.fake import FakeModel, FakeModelExhaustedError
from app.models.openai_compat import OpenAICompatibleModel, TokenLimitField


pytestmark = pytest.mark.anyio


@pytest.fixture
def anyio_backend():
    return "asyncio"


def sample_context() -> AgentContext:
    return AgentContext(
        goal="Prepare a live auction session",
        skill_name="plan_live_session",
        skill_instructions="Use only grounded inventory lots.",
        observations=[],
    )


def sample_proposal() -> SessionProposal:
    return SessionProposal(
        summary="A balanced two-stage live auction plan.",
        strategy="balanced",
        estimated_duration_minutes=12,
        lots=[
            LotPlan(
                lot_id="lot-1",
                sequence=1,
                role="opener",
                start_price_amount=0,
                increment_amount=100,
                duration_seconds=120,
                extension_seconds=10,
                reason="The owned lot has enough stock for the opening slot.",
            )
        ],
        evidence=[
            EvidenceRef(
                tool="inventory.query",
                reference_id="lot-1",
                summary="The inventory result confirms this merchant-owned lot.",
            )
        ],
        risks=["Historical sale evidence is limited."],
    )


async def test_fake_model_returns_typed_decisions_in_queue_order():
    tool_call = ToolCallDecision(
        kind="tool_call", tool="inventory.query", arguments={"limit": 50}
    )
    final = FinalProposalDecision(kind="final_proposal", proposal=sample_proposal())
    model = FakeModel([tool_call, final])

    assert await model.decide(sample_context()) is tool_call
    assert await model.decide(sample_context()) is final


async def test_fake_model_reports_queue_exhaustion_clearly():
    model = FakeModel([])

    with pytest.raises(FakeModelExhaustedError, match="decision queue is exhausted"):
        await model.decide(sample_context())


def test_agent_decision_union_validates_only_discriminated_complete_json():
    adapter = TypeAdapter(AgentDecision)
    decision = adapter.validate_json(
        '{"kind":"tool_call","tool":"inventory.query","arguments":{"limit":50}}'
    )

    assert isinstance(decision, ToolCallDecision)

    with pytest.raises(ValidationError):
        adapter.validate_json('{"tool":"inventory.query","arguments":{}}')
    with pytest.raises(ValidationError):
        adapter.validate_json(
            '```json\n{"kind":"tool_call","tool":"inventory.query","arguments":{}}\n```'
        )


def test_agent_decision_schema_requires_discriminator_for_both_variants():
    definitions = TypeAdapter(AgentDecision).json_schema()["$defs"]

    assert "kind" in definitions["ToolCallDecision"]["required"]
    assert "kind" in definitions["FinalProposalDecision"]["required"]


def test_session_proposal_schema_and_validation_require_evidence_and_risks():
    schema = SessionProposal.model_json_schema()

    assert "evidence" in schema["required"]
    assert "risks" in schema["required"]

    for missing_field in ("evidence", "risks"):
        payload = sample_proposal().model_dump()
        del payload[missing_field]
        with pytest.raises(ValidationError):
            SessionProposal.model_validate(payload)


@pytest.mark.parametrize(
    ("field", "value"),
    (
        ("summary", "   "),
        ("strategy", "reckless"),
        ("estimated_duration_minutes", 0),
    ),
)
def test_session_proposal_rejects_invalid_summary_strategy_and_duration(field, value):
    payload = sample_proposal().model_dump()
    payload[field] = value

    with pytest.raises(ValidationError):
        SessionProposal.model_validate(payload)


@pytest.mark.parametrize(
    ("field", "value"),
    (
        ("lot_id", ""),
        ("sequence", 0),
        ("start_price_amount", -1),
        ("increment_amount", -1),
        ("duration_seconds", 0),
        ("extension_seconds", 0),
    ),
)
def test_lot_plan_rejects_ungrounded_shape_and_invalid_numeric_bounds(field, value):
    payload = sample_proposal().lots[0].model_dump()
    payload[field] = value

    with pytest.raises(ValidationError):
        LotPlan.model_validate(payload)


class FakeCompletions:
    def __init__(self, *, content: str | None = None, error: Exception | None = None) -> None:
        self._content = content
        self._error = error
        self.calls: list[dict] = []

    async def create(self, **kwargs):
        self.calls.append(kwargs)
        if self._error is not None:
            raise self._error
        return SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content=self._content))]
        )


class FakeOpenAIClient:
    def __init__(self, completions: FakeCompletions) -> None:
        self.chat = SimpleNamespace(completions=completions)


async def test_openai_compatible_model_requests_json_and_returns_union_member():
    completions = FakeCompletions(
        content='{"kind":"tool_call","tool":"inventory.query","arguments":{"limit":25}}'
    )
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(completions), model="auction-planner", timeout_seconds=17
    )

    decision = await model.decide(sample_context())

    assert isinstance(decision, ToolCallDecision)
    assert decision.arguments == {"limit": 25}
    assert completions.calls[0]["model"] == "auction-planner"
    assert completions.calls[0]["response_format"] == {"type": "json_object"}
    assert completions.calls[0]["timeout"] == 17
    system_message = completions.calls[0]["messages"][0]["content"]
    assert '"tool_call"' in system_message
    assert '"final_proposal"' in system_message


async def test_openai_compatible_model_requests_strict_lot_turn_schema_and_instructions():
    content = TurnResult(
        assistant_message="还需要一张主图。",
        candidate_patches=[
            CandidatePatch(
                candidate_id="candidate-1",
                source_type="NEW",
                fields={"title": "海洋之心", "startPriceAmount": 1_000_000},
                field_sources={"title": "USER", "startPriceAmount": "USER"},
            )
        ],
        required_inputs=[RequiredInput.DURATION, RequiredInput.MAIN_IMAGE],
        review_readiness="NEEDS_INPUT",
        warnings=[],
    ).model_dump_json()
    completions = FakeCompletions(content=content)
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(completions), model="auction-planner", timeout_seconds=17
    )

    result = await model.draft_lot(sample_context())

    assert isinstance(result, TurnResult)
    system_message = completions.calls[0]["messages"][0]["content"]
    assert "title" in system_message
    assert "startPriceAmount" in system_message
    assert "durationSeconds" in system_message
    assert "下午3点" in system_message
    assert "following 拍卖 or 上架" in system_message
    assert "1000元起拍" in system_message
    assert "竞拍50分钟" in system_message
    assert "名为aa的水杯" in system_message
    assert "deposit" in system_message
    assert "anti-snipe" in system_message
    assert "mainImage" in system_message
    assert "人民币元" in system_message
    assert "100" in system_message


async def test_openai_compatible_model_rejects_extra_lot_turn_fields():
    content = (
        '{"assistant_message":"ok","candidate_patches":[],"required_inputs":[],'
        '"review_readiness":"READY_FOR_REVIEW","warnings":[],"unexpected":true}'
    )
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(FakeCompletions(content=content)),
        model="auction-planner",
        timeout_seconds=17,
    )

    with pytest.raises(ModelOutputError, match="valid TurnResult"):
        await model.draft_lot(sample_context())


async def test_openai_compatible_model_applies_remaining_output_limit_and_usage():
    completions = FakeCompletions(
        content='{"kind":"tool_call","tool":"inventory.query","arguments":{}}'
    )
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(completions), model="auction-planner", timeout_seconds=17
    )
    original_create = completions.create

    async def create_with_usage(**kwargs):
        response = await original_create(**kwargs)
        response.usage = SimpleNamespace(prompt_tokens=11, completion_tokens=7)
        return response

    completions.create = create_with_usage

    await model.decide(sample_context(), max_output_tokens=23)

    assert completions.calls[0]["max_completion_tokens"] == 23
    assert "max_tokens" not in completions.calls[0]
    assert model.last_usage.input_tokens == 11
    assert model.last_usage.output_tokens == 7


async def test_deepseek_model_uses_legacy_max_tokens_request_contract():
    class StrictDeepSeekCompletions(FakeCompletions):
        async def create(self, **kwargs):
            assert "max_completion_tokens" not in kwargs
            assert kwargs["max_tokens"] == 23
            return await super().create(**kwargs)

    completions = StrictDeepSeekCompletions(
        content='{"kind":"tool_call","tool":"inventory.query","arguments":{}}'
    )
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(completions),
        model="deepseek-chat",
        timeout_seconds=17,
        token_limit_field=TokenLimitField.MAX_TOKENS,
    )

    await model.decide(sample_context(), max_output_tokens=23)

    assert completions.calls[0]["max_tokens"] == 23


@pytest.mark.parametrize(
    "content",
    (
        None,
        "",
        "not json",
        '```json\n{"kind":"tool_call","tool":"inventory.query","arguments":{}}\n```',
        '{"kind":"tool_call","tool":"inventory.query"}',
    ),
)
async def test_openai_compatible_model_maps_missing_or_invalid_content(content):
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(FakeCompletions(content=content)),
        model="auction-planner",
        timeout_seconds=17,
    )

    with pytest.raises(ModelOutputError, match="valid AgentDecision"):
        await model.decide(sample_context())


async def test_openai_compatible_model_maps_timeout_without_logging_sensitive_context(caplog):
    secret_goal = "secret auction goal"
    context = sample_context().model_copy(update={"goal": secret_goal})
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(FakeCompletions(error=TimeoutError("raw provider detail"))),
        model="auction-planner",
        timeout_seconds=17,
    )

    with caplog.at_level(logging.DEBUG), pytest.raises(ModelTimeoutError, match="timed out"):
        await model.decide(context)

    assert secret_goal not in caplog.text
    assert "raw provider detail" not in caplog.text


async def test_openai_compatible_model_maps_sdk_timeout_to_typed_error():
    timeout = APITimeoutError(request=httpx.Request("POST", "https://models.example/v1"))
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(FakeCompletions(error=timeout)),
        model="auction-planner",
        timeout_seconds=17,
    )

    with pytest.raises(ModelTimeoutError, match="model request timed out"):
        await model.decide(sample_context())


async def test_openai_compatible_model_maps_other_sdk_errors_without_raw_detail():
    model = OpenAICompatibleModel(
        client=FakeOpenAIClient(FakeCompletions(error=OpenAIError("raw provider secret"))),
        model="auction-planner",
        timeout_seconds=17,
    )

    with pytest.raises(ModelProviderError, match="model provider request failed") as captured:
        await model.decide(sample_context())

    assert "raw provider secret" not in str(captured.value)
