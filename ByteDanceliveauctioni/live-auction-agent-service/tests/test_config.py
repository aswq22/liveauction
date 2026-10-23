import logging

import pytest

from app.config import Settings, log_settings_configuration


def clear_agent_environment(monkeypatch):
    for name in (
        "AUCTION_AGENT_GRPC_ADDR",
        "AUCTION_TOOL_GRPC_ADDR",
        "AGENT_DATABASE_DSN",
        "AUCTION_INTERNAL_SERVICE_TOKEN",
        "AGENT_MODEL_PROVIDER",
        "AGENT_MODEL_BASE_URL",
        "AGENT_MODEL_NAME",
        "AGENT_MODEL_API_KEY",
        "AGENT_MODEL_TOKEN_LIMIT_FIELD",
        "AGENT_MODEL_TIMEOUT_SECONDS",
        "AGENT_MAX_STEPS",
        "AGENT_TOKEN_BUDGET",
        "AGENT_MAX_ACTIVE_RUNS_GLOBAL",
        "AGENT_MAX_ACTIVE_RUNS_PER_TENANT",
        "AGENT_MAX_SUBSCRIBERS_GLOBAL",
        "AGENT_MAX_SUBSCRIBERS_PER_RUN",
    ):
        monkeypatch.delenv(name, raising=False)


def test_settings_require_service_token(monkeypatch):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent@localhost/auction")

    with pytest.raises(ValueError, match="AUCTION_INTERNAL_SERVICE_TOKEN"):
        Settings.from_env()


def test_settings_reject_whitespace_only_service_token(monkeypatch):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent@localhost/auction")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", " \t ")

    with pytest.raises(ValueError, match="AUCTION_INTERNAL_SERVICE_TOKEN"):
        Settings.from_env()


def test_settings_require_database_dsn(monkeypatch):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")

    with pytest.raises(ValueError, match="AGENT_DATABASE_DSN"):
        Settings.from_env()


def test_settings_reject_whitespace_only_database_dsn(monkeypatch):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", " \t ")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")

    with pytest.raises(ValueError, match="AGENT_DATABASE_DSN"):
        Settings.from_env()


def test_mock_provider_uses_defaults_without_model_credentials(monkeypatch):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", " postgresql://agent@localhost/auction ")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")

    settings = Settings.from_env()

    assert settings.grpc_addr == ":19081"
    assert settings.tool_grpc_addr == "localhost:19082"
    assert settings.database_dsn == "postgresql://agent@localhost/auction"
    assert settings.internal_service_token == "internal-token"
    assert settings.model_provider == "mock"
    assert settings.model_base_url == ""
    assert settings.model_name == ""
    assert settings.model_api_key == ""
    assert settings.model_token_limit_field.value == "max_completion_tokens"
    assert settings.model_timeout_seconds == 30
    assert settings.max_steps == 8
    assert settings.token_budget == 8192
    assert settings.max_active_runs_global == 64
    assert settings.max_active_runs_per_tenant == 8
    assert settings.max_subscribers_global == 256
    assert settings.max_subscribers_per_run == 16


@pytest.mark.parametrize(
    ("name", "value"),
    (
        ("AGENT_MODEL_BASE_URL", ""),
        ("AGENT_MODEL_NAME", ""),
        ("AGENT_MODEL_API_KEY", ""),
    ),
)
def test_non_mock_provider_requires_every_model_credential(monkeypatch, name, value):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent@localhost/auction")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "openai-compatible")
    monkeypatch.setenv("AGENT_MODEL_BASE_URL", "https://models.example/v1")
    monkeypatch.setenv("AGENT_MODEL_NAME", "test-model")
    monkeypatch.setenv("AGENT_MODEL_API_KEY", "secret-api-key")
    monkeypatch.setenv(name, value)

    with pytest.raises(ValueError, match=name):
        Settings.from_env()


def test_deepseek_defaults_to_max_tokens_and_custom_endpoint_can_select_it(monkeypatch):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent@localhost/auction")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "deepseek")
    monkeypatch.setenv("AGENT_MODEL_BASE_URL", "https://api.deepseek.example/v1")
    monkeypatch.setenv("AGENT_MODEL_NAME", "deepseek-chat")
    monkeypatch.setenv("AGENT_MODEL_API_KEY", "secret-api-key")

    deepseek = Settings.from_env()
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "openai-compatible")
    monkeypatch.setenv("AGENT_MODEL_TOKEN_LIMIT_FIELD", "max_tokens")
    custom = Settings.from_env()

    assert deepseek.model_token_limit_field.value == "max_tokens"
    assert custom.model_token_limit_field.value == "max_tokens"


@pytest.mark.parametrize(
    ("name", "value"),
    (
        ("AGENT_MODEL_PROVIDER", "unknown-provider"),
        ("AGENT_MODEL_TOKEN_LIMIT_FIELD", "fragile-field"),
    ),
)
def test_settings_reject_unknown_model_contract_values(monkeypatch, name, value):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent@localhost/auction")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")
    monkeypatch.setenv("AGENT_MODEL_PROVIDER", "openai-compatible")
    monkeypatch.setenv("AGENT_MODEL_BASE_URL", "https://models.example/v1")
    monkeypatch.setenv("AGENT_MODEL_NAME", "test-model")
    monkeypatch.setenv("AGENT_MODEL_API_KEY", "secret-api-key")
    monkeypatch.setenv(name, value)

    with pytest.raises(ValueError, match=name):
        Settings.from_env()


def test_configuration_logging_never_exposes_api_key_or_service_token(monkeypatch, caplog):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent@localhost/auction")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")
    monkeypatch.setenv("AGENT_MODEL_API_KEY", "secret-api-key")
    settings = Settings.from_env()

    with caplog.at_level(logging.INFO):
        log_settings_configuration(settings)

    assert "api_key_configured=True" in caplog.text
    assert "secret-api-key" not in caplog.text
    assert "internal-token" not in caplog.text


def test_settings_repr_redacts_sensitive_values(monkeypatch):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent:dsn-secret@localhost/auction")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")
    monkeypatch.setenv("AGENT_MODEL_API_KEY", "secret-api-key")

    settings_repr = repr(Settings.from_env())

    assert "dsn-secret" not in settings_repr
    assert "internal-token" not in settings_repr
    assert "secret-api-key" not in settings_repr
    assert "ModelProvider.MOCK" in settings_repr


@pytest.mark.parametrize(
    ("name", "value"),
    (
        ("AGENT_MODEL_TIMEOUT_SECONDS", "0"),
        ("AGENT_MODEL_TIMEOUT_SECONDS", "not-a-number"),
        ("AGENT_MAX_STEPS", "-1"),
        ("AGENT_TOKEN_BUDGET", "0"),
        ("AGENT_MAX_ACTIVE_RUNS_GLOBAL", "0"),
        ("AGENT_MAX_ACTIVE_RUNS_PER_TENANT", "-1"),
        ("AGENT_MAX_SUBSCRIBERS_GLOBAL", "wat"),
        ("AGENT_MAX_SUBSCRIBERS_PER_RUN", "0"),
    ),
)
def test_settings_reject_invalid_non_positive_numeric_values(monkeypatch, name, value):
    clear_agent_environment(monkeypatch)
    monkeypatch.setenv("AGENT_DATABASE_DSN", "postgresql://agent@localhost/auction")
    monkeypatch.setenv("AUCTION_INTERNAL_SERVICE_TOKEN", "internal-token")
    monkeypatch.setenv(name, value)

    with pytest.raises(ValueError, match=name):
        Settings.from_env()
