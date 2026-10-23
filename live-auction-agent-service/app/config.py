"""Environment-backed configuration for the agent runtime."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
import logging
import os

from app.models.base import TokenLimitField


logger = logging.getLogger(__name__)


class ModelProvider(StrEnum):
    MOCK = "mock"
    OPENAI_COMPATIBLE = "openai-compatible"
    DEEPSEEK = "deepseek"


@dataclass(frozen=True, slots=True)
class Settings:
    grpc_addr: str
    tool_grpc_addr: str
    database_dsn: str = field(repr=False)
    internal_service_token: str = field(repr=False)
    model_provider: ModelProvider
    model_base_url: str
    model_name: str
    model_api_key: str = field(repr=False)
    model_timeout_seconds: int
    max_steps: int
    token_budget: int
    max_active_runs_global: int = 64
    max_active_runs_per_tenant: int = 8
    max_subscribers_global: int = 256
    max_subscribers_per_run: int = 16
    model_token_limit_field: TokenLimitField = TokenLimitField.MAX_COMPLETION_TOKENS

    @classmethod
    def from_env(cls) -> Settings:
        model_provider = _model_provider_env()
        model_base_url = _optional_env("AGENT_MODEL_BASE_URL", "")
        model_name = _optional_env("AGENT_MODEL_NAME", "")
        model_api_key = _optional_env("AGENT_MODEL_API_KEY", "")

        if model_provider is not ModelProvider.MOCK:
            _require_value("AGENT_MODEL_BASE_URL", model_base_url)
            _require_value("AGENT_MODEL_NAME", model_name)
            _require_value("AGENT_MODEL_API_KEY", model_api_key)

        default_limit_field = (
            TokenLimitField.MAX_TOKENS
            if model_provider is ModelProvider.DEEPSEEK
            else TokenLimitField.MAX_COMPLETION_TOKENS
        )
        token_limit_field = _token_limit_field_env(default_limit_field)

        return cls(
            grpc_addr=_optional_env("AUCTION_AGENT_GRPC_ADDR", ":19081"),
            tool_grpc_addr=_optional_env("AUCTION_TOOL_GRPC_ADDR", "localhost:19082"),
            database_dsn=_required_env("AGENT_DATABASE_DSN"),
            internal_service_token=_required_env("AUCTION_INTERNAL_SERVICE_TOKEN"),
            model_provider=model_provider,
            model_base_url=model_base_url,
            model_name=model_name,
            model_api_key=model_api_key,
            model_token_limit_field=token_limit_field,
            model_timeout_seconds=_positive_int_env("AGENT_MODEL_TIMEOUT_SECONDS", 30),
            max_steps=_positive_int_env("AGENT_MAX_STEPS", 8),
            token_budget=_positive_int_env("AGENT_TOKEN_BUDGET", 8192),
            max_active_runs_global=_positive_int_env(
                "AGENT_MAX_ACTIVE_RUNS_GLOBAL", 64
            ),
            max_active_runs_per_tenant=_positive_int_env(
                "AGENT_MAX_ACTIVE_RUNS_PER_TENANT", 8
            ),
            max_subscribers_global=_positive_int_env(
                "AGENT_MAX_SUBSCRIBERS_GLOBAL", 256
            ),
            max_subscribers_per_run=_positive_int_env(
                "AGENT_MAX_SUBSCRIBERS_PER_RUN", 16
            ),
        )


def log_settings_configuration(settings: Settings) -> None:
    """Log configuration state without exposing credentials or service tokens."""
    logger.info(
        "Agent model configuration loaded: model_provider=%s api_key_configured=%s",
        settings.model_provider,
        bool(settings.model_api_key),
    )


def _optional_env(name: str, default: str) -> str:
    return os.getenv(name, default).strip()


def _required_env(name: str) -> str:
    value = _optional_env(name, "")
    _require_value(name, value)
    return value


def _require_value(name: str, value: str) -> None:
    if not value:
        raise ValueError(f"{name} must be configured")


def _positive_int_env(name: str, default: int) -> int:
    raw_value = _optional_env(name, str(default))
    try:
        value = int(raw_value)
    except ValueError as error:
        raise ValueError(f"{name} must be a positive integer") from error
    if value <= 0:
        raise ValueError(f"{name} must be a positive integer")
    return value


def _model_provider_env() -> ModelProvider:
    raw_value = _optional_env("AGENT_MODEL_PROVIDER", ModelProvider.MOCK.value)
    try:
        return ModelProvider(raw_value)
    except ValueError as error:
        raise ValueError(
            "AGENT_MODEL_PROVIDER must be one of: mock, openai-compatible, deepseek"
        ) from error


def _token_limit_field_env(default: TokenLimitField) -> TokenLimitField:
    raw_value = _optional_env("AGENT_MODEL_TOKEN_LIMIT_FIELD", default.value)
    try:
        return TokenLimitField(raw_value)
    except ValueError as error:
        raise ValueError(
            "AGENT_MODEL_TOKEN_LIMIT_FIELD must be one of: "
            "max_completion_tokens, max_tokens"
        ) from error
