from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from app.domain.models import AgentContext, FinalProposalDecision, Observation
from app.models.base import TokenLimitField
from app.models.fake import DeterministicPlanningModel
from app.models.openai_compat import OpenAICompatibleModel


CASES = json.loads(
    (Path(__file__).with_name("lot_draft_cases.json")).read_text(encoding="utf-8")
)
VALID_MODEL_DEFAULTS = {
    "category": "其他",
    "stock": 1,
    "antiSnipeWindowSeconds": 30,
    "antiSnipeExtendSeconds": 15,
    "maxExtendCount": 5,
    "depositAmount": 0,
}


@pytest.fixture
def anyio_backend() -> str:
    return "asyncio"


@pytest.mark.anyio
@pytest.mark.parametrize("case", CASES, ids=[case["id"] for case in CASES])
async def test_deterministic_lot_draft_eval(case: dict) -> None:
    model = DeterministicPlanningModel()
    context = AgentContext(
        goal=case["message"],
        skill_name="lot-draft-assist",
        skill_instructions="Extract explicit required fields and ask only for missing input.",
        observations=[Observation(tool="turn.input", evidence={"candidate_patches": []})],
    )

    result = await model.draft_lot(context)

    assert len(result.candidate_patches) == 1
    patch = result.candidate_patches[0]
    assert patch.source_type == "NEW"
    for field, expected in case["expected_fields"].items():
        assert patch.fields[field] == expected
        assert patch.field_sources[field] == "USER"
    for field, expected in VALID_MODEL_DEFAULTS.items():
        assert patch.fields[field] == expected
        assert patch.field_sources[field] == "AI"
    assert patch.fields["minIncrementAmount"] == max(
        100, patch.fields.get("startPriceAmount", 0) // 100
    )
    assert patch.field_sources["minIncrementAmount"] == "AI"
    assert result.required_inputs == case["required_inputs"]
    assert result.review_readiness == "NEEDS_INPUT"


@pytest.mark.anyio
async def test_opt_in_deepseek_structured_smoke() -> None:
    """No response text is logged or persisted; this runs only by explicit opt-in."""
    key = os.getenv("AGENT_MODEL_API_KEY") or os.getenv("DEEPSEEK_API_KEY")
    enabled = os.getenv("AGENT_RUN_REAL_MODEL_SMOKE") == "1"
    if not enabled or not key:
        pytest.skip("real provider smoke was not explicitly enabled with a runtime key")
    model = OpenAICompatibleModel(
        model=os.getenv("AGENT_MODEL_NAME", "deepseek-chat"),
        timeout_seconds=float(os.getenv("AGENT_MODEL_TIMEOUT_SECONDS", "60")),
        base_url=os.getenv("AGENT_MODEL_BASE_URL", "https://api.deepseek.com"),
        api_key=key,
        token_limit_field=TokenLimitField.MAX_TOKENS,
    )
    draft = await model.draft_lot(
        AgentContext(
            goal="烟测宝石，价格100，时长10分钟",
            skill_name="lot-draft-assist",
            skill_instructions="Extract explicit fields and ask only for missing required input.",
            observations=[Observation(tool="turn.input", evidence={"candidate_patches": []})],
        ),
        max_output_tokens=900,
    )
    assert draft.candidate_patches
    assert "MAIN_IMAGE" in draft.required_inputs
    assert draft.review_readiness == "NEEDS_INPUT"

    planning = await model.decide(
        AgentContext(
            goal="为库存生成排期",
            skill_name="auction-planning",
            skill_instructions="Use only supplied inventory and history evidence.",
            observations=[
                Observation(tool="inventory.query", evidence={"lots": [{"id": "smoke-owned-lot", "start_price": {"amount": 10000}}]}),
                Observation(tool="auction.history.query", evidence={"lots": []}),
            ],
        ),
        max_output_tokens=900,
    )
    assert isinstance(planning, FinalProposalDecision)
    assert planning.proposal.lots
    assert {lot.lot_id for lot in planning.proposal.lots} <= {"smoke-owned-lot"}
