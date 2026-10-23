"""Typed inputs and outcomes for one auction-agent execution."""

from __future__ import annotations

from enum import StrEnum
from typing import Annotated

from pydantic import Field, StringConstraints

from app.domain.models import RunContext, SessionProposal, StrictModel, TurnResult


NonBlankText = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1)]


class RunStatus(StrEnum):
    CREATED = "CREATED"
    PLANNING = "PLANNING"
    COLLECTING_CONTEXT = "COLLECTING_CONTEXT"
    GENERATING_PROPOSAL = "GENERATING_PROPOSAL"
    VERIFYING = "VERIFYING"
    COMPLETED = "COMPLETED"
    FAILED = "FAILED"
    CANCELLED = "CANCELLED"


class RunType(StrEnum):
    SESSION_PLAN = "SESSION_PLAN"
    LOT_DRAFT_ASSIST = "LOT_DRAFT_ASSIST"


class RunInput(StrictModel):
    context: RunContext
    goal: NonBlankText
    max_steps: int = Field(gt=0)
    run_type: RunType = RunType.SESSION_PLAN
    turn_input_json: str = ""


class RunError(StrictModel):
    code: NonBlankText
    message: NonBlankText


class AgentRunSummary(StrictModel):
    run_id: NonBlankText
    status: RunStatus
    proposal: SessionProposal | None
    error_code: str
    error_message: str
    turn_result: TurnResult | None = None
