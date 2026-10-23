"""Checkpointed LangGraph state for one planning run."""

from __future__ import annotations

from typing import TypedDict

from app.domain.models import AgentDecision, Observation, SessionProposal
from app.harness.context import RunError, RunInput, RunStatus


class AgentState(TypedDict):
    run: RunInput
    skill_name: str
    step_count: int
    validation_retry_count: int
    observations: list[Observation]
    decision: AgentDecision | None
    proposal: SessionProposal | None
    status: RunStatus
    error: RunError | None
