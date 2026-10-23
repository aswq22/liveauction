from __future__ import annotations

import asyncio

import pytest

from app.config import Settings
from app.main import ApplicationFactories, serve


pytestmark = pytest.mark.anyio


@pytest.fixture
def anyio_backend():
    return "asyncio"


def settings() -> Settings:
    return Settings(
        grpc_addr=":19081",
        tool_grpc_addr="localhost:19082",
        database_dsn="postgresql://agent@localhost/auction",
        internal_service_token="internal-token",
        model_provider="mock",
        model_base_url="",
        model_name="",
        model_api_key="",
        model_timeout_seconds=30,
        max_steps=8,
        token_budget=8192,
    )


class FakeRepository:
    def __init__(self, calls: list[str], *, fail_setup: bool = False) -> None:
        self.calls = calls
        self.fail_setup = fail_setup

    async def setup(self) -> None:
        self.calls.append("repository.setup")
        if self.fail_setup:
            raise RuntimeError("repository setup failed")

    async def close(self) -> None:
        self.calls.append("repository.close")

    async def fail_nonterminal_runs_on_restart(self):
        self.calls.append("repository.restart_sweep")
        return []


class FakeToolClient:
    def __init__(self, calls: list[str]) -> None:
        self.calls = calls

    async def close(self) -> None:
        self.calls.append("tool.close")


class FakeCheckpointer:
    def __init__(self, calls: list[str], *, fail_setup: bool = False) -> None:
        self.calls = calls
        self.fail_setup = fail_setup

    async def __aenter__(self):
        self.calls.append("checkpointer.enter")
        return self

    async def __aexit__(self, exc_type, exc, traceback) -> None:
        del exc_type, exc, traceback
        self.calls.append("checkpointer.exit")

    async def setup(self) -> None:
        self.calls.append("checkpointer.setup")
        if self.fail_setup:
            raise RuntimeError("checkpointer setup failed")


class FakeTracerProvider:
    def __init__(self, calls: list[str]) -> None:
        self.calls = calls

    def get_tracer(self, name: str):
        del name
        return object()

    def shutdown(self) -> None:
        self.calls.append("tracer.shutdown")


class FakeServer:
    def __init__(
        self,
        calls: list[str],
        *,
        fail_start: bool = False,
        fail_stop: bool = False,
    ) -> None:
        self.calls = calls
        self.fail_start = fail_start
        self.fail_stop = fail_stop
        self.started = asyncio.Event()

    def add_generic_rpc_handlers(self, handlers) -> None:
        del handlers

    def add_registered_method_handlers(self, service_name, handlers) -> None:
        del service_name, handlers

    def add_insecure_port(self, address: str) -> int:
        assert address == "[::]:19081"
        self.calls.append("server.bind")
        return 19081

    async def start(self) -> None:
        self.calls.append("server.start")
        self.started.set()
        if self.fail_start:
            raise RuntimeError("server start failed")

    async def stop(self, grace: int) -> None:
        assert grace == 10
        self.calls.append("server.stop")
        if self.fail_stop:
            raise RuntimeError("server stop failed")


class FakeRuntime:
    def __init__(self, calls: list[str]) -> None:
        self.calls = calls

    async def StartRun(self, request, context):
        del request, context

    async def GetRun(self, request, context):
        del request, context

    async def CancelRun(self, request, context):
        del request, context

    async def StreamRunEvents(self, request, context):
        del request, context
        if False:
            yield None

    async def shutdown(self) -> None:
        self.calls.append("runtime.shutdown")


def factories(
    calls: list[str],
    *,
    repository: FakeRepository | None = None,
    checkpointer: FakeCheckpointer | None = None,
    server: FakeServer | None = None,
):
    repository = repository or FakeRepository(calls)
    checkpointer = checkpointer or FakeCheckpointer(calls)
    server = server or FakeServer(calls)
    tool_client = FakeToolClient(calls)
    runtime = FakeRuntime(calls)
    return (
        ApplicationFactories(
            repository=lambda dsn: repository,
            tool_client=lambda **configuration: tool_client,
            checkpointer=lambda dsn: checkpointer,
            tracer_provider=lambda: FakeTracerProvider(calls),
            server=lambda: server,
            runtime=lambda **dependencies: runtime,
        ),
        server,
    )


async def test_repository_is_closed_when_its_setup_fails():
    calls: list[str] = []
    application, _ = factories(
        calls, repository=FakeRepository(calls, fail_setup=True)
    )

    with pytest.raises(RuntimeError, match="repository setup failed"):
        await serve(settings(), stop_event=asyncio.Event(), factories=application)

    assert calls == ["repository.setup", "repository.close"]


async def test_prior_resources_close_when_checkpointer_setup_fails():
    calls: list[str] = []
    application, _ = factories(
        calls, checkpointer=FakeCheckpointer(calls, fail_setup=True)
    )

    with pytest.raises(RuntimeError, match="checkpointer setup failed"):
        await serve(settings(), stop_event=asyncio.Event(), factories=application)

    assert "checkpointer.exit" in calls
    assert "tool.close" in calls
    assert "repository.close" in calls


async def test_wait_cancellation_stops_server_runtime_and_all_dependencies():
    calls: list[str] = []
    application, server = factories(calls)
    serving = asyncio.create_task(
        serve(settings(), stop_event=asyncio.Event(), factories=application)
    )
    await asyncio.wait_for(server.started.wait(), timeout=0.5)

    serving.cancel()
    with pytest.raises(asyncio.CancelledError):
        await serving

    assert "server.stop" in calls
    assert "runtime.shutdown" in calls
    assert "tracer.shutdown" in calls
    assert "checkpointer.exit" in calls
    assert "tool.close" in calls
    assert "repository.close" in calls
    assert calls.index("repository.restart_sweep") < calls.index("server.start")


async def test_one_cleanup_failure_does_not_skip_remaining_resources():
    calls: list[str] = []
    application, _ = factories(calls, server=FakeServer(calls, fail_stop=True))
    stop_event = asyncio.Event()
    stop_event.set()

    with pytest.raises(RuntimeError, match="server stop failed"):
        await serve(settings(), stop_event=stop_event, factories=application)

    assert "server.stop" in calls
    assert "runtime.shutdown" in calls
    assert "tracer.shutdown" in calls
    assert "checkpointer.exit" in calls
    assert "tool.close" in calls
    assert "repository.close" in calls


async def test_server_start_failure_still_stops_server_and_prior_resources():
    calls: list[str] = []
    application, _ = factories(calls, server=FakeServer(calls, fail_start=True))

    with pytest.raises(RuntimeError, match="server start failed"):
        await serve(settings(), stop_event=asyncio.Event(), factories=application)

    assert "server.stop" in calls
    assert "runtime.shutdown" in calls
    assert "tracer.shutdown" in calls
    assert "checkpointer.exit" in calls
    assert "tool.close" in calls
    assert "repository.close" in calls
