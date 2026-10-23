"""Deterministic model adapter for harness and unit tests."""

from __future__ import annotations

from collections import deque
from collections.abc import Iterable
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
from app.harness.lot_draft_grounding import (
    candidate_id_for_run,
    explicit_lot_fields,
    new_lot_defaults,
)
from app.models.base import ModelError, ModelOutputError


class FakeModelExhaustedError(ModelError):
    """Raised when a deterministic fake has no remaining decision."""


class FakeModel:
    def __init__(self, decisions: Iterable[AgentDecision | TurnResult]) -> None:
        queued = tuple(decisions)
        if not all(
            isinstance(item, (ToolCallDecision, FinalProposalDecision, TurnResult))
            for item in queued
        ):
            raise TypeError("FakeModel outputs must be typed AgentDecision or TurnResult values")
        self._outputs = deque(queued)

    async def decide(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> AgentDecision:
        del max_output_tokens
        del context
        if not self._outputs:
            raise FakeModelExhaustedError("FakeModel decision queue is exhausted")
        output = self._outputs.popleft()
        if not isinstance(output, (ToolCallDecision, FinalProposalDecision)):
            raise TypeError("next FakeModel output is not an AgentDecision")
        return output

    async def draft_lot(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> TurnResult:
        del max_output_tokens
        del context
        if not self._outputs:
            raise FakeModelExhaustedError("FakeModel decision queue is exhausted")
        output = self._outputs.popleft()
        if not isinstance(output, TurnResult):
            raise TypeError("next FakeModel output is not a TurnResult")
        return output


class DeterministicPlanningModel:
    """Stateless mock provider suitable for concurrent Compose runs."""

    async def decide(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> AgentDecision:
        del max_output_tokens
        inventory = next(
            (item for item in context.observations if item.tool == "inventory.query"),
            None,
        )
        if inventory is None:
            return ToolCallDecision(
                kind="tool_call", tool="inventory.query", arguments={"limit": 50}
            )
        history = next(
            (
                item
                for item in context.observations
                if item.tool == "auction.history.query"
            ),
            None,
        )
        if history is None:
            return ToolCallDecision(
                kind="tool_call",
                tool="auction.history.query",
                arguments={"limit": 50},
            )
        lots = _inventory_lots(inventory.evidence.get("lots"))
        if not lots:
            raise ModelOutputError("mock model requires at least one inventory lot")
        plans: list[LotPlan] = []
        evidence: list[EvidenceRef] = []
        for sequence, lot in enumerate(lots[:10], start=1):
            lot_id = lot["id"]
            start_price = lot.get("start_price")
            amount = start_price.get("amount", 0) if isinstance(start_price, dict) else 0
            if not isinstance(amount, int) or amount < 0:
                amount = 0
            plans.append(
                LotPlan(
                    lot_id=lot_id,
                    sequence=sequence,
                    role="opening" if sequence == 1 else "supporting",
                    start_price_amount=amount,
                    increment_amount=0,
                    duration_seconds=300,
                    extension_seconds=30,
                    reason="Deterministic mock plan grounded in inventory.query",
                )
            )
            evidence.append(
                EvidenceRef(
                    tool="inventory.query",
                    reference_id=lot_id,
                    summary="Lot was returned by the tenant inventory query",
                )
            )
        return FinalProposalDecision(
            kind="final_proposal",
            proposal=SessionProposal(
                summary="Deterministic mock live-auction plan",
                strategy="balanced",
                estimated_duration_minutes=max(1, len(plans) * 5),
                lots=plans,
                evidence=evidence,
                risks=["Mock planning does not estimate demand from model inference"],
            ),
        )

    async def draft_lot(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> TurnResult:
        del max_output_tokens
        current = _turn_candidates(context)
        if len(current) > 1:
            return TurnResult(
                assistant_message="Please identify which candidate to update.",
                candidate_patches=[],
                required_inputs=[RequiredInput.AMBIGUOUS_CANDIDATE],
                review_readiness="NEEDS_INPUT",
                warnings=[],
            )
        candidate_id = (
            current[0].candidate_id
            if current
            else candidate_id_for_run(
                str(
                    next(
                        (
                            item.evidence.get("run_id")
                            for item in context.observations
                            if item.tool == "turn.input"
                        ),
                        "",
                    )
                )
            )
        )
        source_type = current[0].source_type if current else "NEW"
        explicit_fields = explicit_lot_fields(context.goal)
        fields = dict(explicit_fields)
        sources = {name: "USER" for name in explicit_fields}
        if not current and source_type == "NEW":
            defaults = new_lot_defaults(explicit_fields)
            fields = {**defaults, **explicit_fields}
            sources = {**{name: "AI" for name in defaults}, **sources}
        missing = []
        if source_type == "NEW":
            if "title" not in fields:
                missing.append(RequiredInput.TITLE)
            if "startPriceAmount" not in fields:
                missing.append(RequiredInput.START_PRICE)
            if "durationSeconds" not in fields:
                missing.append(RequiredInput.DURATION)
            if not ({"mainImageAssetId", "mainImageUrl"} & fields.keys()):
                missing.append(RequiredInput.MAIN_IMAGE)
        return TurnResult(
            assistant_message="Deterministic mock lot draft",
            candidate_patches=[
                CandidatePatch(
                    candidate_id=candidate_id,
                    source_type=source_type,
                    fields=fields,
                    field_sources=sources,
                )
            ],
            required_inputs=missing,
            review_readiness="NEEDS_INPUT" if missing else "READY_FOR_REVIEW",
            warnings=[],
        )


def _inventory_lots(value) -> list[dict]:
    if not isinstance(value, list):
        return []
    return [
        lot
        for lot in value
        if isinstance(lot, dict)
        and isinstance(lot.get("id"), str)
        and lot["id"].strip()
    ]


def _turn_candidates(context: AgentContext) -> list[CandidatePatch]:
    observation = next(
        (item for item in context.observations if item.tool == "turn.input"), None
    )
    if observation is None:
        return []
    raw = observation.evidence.get("candidate_patches")
    if not isinstance(raw, list):
        return []
    candidates = []
    for item in raw:
        try:
            candidates.append(CandidatePatch.model_validate(item))
        except (TypeError, ValueError):
            continue
    return candidates
