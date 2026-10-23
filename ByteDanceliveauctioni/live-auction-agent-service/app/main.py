"""Production wiring and lifecycle for the auction-agent gRPC runtime."""

from __future__ import annotations

import asyncio
from collections.abc import Callable
from contextlib import AsyncExitStack
from dataclasses import dataclass
import logging
from pathlib import Path
import signal
from typing import Any

import grpc

from app.config import Settings, log_settings_configuration
from app.generated.proto.agent.v1 import runtime_pb2_grpc
from app.harness.harness import AuctionAgentHarness
from app.models.base import ModelAdapter
from app.models.fake import DeterministicPlanningModel
from app.models.openai_compat import OpenAICompatibleModel
from app.observability.tracing import Tracing
from app.runtime.repository import PostgresRuntimeRepository
from app.runtime.service import AgentRuntimeService
from app.skills.registry import SkillRegistry
from app.tools.grpc_client import GrpcToolClient
from app.tools.registry import ToolRegistry


logger = logging.getLogger(__name__)
ModelFactory = Callable[[], ModelAdapter]


@dataclass(frozen=True, slots=True)
class ApplicationFactories:
    """Injectable constructors for lifecycle tests and alternate deployments."""

    repository: Callable[[str], Any]
    tool_client: Callable[..., Any]
    checkpointer: Callable[[str], Any]
    tracer_provider: Callable[[], Any]
    server: Callable[[], Any]
    runtime: Callable[..., Any]


def create_model_factory(settings: Settings) -> ModelFactory:
    if settings.model_provider == "mock":
        return DeterministicPlanningModel

    def create_openai_model() -> ModelAdapter:
        return OpenAICompatibleModel(
            model=settings.model_name,
            timeout_seconds=settings.model_timeout_seconds,
            base_url=settings.model_base_url,
            api_key=settings.model_api_key,
            token_limit_field=settings.model_token_limit_field,
        )

    return create_openai_model


def normalize_grpc_address(configured: str) -> str:
    normalized = configured.strip()
    if normalized.startswith(":"):
        return f"[::]{normalized}"
    return normalized


async def serve(
    settings: Settings,
    *,
    stop_event: asyncio.Event | None = None,
    factories: ApplicationFactories | None = None,
) -> None:
    application = factories or _default_application_factories()

    log_settings_configuration(settings)
    shutdown_event = stop_event or asyncio.Event()
    async with AsyncExitStack() as stack:
        repository = application.repository(settings.database_dsn)
        stack.push_async_callback(repository.close)
        await repository.setup()
        await repository.fail_nonterminal_runs_on_restart()

        tool_client = application.tool_client(
            target=settings.tool_grpc_addr,
            internal_token=settings.internal_service_token,
        )
        stack.push_async_callback(tool_client.close)

        checkpointer = await stack.enter_async_context(
            application.checkpointer(settings.database_dsn)
        )
        await checkpointer.setup()

        model_factory = create_model_factory(settings)
        skill_registry = SkillRegistry(Path(__file__).parent / "skills" / "packages")
        tool_registry = ToolRegistry(tool_client)
        tracer_provider = application.tracer_provider()
        tracer_shutdown = getattr(tracer_provider, "shutdown", None)
        if callable(tracer_shutdown):
            stack.callback(tracer_shutdown)
        tracer = tracer_provider.get_tracer("live-auction-agent-service")

        def harness_factory(parent_context, event_sink):
            return AuctionAgentHarness(
                model=model_factory(),
                tools=tool_registry,
                skills=skill_registry,
                checkpointer=checkpointer,
                cancellation=repository,
                tracing=Tracing(tracer=tracer, parent_context=parent_context),
                event_sink=event_sink,
                token_budget=settings.token_budget,
            )

        runtime = application.runtime(
            repository=repository,
            harness_factory=harness_factory,
            internal_token=settings.internal_service_token,
            max_steps=settings.max_steps,
            max_active_runs_global=settings.max_active_runs_global,
            max_active_runs_per_tenant=settings.max_active_runs_per_tenant,
            max_subscribers_global=settings.max_subscribers_global,
            max_subscribers_per_run=settings.max_subscribers_per_run,
        )
        stack.push_async_callback(runtime.shutdown)

        server = application.server()
        stack.push_async_callback(server.stop, grace=10)
        runtime_pb2_grpc.add_AgentRuntimeServiceServicer_to_server(runtime, server)
        bound_port = server.add_insecure_port(normalize_grpc_address(settings.grpc_addr))
        if bound_port == 0:
            raise RuntimeError("agent runtime gRPC address could not be bound")
        if stop_event is None:
            _install_signal_handlers(shutdown_event)
        await server.start()
        logger.info("agent runtime gRPC server started")
        await shutdown_event.wait()


def _default_application_factories() -> ApplicationFactories:
    from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
    from opentelemetry.sdk.trace import TracerProvider

    return ApplicationFactories(
        repository=PostgresRuntimeRepository,
        tool_client=GrpcToolClient,
        checkpointer=AsyncPostgresSaver.from_conn_string,
        tracer_provider=TracerProvider,
        server=grpc.aio.server,
        runtime=AgentRuntimeService,
    )


def _install_signal_handlers(stop_event: asyncio.Event) -> None:
    loop = asyncio.get_running_loop()
    for signum in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(signum, stop_event.set)
        except (NotImplementedError, RuntimeError):
            signal.signal(
                signum,
                lambda *_args, event=stop_event, event_loop=loop: event_loop.call_soon_threadsafe(
                    event.set
                ),
            )


def main() -> None:
    logging.basicConfig(level=logging.INFO)
    try:
        asyncio.run(serve(Settings.from_env()))
    except (KeyboardInterrupt, SystemExit):
        raise
    except Exception:
        logger.error("agent runtime failed to start or stopped unexpectedly")
        raise SystemExit(1) from None


if __name__ == "__main__":
    main()
