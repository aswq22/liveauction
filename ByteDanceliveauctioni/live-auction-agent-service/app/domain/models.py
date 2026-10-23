"""Strict domain models for agent decisions, proposals, and tool evidence."""

from __future__ import annotations

from enum import StrEnum
from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, JsonValue, StringConstraints, TypeAdapter


NonBlankText = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1)]
StrategyTier = Literal["conservative", "balanced", "aggressive"]


class StrictModel(BaseModel):
    """Base for trusted boundary objects: no coercion and no unknown fields."""

    model_config = ConfigDict(extra="forbid", strict=True)


class RunContext(StrictModel):
    """Run correlation sent in protobuf bodies, never as authorization metadata."""

    run_id: NonBlankText
    request_id: NonBlankText | None = None
    trace_id: NonBlankText | None = None
    tenant_id: NonBlankText | None = None
    operator_id: NonBlankText | None = None


class Observation(StrictModel):
    """A safe, JSON-compatible evidence payload returned by one registered tool."""

    tool: NonBlankText
    evidence: dict[str, JsonValue]


class AgentContext(StrictModel):
    """Complete structured input passed to a model decision boundary."""

    goal: NonBlankText
    skill_name: NonBlankText
    skill_instructions: NonBlankText
    observations: list[Observation] = Field(default_factory=list)


class EvidenceRef(StrictModel):
    tool: NonBlankText
    reference_id: NonBlankText
    summary: NonBlankText


class LotPlan(StrictModel):
    lot_id: NonBlankText
    sequence: int = Field(ge=1)
    role: NonBlankText
    start_price_amount: int = Field(ge=0)
    increment_amount: int = Field(ge=0)
    duration_seconds: int = Field(gt=0, le=86_400)
    extension_seconds: int = Field(gt=0, le=3_600)
    reason: NonBlankText


class SessionProposal(StrictModel):
    summary: NonBlankText
    strategy: StrategyTier
    estimated_duration_minutes: int = Field(gt=0, le=1_440)
    lots: list[LotPlan] = Field(min_length=1)
    evidence: list[EvidenceRef]
    risks: list[NonBlankText]


class RequiredInput(StrEnum):
    """Stable user inputs needed before a draft can be reviewed."""

    TITLE = "TITLE"
    START_PRICE = "START_PRICE"
    DURATION = "DURATION"
    MAIN_IMAGE = "MAIN_IMAGE"
    AMBIGUOUS_CANDIDATE = "AMBIGUOUS_CANDIDATE"


class CandidatePatch(StrictModel):
    """One proposed candidate update at the model/harness boundary."""

    candidate_id: NonBlankText
    source_type: Literal["EXISTING", "NEW"]
    fields: dict[str, JsonValue]
    field_sources: dict[str, Literal["USER", "AI", "SYSTEM", "EXISTING"]]


class TurnResult(StrictModel):
    """Complete structured result returned for one conversational turn."""

    assistant_message: str
    candidate_patches: list[CandidatePatch]
    required_inputs: list[RequiredInput]
    review_readiness: Literal["NEEDS_INPUT", "READY_FOR_REVIEW"]
    warnings: list[str]


class ToolCallDecision(StrictModel):
    kind: Literal["tool_call"]
    tool: NonBlankText
    arguments: dict[str, JsonValue]


class FinalProposalDecision(StrictModel):
    kind: Literal["final_proposal"]
    proposal: SessionProposal


AgentDecision = Annotated[
    ToolCallDecision | FinalProposalDecision,
    Field(discriminator="kind"),
]
AGENT_DECISION_ADAPTER = TypeAdapter(AgentDecision)
