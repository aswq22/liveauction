"""Strict JSON adapter for OpenAI-compatible Chat Completions endpoints."""

from __future__ import annotations

import json
from typing import Any

from openai import APITimeoutError, AsyncOpenAI, OpenAIError
from pydantic import ValidationError

from app.domain.models import (
    AGENT_DECISION_ADAPTER,
    AgentContext,
    AgentDecision,
    TurnResult,
)
from app.models.base import (
    ModelOutputError,
    ModelProviderError,
    ModelTimeoutError,
    TokenLimitField,
    TokenUsage,
)


_DECISION_SCHEMA = json.dumps(
    AGENT_DECISION_ADAPTER.json_schema(), ensure_ascii=True, separators=(",", ":")
)
_SYSTEM_MESSAGE = (
    "Return exactly one JSON object matching the supplied AgentDecision schema. "
    "Do not wrap JSON in prose or a code fence. Schema: "
    f"{_DECISION_SCHEMA}"
)
_TURN_SCHEMA = json.dumps(
    TurnResult.model_json_schema(), ensure_ascii=True, separators=(",", ":")
)
_LOT_DRAFT_SYSTEM_MESSAGE = (
    "Return exactly one JSON object matching the supplied TurnResult schema. "
    "Do not wrap JSON in prose or a code fence. A NEW lot has four explicit "
    "user-required fields: title, startPriceAmount, durationSeconds, and a main "
    "image supplied as mainImageAssetId or mainImageUrl. Never infer or invent "
    "those four fields when the user did not explicitly provide them; report the "
    "corresponding TITLE, START_PRICE, DURATION, or MAIN_IMAGE required input. "
    "Convert explicit prices stated in 人民币元 to integer cents by multiplying "
    "by 100, including both label-first and amount-first phrases such as 起拍价1000 "
    "and 1000元起拍 or 以1000元起拍价格. Convert duration phrases such as "
    "竞拍50分钟, 拍50分钟, or 持续半小时 to integer seconds. "
    "Preserve explicit minimum increment, cap price, deposit, stock, category, "
    "and anti-snipe window, extension, and maximum extension count values. "
    "In an explicit naming phrase such as 名为aa的水杯, use exactly aa as "
    "the title; 水杯 is only the product type. "
    "A request to generate or rewrite an introduction, description, copy, tags, "
    "or category updates a matching current candidate and is never a new title. "
    "A date or clock time such "
    "as 下午3点 is scheduling context, never a title; extract the title from the "
    "item noun phrase following 拍卖 or 上架. Preserve candidate IDs and "
    "label fields extracted directly from the current user message as USER; use AI "
    "only for optional inferred suggestions. Schema: "
    f"{_TURN_SCHEMA}"
)


class OpenAICompatibleModel:
    def __init__(
        self,
        *,
        model: str,
        timeout_seconds: float,
        client: Any | None = None,
        base_url: str | None = None,
        api_key: str | None = None,
        token_limit_field: TokenLimitField = TokenLimitField.MAX_COMPLETION_TOKENS,
    ) -> None:
        normalized_model = model.strip()
        if not normalized_model:
            raise ValueError("model must be nonblank")
        if timeout_seconds <= 0:
            raise ValueError("timeout_seconds must be positive")
        if client is None:
            if not base_url or not base_url.strip():
                raise ValueError("base_url must be nonblank when client is not injected")
            if not api_key or not api_key.strip():
                raise ValueError("api_key must be nonblank when client is not injected")
            client = AsyncOpenAI(base_url=base_url.strip(), api_key=api_key.strip())
        self._client = client
        self._model = normalized_model
        self._timeout_seconds = timeout_seconds
        self._token_limit_field = TokenLimitField(token_limit_field)
        self.last_usage: TokenUsage | None = None

    def budget_input_text(self, context: AgentContext) -> str:
        """Return all provider-bound input for fallback token accounting."""
        return _SYSTEM_MESSAGE + "\n" + context.model_dump_json()

    def lot_budget_input_text(self, context: AgentContext) -> str:
        """Return provider input used by the conversational draft branch."""
        return _LOT_DRAFT_SYSTEM_MESSAGE + "\n" + context.model_dump_json()

    async def decide(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> AgentDecision:
        try:
            content = await self._complete_json(
                context,
                system_message=_SYSTEM_MESSAGE,
                max_output_tokens=max_output_tokens,
            )
            return AGENT_DECISION_ADAPTER.validate_json(content)
        except (ModelOutputError, TypeError, ValueError, ValidationError):
            raise ModelOutputError("model did not return a valid AgentDecision") from None

    async def draft_lot(
        self, context: AgentContext, *, max_output_tokens: int | None = None
    ) -> TurnResult:
        try:
            content = await self._complete_json(
                context,
                system_message=_LOT_DRAFT_SYSTEM_MESSAGE,
                max_output_tokens=max_output_tokens,
            )
            return TurnResult.model_validate_json(content)
        except (ModelOutputError, TypeError, ValueError, ValidationError):
            raise ModelOutputError("model did not return a valid TurnResult") from None

    async def _complete_json(
        self,
        context: AgentContext,
        *,
        system_message: str,
        max_output_tokens: int | None,
    ) -> str:
        self.last_usage = None
        request: dict[str, Any] = {
            "model": self._model,
            "messages": [
                {"role": "system", "content": system_message},
                {"role": "user", "content": context.model_dump_json()},
            ],
            "response_format": {"type": "json_object"},
            "timeout": self._timeout_seconds,
        }
        if max_output_tokens is not None:
            if max_output_tokens <= 0:
                raise ValueError("max_output_tokens must be positive")
            request[self._token_limit_field.value] = max_output_tokens
        try:
            response = await self._client.chat.completions.create(**request)
        except (APITimeoutError, TimeoutError):
            raise ModelTimeoutError("model request timed out") from None
        except OpenAIError:
            raise ModelProviderError("model provider request failed") from None

        try:
            content = response.choices[0].message.content
            if not isinstance(content, str) or not content.strip():
                raise ValueError("missing content")
            usage = getattr(response, "usage", None)
            prompt_tokens = getattr(usage, "prompt_tokens", None)
            completion_tokens = getattr(usage, "completion_tokens", None)
            if isinstance(prompt_tokens, int) and isinstance(completion_tokens, int):
                self.last_usage = TokenUsage(prompt_tokens, completion_tokens)
            return content
        except (AttributeError, IndexError, TypeError, ValueError, ValidationError):
            raise ModelOutputError("model did not return complete JSON content") from None
