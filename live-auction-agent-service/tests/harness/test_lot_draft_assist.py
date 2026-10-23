from __future__ import annotations

import json
from pathlib import Path

import pytest
from langgraph.checkpoint.memory import InMemorySaver

from app.domain.models import CandidatePatch, RequiredInput, RunContext, TurnResult
from app.harness.context import RunInput, RunStatus, RunType
from app.harness.harness import AuctionAgentHarness
from app.models.fake import DeterministicPlanningModel, FakeModel
from app.skills.registry import SkillRegistry
from app.tools.registry import ToolRegistry


pytestmark = pytest.mark.anyio


@pytest.fixture
def anyio_backend():
    return "asyncio"


class NoToolClient:
    def __getattr__(self, name):
        raise AssertionError(f"lot draft assistance must not call tool client method {name}")


@pytest.fixture
def skill_registry() -> SkillRegistry:
    skill_root = Path(__file__).parents[2] / "app" / "skills" / "packages"
    return SkillRegistry(skill_root)


def lot_run(
    message: str,
    *,
    candidates: list[dict] | None = None,
    run_id: str = "run-lot-1",
) -> RunInput:
    return RunInput(
        context=RunContext(run_id=run_id, tenant_id="tenant-1"),
        goal=message,
        max_steps=4,
        run_type=RunType.LOT_DRAFT_ASSIST,
        turn_input_json=json.dumps(
            {"message": message, "candidate_patches": candidates or []},
            ensure_ascii=False,
        ),
    )


def model_turn(
    fields: dict,
    sources: dict,
    *,
    required_inputs: list[RequiredInput] | None = None,
):
    return TurnResult(
        assistant_message="我整理好了拍品草稿。",
        candidate_patches=[
            CandidatePatch(
                candidate_id="candidate-1",
                source_type="NEW",
                fields=fields,
                field_sources=sources,
            )
        ],
        required_inputs=required_inputs or [],
        review_readiness="READY_FOR_REVIEW",
        warnings=[],
    )


async def run_lot_turn(
    skill_registry: SkillRegistry, message: str, turn: TurnResult, *, candidates=None
):
    harness = AuctionAgentHarness(
        model=FakeModel([turn]),
        tools=ToolRegistry(NoToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )
    return await harness.start(lot_run(message, candidates=candidates))


async def test_deterministic_model_candidate_id_is_run_scoped_and_retry_stable(
    skill_registry: SkillRegistry,
):
    harness = AuctionAgentHarness(
        model=DeterministicPlanningModel(),
        tools=ToolRegistry(NoToolClient()),
        skills=skill_registry,
        checkpointer=InMemorySaver(),
    )
    message = "海洋之心，价格10000，时长50分钟"

    first = await harness.start(lot_run(message, run_id="run-session-a"))
    replay = await harness.start(lot_run(message, run_id="run-session-a"))
    second_session = await harness.start(lot_run(message, run_id="run-session-b"))

    first_id = first.turn_result.candidate_patches[0].candidate_id
    replay_id = replay.turn_result.candidate_patches[0].candidate_id
    second_id = second_session.turn_result.candidate_patches[0].candidate_id
    assert first_id == replay_id
    assert first_id != second_id
    assert first_id.startswith("candidate-")
    assert len(first_id) <= 64


async def test_initial_new_fixed_chinese_lot_copy_converts_money_duration_and_only_asks_image(
    skill_registry: SkillRegistry,
):
    message = "海洋之心，价格10000，时长50分钟，封顶100000"
    turn = model_turn(
        {
            "title": "海洋之心",
            "startPriceAmount": 1_000_000,
            "durationSeconds": 3_000,
            "capPriceAmount": 10_000_000,
        },
        {
            "title": "USER",
            "startPriceAmount": "USER",
            "durationSeconds": "USER",
            "capPriceAmount": "USER",
        },
    )

    summary = await run_lot_turn(skill_registry, message, turn)

    assert summary.status is RunStatus.COMPLETED
    assert summary.turn_result is not None
    patch = summary.turn_result.candidate_patches[0]
    assert patch.fields["startPriceAmount"] == 1_000_000
    assert patch.fields["durationSeconds"] == 3_000
    assert patch.fields["capPriceAmount"] == 10_000_000
    assert summary.turn_result.required_inputs == ["MAIN_IMAGE"]
    assert summary.turn_result.review_readiness == "NEEDS_INPUT"


async def test_harness_grounds_all_explicit_auction_rule_fields(
    skill_registry: SkillRegistry,
):
    message = (
        "我要拍卖一台名为“农夫山泉”的矿泉水，分类是数码手机，"
        "起拍价5000元，最低每次加价100元，封顶价8000元，"
        "竞拍时长30分钟，库存2台，保证金500元。"
        "结束前30秒有人出价就延长15秒，最多延长5次"
    )
    provider_turn = model_turn(
        {"title": "错误标题", "depositAmount": 0},
        {"title": "AI", "depositAmount": "AI"},
    )

    summary = await run_lot_turn(skill_registry, message, provider_turn)

    assert summary.status is RunStatus.COMPLETED
    patch = summary.turn_result.candidate_patches[0]
    expected = {
        "title": "农夫山泉",
        "category": "数码手机",
        "startPriceAmount": 500_000,
        "minIncrementAmount": 10_000,
        "capPriceAmount": 800_000,
        "durationSeconds": 1_800,
        "stock": 2,
        "depositAmount": 50_000,
        "antiSnipeWindowSeconds": 30,
        "antiSnipeExtendSeconds": 15,
        "maxExtendCount": 5,
    }
    for field, value in expected.items():
        assert patch.fields[field] == value
    for field in (
        "title", "category", "startPriceAmount", "minIncrementAmount",
        "capPriceAmount", "durationSeconds", "stock", "depositAmount",
        "antiSnipeWindowSeconds", "antiSnipeExtendSeconds", "maxExtendCount",
    ):
        assert patch.field_sources[field] == "USER"
    assert summary.turn_result.required_inputs == ["MAIN_IMAGE"]


@pytest.mark.parametrize(
    ("message", "expected_title"),
    [
        ("我要拍卖名为aa的水杯，竞拍50分钟，封顶10万元，价格1万元", "aa"),
        ("我要拍卖名为海洋之心的宝石，竞拍50分钟，价格1万元", "海洋之心"),
        ("我要拍卖名为春晓的书法作品，竞拍20分钟，价格5000元", "春晓"),
        ("我要拍卖名为“爱的礼物”的摆件，竞拍50分钟，价格1万元", "爱的礼物"),
    ],
)
async def test_harness_treats_explicit_name_as_title_not_following_product_type(
    skill_registry: SkillRegistry,
    message: str,
    expected_title: str,
):
    summary = await run_lot_turn(
        skill_registry,
        message,
        model_turn({"title": "错误标题"}, {"title": "AI"}),
    )

    assert summary.status is RunStatus.COMPLETED
    patch = summary.turn_result.candidate_patches[0]
    assert patch.fields["title"] == expected_title
    assert patch.field_sources["title"] == "USER"


async def test_harness_grounds_common_auction_phrase_when_provider_uses_wrong_schema(
    skill_registry: SkillRegistry,
):
    message = "拍卖迪迦奥特曼，价格19698，时长10分钟，封顶价格100000。"
    provider_turn = model_turn(
        {
            "title": "拍卖迪迦奥特曼",
            "startPriceAmount": 1_969_800,
            "durationSeconds": 600,
            "maxPrice": 10_000_000,
        },
        {
            "title": "AI",
            "startPriceAmount": "AI",
            "durationSeconds": "AI",
            "maxPrice": "AI",
        },
    )
    provider_turn = provider_turn.model_copy(
        update={"assistant_message": "请提供迪迦奥特曼的标题和主图。"}
    )

    summary = await run_lot_turn(skill_registry, message, provider_turn)

    assert summary.status is RunStatus.COMPLETED
    patch = summary.turn_result.candidate_patches[0]
    assert patch.candidate_id.startswith("candidate-")
    assert patch.candidate_id != "candidate-1"
    assert patch.fields["title"] == "迪迦奥特曼"
    assert patch.fields["startPriceAmount"] == 1_969_800
    assert patch.fields["durationSeconds"] == 600
    assert patch.fields["capPriceAmount"] == 10_000_000
    assert "maxPrice" not in patch.fields
    assert patch.field_sources["title"] == "USER"
    assert patch.field_sources["capPriceAmount"] == "USER"
    assert summary.turn_result.required_inputs == ["MAIN_IMAGE"]
    assert summary.turn_result.assistant_message == (
        "竞拍草稿已生成。还需要你补充：真实主图。"
        "其余非必填参数已由 AI 自动补全，可在右侧编辑。"
    )


async def test_harness_does_not_treat_leading_clock_time_as_lot_title(
    skill_registry: SkillRegistry,
):
    message = "下午3点，拍卖小米手机"
    provider_turn = model_turn(
        {"title": "下午3点"},
        {"title": "AI"},
        required_inputs=[RequiredInput.START_PRICE, RequiredInput.DURATION, RequiredInput.MAIN_IMAGE],
    )

    summary = await run_lot_turn(skill_registry, message, provider_turn)

    assert summary.status is RunStatus.COMPLETED
    patch = summary.turn_result.candidate_patches[0]
    assert patch.fields["title"] == "小米手机"
    assert patch.field_sources["title"] == "USER"
    assert summary.turn_result.required_inputs == ["START_PRICE", "DURATION", "MAIN_IMAGE"]


@pytest.mark.parametrize(
    ("message", "expected_cents"),
    [
        ("下午5点，，以1000元起拍价格拍卖眼睛喷雾", 100_000),
        ("拍卖眼睛喷雾，起拍价格为1000元", 100_000),
        ("拍卖眼睛喷雾，1000元起拍", 100_000),
        ("拍卖眼睛喷雾，价格1000", 100_000),
    ],
)
async def test_harness_grounds_start_price_in_common_chinese_word_orders(
    skill_registry: SkillRegistry,
    message: str,
    expected_cents: int,
):
    provider_turn = model_turn(
        {"title": "错误标题"},
        {"title": "AI"},
        required_inputs=[RequiredInput.START_PRICE, RequiredInput.DURATION, RequiredInput.MAIN_IMAGE],
    )

    summary = await run_lot_turn(skill_registry, message, provider_turn)

    assert summary.status is RunStatus.COMPLETED
    patch = summary.turn_result.candidate_patches[0]
    assert patch.fields["title"] == "眼睛喷雾"
    assert patch.fields["startPriceAmount"] == expected_cents
    assert patch.field_sources["startPriceAmount"] == "USER"
    assert summary.turn_result.required_inputs == ["DURATION", "MAIN_IMAGE"]


@pytest.mark.parametrize(
    ("message", "expected_seconds"),
    [
        ("我要拍卖名为海洋之心的宝石，竞拍50分钟，封顶10万元，价格1万元。", 3_000),
        ("拍卖海洋之心，竞拍时长50分钟，价格1万元", 3_000),
        ("拍卖海洋之心，拍50分钟，价格1万元", 3_000),
        ("拍卖海洋之心，持续半小时，价格1万元", 1_800),
        ("拍卖海洋之心，竞拍1.5小时，价格1万元", 5_400),
    ],
)
async def test_harness_grounds_duration_in_common_chinese_phrasings(
    skill_registry: SkillRegistry,
    message: str,
    expected_seconds: int,
):
    provider_turn = model_turn(
        {"title": "海洋之心", "startPriceAmount": 1_000_000},
        {"title": "USER", "startPriceAmount": "USER"},
        required_inputs=[RequiredInput.DURATION, RequiredInput.MAIN_IMAGE],
    )

    summary = await run_lot_turn(skill_registry, message, provider_turn)

    assert summary.status is RunStatus.COMPLETED
    patch = summary.turn_result.candidate_patches[0]
    assert patch.fields["durationSeconds"] == expected_seconds
    assert patch.field_sources["durationSeconds"] == "USER"
    assert summary.turn_result.required_inputs == ["MAIN_IMAGE"]


async def test_provider_cannot_introduce_invented_existing_candidate(
    skill_registry: SkillRegistry,
):
    turn = TurnResult(
        assistant_message="找到一个现有拍品。",
        candidate_patches=[
            CandidatePatch(
                candidate_id="invented-existing",
                source_type="EXISTING",
                fields={},
                field_sources={},
            )
        ],
        required_inputs=[],
        review_readiness="READY_FOR_REVIEW",
        warnings=[],
    )

    summary = await run_lot_turn(skill_registry, "使用现有拍品", turn)

    assert summary.status is RunStatus.FAILED
    assert summary.turn_result is None


async def test_provider_cannot_mutate_matched_candidate_source_type(
    skill_registry: SkillRegistry,
):
    current = {
        "candidate_id": "candidate-1",
        "source_type": "NEW",
        "fields": {"title": "用户标题"},
        "field_sources": {"title": "USER"},
    }
    turn = TurnResult(
        assistant_message="改成现有拍品。",
        candidate_patches=[
            CandidatePatch(
                candidate_id="candidate-1",
                source_type="EXISTING",
                fields={"existingLotId": "lot-invented"},
                field_sources={"existingLotId": "AI"},
            )
        ],
        required_inputs=[],
        review_readiness="READY_FOR_REVIEW",
        warnings=[],
    )

    summary = await run_lot_turn(
        skill_registry, "把它改成现有拍品", turn, candidates=[current]
    )

    assert summary.status is RunStatus.FAILED
    assert summary.turn_result is None


async def test_provider_duplicate_candidate_ids_are_rejected(
    skill_registry: SkillRegistry,
):
    duplicate = CandidatePatch(
        candidate_id="candidate-duplicate",
        source_type="NEW",
        fields={"title": "海洋之心"},
        field_sources={"title": "USER"},
    )
    turn = TurnResult(
        assistant_message="生成两个草稿。",
        candidate_patches=[duplicate, duplicate.model_copy(deep=True)],
        required_inputs=[],
        review_readiness="READY_FOR_REVIEW",
        warnings=[],
    )

    summary = await run_lot_turn(skill_registry, "添加海洋之心", turn)

    assert summary.status is RunStatus.FAILED
    assert summary.turn_result is None


async def test_duplicate_current_candidate_ids_are_rejected(
    skill_registry: SkillRegistry,
):
    current = {
        "candidate_id": "candidate-duplicate",
        "source_type": "NEW",
        "fields": {"title": "海洋之心"},
        "field_sources": {"title": "USER"},
    }
    turn = model_turn({}, {})

    summary = await run_lot_turn(
        skill_registry,
        "继续编辑",
        turn,
        candidates=[current, dict(current)],
    )

    assert summary.status is RunStatus.FAILED
    assert summary.turn_result is None


@pytest.mark.parametrize(
    ("message", "fields", "sources", "missing"),
    (
        (
            "海洋之心，时长50分钟",
            {"title": "海洋之心", "durationSeconds": 3_000},
            {"title": "USER", "durationSeconds": "USER"},
            "START_PRICE",
        ),
        (
            "海洋之心，价格10000",
            {"title": "海洋之心", "startPriceAmount": 1_000_000},
            {"title": "USER", "startPriceAmount": "USER"},
            "DURATION",
        ),
    ),
)
async def test_harness_derives_missing_required_price_and_duration_instead_of_trusting_model(
    skill_registry: SkillRegistry, message, fields, sources, missing
):
    summary = await run_lot_turn(
        skill_registry,
        message,
        model_turn(fields, sources, required_inputs=[]),
    )

    assert missing in summary.turn_result.required_inputs
    assert summary.turn_result.review_readiness == "NEEDS_INPUT"


async def test_model_patch_cannot_replace_user_fields_and_conflicts_become_warnings(
    skill_registry: SkillRegistry,
):
    current = {
        "candidate_id": "candidate-1",
        "source_type": "NEW",
        "fields": {
            "title": "用户标题",
            "startPriceAmount": 10_000,
            "durationSeconds": 600,
        },
        "field_sources": {
            "title": "USER",
            "startPriceAmount": "USER",
            "durationSeconds": "USER",
        },
    }
    proposed = model_turn(
        {
            "title": "模型标题",
            "startPriceAmount": 20_000,
            "durationSeconds": 600,
            "description": "AI 可以补充空字段",
        },
        {
            "title": "AI",
            "startPriceAmount": "AI",
            "durationSeconds": "AI",
            "description": "AI",
        },
    )

    summary = await run_lot_turn(
        skill_registry, "标题和价格不要改，补充描述", proposed, candidates=[current]
    )

    merged = summary.turn_result.candidate_patches[0]
    assert merged.fields["title"] == "用户标题"
    assert merged.fields["startPriceAmount"] == 10_000
    assert merged.fields["durationSeconds"] == 600
    assert merged.fields["description"] == "AI 可以补充空字段"
    assert merged.field_sources["title"] == "USER"
    assert merged.field_sources["startPriceAmount"] == "USER"
    assert any("title" in warning for warning in summary.turn_result.warnings)
    assert any("startPriceAmount" in warning for warning in summary.turn_result.warnings)


async def test_harness_preserves_semantic_ambiguous_candidate_question(
    skill_registry: SkillRegistry,
):
    turn = model_turn(
        {
            "title": "海洋之心",
            "startPriceAmount": 1_000_000,
            "durationSeconds": 3_000,
            "mainImageAssetId": "asset-1",
        },
        {
            "title": "USER",
            "startPriceAmount": "USER",
            "durationSeconds": "USER",
            "mainImageAssetId": "USER",
        },
        required_inputs=[RequiredInput.AMBIGUOUS_CANDIDATE],
    )

    summary = await run_lot_turn(skill_registry, "把这个也加进去", turn)

    assert summary.turn_result.required_inputs == ["AMBIGUOUS_CANDIDATE"]
    assert summary.turn_result.review_readiness == "NEEDS_INPUT"


async def test_copy_request_without_current_draft_does_not_create_a_phantom_lot(
    skill_registry: SkillRegistry,
):
    proposed = model_turn(
        {"title": "给海洋之心生成一个适合直播间展示的介绍", "description": "高端介绍"},
        {"title": "AI", "description": "AI"},
    )

    summary = await run_lot_turn(
        skill_registry,
        "给海洋之心生成一个适合直播间展示的介绍，风格高端一些。",
        proposed,
    )

    assert summary.turn_result.candidate_patches == []
    assert summary.turn_result.required_inputs == ["AMBIGUOUS_CANDIDATE"]
    assert summary.turn_result.assistant_message == "请说明要修改哪一件候选拍品。"


async def test_copy_request_updates_the_current_draft_instead_of_creating_another(
    skill_registry: SkillRegistry,
):
    current = CandidatePatch(
        candidate_id="candidate-ocean",
        source_type="NEW",
        fields={"title": "海洋之心", "description": "普通介绍"},
        field_sources={"title": "USER", "description": "AI"},
    )
    proposed = model_turn(
        {"description": "海洋之心甄选宝石拍品，呈现典雅收藏质感。"},
        {"description": "AI"},
    ).model_copy(
        update={
            "candidate_patches": [
                CandidatePatch(
                    candidate_id="candidate-ocean",
                    source_type="NEW",
                    fields={"description": "海洋之心甄选宝石拍品，呈现典雅收藏质感。"},
                    field_sources={"description": "AI"},
                )
            ]
        }
    )

    summary = await run_lot_turn(
        skill_registry,
        "给海洋之心生成一个适合直播间展示的介绍，风格高端一些。",
        proposed,
        candidates=[current.model_dump(mode="json")],
    )

    assert len(summary.turn_result.candidate_patches) == 1
    updated = summary.turn_result.candidate_patches[0]
    assert updated.candidate_id == "candidate-ocean"
    assert updated.fields["title"] == "海洋之心"
    assert updated.fields["description"] == "海洋之心甄选宝石拍品，呈现典雅收藏质感。"
