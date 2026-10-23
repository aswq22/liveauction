"""Provider-neutral model adapter interface and typed failures."""

from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum
import math
from typing import Protocol

from app.domain.models import AgentContext, AgentDecision, TurnResult


class ModelError(RuntimeError):
    """Base class for safe model-boundary failures."""


class ModelTimeoutError(ModelError):
    """Raised when the configured provider deadline expires."""


class ModelOutputError(ModelError):
    """Raised when a provider does not return a complete typed decision."""


class ModelProviderError(ModelError):
    """Raised when the provider fails for a reason safe to expose to the harness."""


class TokenBudgetExceededError(ModelError):
    """Raised when a run consumes or cannot safely fit its token budget."""


class TokenLimitField(StrEnum):
    """Supported Chat Completions output-cap request fields."""

    MAX_COMPLETION_TOKENS = "max_completion_tokens"
    MAX_TOKENS = "max_tokens"


@dataclass(frozen=True, slots=True)
class TokenUsage:
    input_tokens: int
    output_tokens: int


class TokenBudget:
    """Fail-closed cumulative token accounting for one run."""

    def __init__(self, limit: int) -> None:
        if limit <= 0:
            raise ValueError("token budget must be positive")
        self._limit = limit
        self._used = 0
        self._reserved_input = 0

    @property
    def used_tokens(self) -> int:
        return self._used

    @property
    def remaining_tokens(self) -> int:
        return max(0, self._limit - self._used)

    def reserve_input(self, value: str) -> int:
        estimated = max(1, math.ceil(len(value.encode("utf-8")) / 3))
        if self._used + estimated >= self._limit:
            raise TokenBudgetExceededError("agent token budget exceeded")
        self._used += estimated
        self._reserved_input = estimated
        return estimated

    def commit_usage(self, *, input_tokens: int, output_tokens: int) -> None:
        if input_tokens < 0 or output_tokens < 0:
            raise ValueError("token usage cannot be negative")
        self._used += input_tokens + output_tokens - self._reserved_input
        self._reserved_input = 0
        if self._used > self._limit:
            raise TokenBudgetExceededError("agent token budget exceeded")

    def commit_estimated_output(self, value: str) -> None:
        estimated = max(1, math.ceil(len(value.encode("utf-8")) / 3))
        self._reserved_input = 0
        self._used += estimated
        if self._used > self._limit:
            raise TokenBudgetExceededError("agent token budget exceeded")


class ModelAdapter(Protocol):
    async def decide(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> AgentDecision:
        """Return exactly one complete, typed agent decision."""
        ...

    async def draft_lot(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> TurnResult:
        """Return exactly one complete, typed conversational lot-draft result."""
        ...
