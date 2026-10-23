"""Public harness around the compiled LangGraph planning workflow."""

from __future__ import annotations

import json

from pydantic import ValidationError

from app.domain.models import (
    AgentContext,
    CandidatePatch,
    Observation,
    RequiredInput,
    TurnResult,
)
from app.graph.state import AgentState
from app.graph.workflow import CancellationChecker, NeverCancelled, PlanningWorkflow
from app.harness.context import AgentRunSummary, RunError, RunInput, RunStatus, RunType
from app.harness.lot_draft_grounding import ground_provider_turn
from app.models.base import ModelAdapter
from app.observability.tracing import Tracing, set_safe_attribute
from app.skills.registry import SkillRegistry
from app.tools.registry import ToolRegistry


class AuctionAgentHarness:
    def __init__(
        self,
        *,
        model: ModelAdapter,
        tools: ToolRegistry,
        skills: SkillRegistry,
        checkpointer,
        cancellation: CancellationChecker | None = None,
        tracing: Tracing | None = None,
        event_sink=None,
        token_budget: int = 8192,
    ) -> None:
        self._tracing = tracing or Tracing()
        self._model = model
        self._token_budget = token_budget
        self._workflow = PlanningWorkflow(
            model=model,
            tools=tools,
            skills=skills,
            checkpointer=checkpointer,
            cancellation=cancellation or NeverCancelled(),
            tracing=self._tracing,
            event_sink=event_sink,
            token_budget=token_budget,
        )
    async def start(self, run: RunInput) -> AgentRunSummary:
        if run.run_type is RunType.LOT_DRAFT_ASSIST:
            return await self._start_lot_draft_assist(run)
        # Session runs are persisted with a generic audit goal.  The actual
        # planning instruction lives in turn_input_json and must be the goal
        # seen by the planning model (for example, "low-price opener, three
        # featured lots, high-value finale").
        _, planning_message = _decode_turn_input(run)
        run = run.model_copy(update={"goal": planning_message})
        initial: AgentState = {
            "run": run,
            "skill_name": "",
            "step_count": 0,
            "validation_retry_count": 0,
            "observations": [],
            "decision": None,
            "proposal": None,
            "status": RunStatus.CREATED,
            "error": None,
        }
        with self._tracing.span(
            "agent.run",
            {"run_id": run.context.run_id, "skill": "plan_live_session"},
            root=True,
        ) as span:
            try:
                final = await self._workflow.graph.ainvoke(
                    initial,
                    config={
                        "configurable": {"thread_id": run.context.run_id},
                        "recursion_limit": run.max_steps * 2 + 5,
                    },
                )
            except Exception:
                final = {
                    **initial,
                    "status": RunStatus.FAILED,
                    "error": RunError(
                        code="HARNESS_ERROR", message="planning workflow failed"
                    ),
                }
            status = final["status"]
            set_safe_attribute(span, "status", status.value)
        error = final.get("error")
        return AgentRunSummary(
            run_id=run.context.run_id,
            status=status,
            proposal=final.get("proposal") if status is RunStatus.COMPLETED else None,
            error_code=error.code if error else "",
            error_message=error.message if error else "",
            turn_result=(
                _proposal_turn_result(final.get("proposal"))
                if status is RunStatus.COMPLETED and final.get("proposal") is not None
                else None
            ),
        )

    async def _start_lot_draft_assist(self, run: RunInput) -> AgentRunSummary:
        with self._tracing.span(
            "agent.run",
            {"run_id": run.context.run_id, "skill": "lot_draft_assist"},
            root=True,
        ) as span:
            try:
                current, message = _decode_turn_input(run)
                context = AgentContext(
                    goal=message,
                    skill_name="lot_draft_assist",
                    skill_instructions=(
                        "Draft candidate patches from explicit user input. The required "
                        "NEW-lot fields are title, start price, duration, and main image. "
                        "Convert CNY yuan to integer cents and durations to seconds; "
                        "recognize prices both before and after 起拍价/起拍. "
                        "Preserve explicit minimum increment, cap price, deposit, stock, "
                        "category, and anti-snipe window/extension/count values. "
                        "Recognize duration labels 时长, 持续, 竞拍, and 拍. "
                        "For 名为X的Y, use X as the exact title and Y only as "
                        "the product type. "
                        "A request to generate or rewrite an introduction, description, "
                        "copy, tags, or category must patch the matching current candidate; "
                        "never turn the instruction itself into a new lot title. "
                        "Treat dates and clock times as scheduling context, never as the "
                        "lot title; the title is the item after 拍卖 or 上架."
                    ),
                    observations=[
                        Observation(
                            tool="turn.input",
                            evidence={
                                "run_id": run.context.run_id,
                                "candidate_patches": [
                                    item.model_dump(mode="json") for item in current
                                ]
                            },
                        )
                    ],
                )
                proposed = await self._model.draft_lot(
                    context, max_output_tokens=self._token_budget
                )
                _require_unique_candidate_ids(
                    proposed.candidate_patches, boundary="model output"
                )
                proposed = ground_provider_turn(
                    current,
                    proposed,
                    message=message,
                    run_id=run.context.run_id,
                )
                result = _merge_turn_result(current, proposed)
                status = RunStatus.COMPLETED
                error = None
            except Exception:
                result = None
                status = RunStatus.FAILED
                error = RunError(
                    code="HARNESS_ERROR", message="lot draft assistance failed"
                )
            set_safe_attribute(span, "status", status.value)
        return AgentRunSummary(
            run_id=run.context.run_id,
            status=status,
            proposal=None,
            error_code=error.code if error else "",
            error_message=error.message if error else "",
            turn_result=result,
        )


def _decode_turn_input(run: RunInput) -> tuple[list[CandidatePatch], str]:
    if not run.turn_input_json.strip():
        return [], run.goal
    try:
        payload = json.loads(run.turn_input_json)
    except (TypeError, ValueError):
        raise ValueError("turn input must be valid JSON") from None
    if not isinstance(payload, dict):
        raise ValueError("turn input must be a JSON object")
    raw_candidates = payload.get("candidate_patches", [])
    if not isinstance(raw_candidates, list):
        raise ValueError("candidate_patches must be a list")
    try:
        candidates = [CandidatePatch.model_validate(item) for item in raw_candidates]
    except ValidationError:
        raise ValueError("turn input contains an invalid candidate patch") from None
    _require_unique_candidate_ids(candidates, boundary="turn input")
    message = payload.get("message", run.goal)
    if not isinstance(message, str) or not message.strip():
        raise ValueError("turn input message must be nonblank")
    return candidates, message.strip()


def _merge_turn_result(
    current: list[CandidatePatch], proposed: TurnResult
) -> TurnResult:
    _require_unique_candidate_ids(proposed.candidate_patches, boundary="model output")
    merged = [item.model_copy(deep=True) for item in current]
    positions = {item.candidate_id: index for index, item in enumerate(merged)}
    warnings = list(proposed.warnings)
    for patch in proposed.candidate_patches:
        position = positions.get(patch.candidate_id)
        if position is None:
            if patch.source_type != "NEW":
                raise ValueError("model cannot introduce an EXISTING candidate")
            positions[patch.candidate_id] = len(merged)
            merged.append(_normalized_new_patch(patch))
            continue
        existing = merged[position]
        if existing.source_type != patch.source_type:
            raise ValueError("model cannot change candidate source_type")
        fields = dict(existing.fields)
        sources = dict(existing.field_sources)
        for name, value in patch.fields.items():
            old_value = fields.get(name)
            old_source = sources.get(name)
            if old_source == "USER":
                if old_value != value:
                    warnings.append(
                        f"candidate {patch.candidate_id} field {name} conflicts with USER value"
                    )
                continue
            if _empty_value(old_value) or old_source in {"AI", "SYSTEM"}:
                fields[name] = value
                sources[name] = patch.field_sources.get(name, "AI")
            elif old_value != value:
                warnings.append(
                    f"candidate {patch.candidate_id} field {name} suggestion was ignored"
                )
        merged[position] = CandidatePatch(
            candidate_id=existing.candidate_id,
            source_type=existing.source_type,
            fields=fields,
            field_sources=sources,
        )
    required = _missing_required_inputs(merged)
    if (
        RequiredInput.AMBIGUOUS_CANDIDATE in proposed.required_inputs
        and RequiredInput.AMBIGUOUS_CANDIDATE not in required
    ):
        required.append(RequiredInput.AMBIGUOUS_CANDIDATE)
    return TurnResult(
        assistant_message=_authoritative_assistant_message(required),
        candidate_patches=merged,
        required_inputs=required,
        review_readiness="NEEDS_INPUT" if required else "READY_FOR_REVIEW",
        warnings=warnings,
    )


def _authoritative_assistant_message(required: list[RequiredInput]) -> str:
    """Describe the merged draft state instead of repeating stale model prose."""
    if RequiredInput.AMBIGUOUS_CANDIDATE in required:
        return "请说明要修改哪一件候选拍品。"
    if not required:
        return "信息已完整。请检查右侧草稿和 AI 建议参数，确认无误后生成确认预览。"
    labels = {
        RequiredInput.TITLE: "拍品名称",
        RequiredInput.START_PRICE: "起拍价",
        RequiredInput.DURATION: "竞拍时长",
        RequiredInput.MAIN_IMAGE: "真实主图",
    }
    missing = "、".join(labels[item] for item in required if item in labels)
    return (
        f"竞拍草稿已生成。还需要你补充：{missing}。"
        "其余非必填参数已由 AI 自动补全，可在右侧编辑。"
    )


def _require_unique_candidate_ids(
    candidates: list[CandidatePatch], *, boundary: str
) -> None:
    candidate_ids = [item.candidate_id for item in candidates]
    if len(candidate_ids) != len(set(candidate_ids)):
        raise ValueError(f"{boundary} contains duplicate candidate IDs")


def _normalized_new_patch(patch: CandidatePatch) -> CandidatePatch:
    fields = dict(patch.fields)
    return CandidatePatch(
        candidate_id=patch.candidate_id,
        source_type="NEW",
        fields=fields,
        field_sources={name: patch.field_sources.get(name, "AI") for name in fields},
    )


def _missing_required_inputs(candidates: list[CandidatePatch]) -> list[RequiredInput]:
    if not candidates:
        return [RequiredInput.AMBIGUOUS_CANDIDATE]
    missing: set[RequiredInput] = set()
    for candidate in candidates:
        if candidate.source_type == "EXISTING":
            continue
        fields = candidate.fields
        sources = candidate.field_sources
        if not _explicit_user_value(fields, sources, "title"):
            missing.add(RequiredInput.TITLE)
        if not _explicit_user_value(fields, sources, "startPriceAmount", allow_zero=True):
            missing.add(RequiredInput.START_PRICE)
        if not _explicit_user_value(fields, sources, "durationSeconds"):
            missing.add(RequiredInput.DURATION)
        if not (
            _explicit_user_value(fields, sources, "mainImageAssetId")
            or _explicit_user_value(fields, sources, "mainImageUrl")
        ):
            missing.add(RequiredInput.MAIN_IMAGE)
    order = (
        RequiredInput.TITLE,
        RequiredInput.START_PRICE,
        RequiredInput.DURATION,
        RequiredInput.MAIN_IMAGE,
        RequiredInput.AMBIGUOUS_CANDIDATE,
    )
    return [item for item in order if item in missing]


def _explicit_user_value(
    fields: dict, sources: dict, name: str, *, allow_zero: bool = False
) -> bool:
    if sources.get(name) != "USER" or name not in fields:
        return False
    value = fields[name]
    if allow_zero and value == 0:
        return True
    return not _empty_value(value)


def _empty_value(value) -> bool:
    return value is None or value == "" or value == [] or value == {}


def _proposal_turn_result(proposal) -> TurnResult:
    patches = []
    seen_lot_ids: set[str] = set()
    duplicate_count = 0
    for lot in proposal.lots:
        lot_id = lot.lot_id.strip()
        if lot_id in seen_lot_ids:
            duplicate_count += 1
            continue
        seen_lot_ids.add(lot_id)
        fields = {
            "existingLotId": lot_id,
        }
        patches.append(
            CandidatePatch(
                candidate_id=lot_id,
                source_type="EXISTING",
                fields=fields,
                field_sources={name: "EXISTING" for name in fields},
            )
        )
    warnings = list(proposal.risks)
    if duplicate_count:
        warnings.append(
            f"库存不足：模型重复选择的 {duplicate_count} 个拍品位置已自动去重。"
        )
    return TurnResult(
        assistant_message=proposal.summary,
        candidate_patches=patches,
        required_inputs=[],
        review_readiness="READY_FOR_REVIEW",
        warnings=warnings,
    )
