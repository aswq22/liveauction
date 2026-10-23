from __future__ import annotations

import asyncio
from dataclasses import dataclass
from types import SimpleNamespace

import grpc
import pytest
from pydantic import ValidationError

from app.domain.models import Observation, RunContext
from app.generated.proto.agent.v1 import tool_pb2
from app.tools.grpc_client import (
    GrpcToolClient,
    ToolResponseError,
    ToolTimeoutError,
    ToolTransportError,
)
from app.tools.registry import (
    AuctionHistoryQueryArguments,
    InventoryQueryArguments,
    SessionSnapshotGetArguments,
    ToolArgumentsError,
    ToolNotFoundError,
    ToolPolicyError,
    ToolRegistry,
)


pytestmark = pytest.mark.anyio


@pytest.fixture
def anyio_backend():
    return "asyncio"


def sample_run() -> RunContext:
    return RunContext(
        run_id="run-1",
        request_id="request-1",
        trace_id="trace-1",
        tenant_id="tenant-observation",
        operator_id="operator-observation",
    )


@dataclass
class RecordedCall:
    name: str
    arguments: object
    run_context: RunContext
    timeout: float


class FakeToolClient:
    def __init__(self) -> None:
        self.calls: list[RecordedCall] = []

    async def query_inventory(self, arguments, run_context, *, timeout):
        self.calls.append(RecordedCall("inventory.query", arguments, run_context, timeout))
        return Observation(tool="inventory.query", evidence={"lots": []})

    async def query_auction_history(self, arguments, run_context, *, timeout):
        self.calls.append(RecordedCall("auction.history.query", arguments, run_context, timeout))
        return Observation(tool="auction.history.query", evidence={"items": []})

    async def get_session_snapshot(self, arguments, run_context, *, timeout):
        self.calls.append(RecordedCall("session.snapshot.get", arguments, run_context, timeout))
        return Observation(
            tool="session.snapshot.get",
            evidence={"snapshot": {"room_id": arguments.room_id}},
        )


async def test_tool_registry_contains_exact_phase_one_read_only_catalog():
    registry = ToolRegistry(FakeToolClient())

    assert set(registry.specs) == {
        "inventory.query",
        "auction.history.query",
        "session.snapshot.get",
    }
    assert {spec.risk for spec in registry.specs.values()} == {"read_only"}
    assert {spec.deadline_seconds for spec in registry.specs.values()} == {10.0}
    forbidden_business_verbs = ("create", "update", "queue", "start")
    assert all(
        verb not in name.lower()
        for name in registry.specs
        for verb in forbidden_business_verbs
    )


async def test_tool_registry_rejects_tool_outside_selected_skill_before_rpc():
    client = FakeToolClient()
    registry = ToolRegistry(client)

    with pytest.raises(ToolPolicyError, match="not allowed by the selected Skill"):
        await registry.execute(
            "session.create", {}, sample_run(), allowed_tools={"inventory.query"}
        )

    assert client.calls == []


async def test_tool_registry_rejects_unregistered_tool_even_if_skill_declares_it():
    client = FakeToolClient()
    registry = ToolRegistry(client)

    with pytest.raises(ToolNotFoundError, match="not registered"):
        await registry.execute(
            "unregistered.query", {}, sample_run(), allowed_tools={"unregistered.query"}
        )

    assert client.calls == []


@pytest.mark.parametrize("arguments", ({"limit": 101}, {"limit": 0}, {"extra": True}))
async def test_tool_registry_validates_query_arguments_before_rpc(arguments):
    client = FakeToolClient()
    registry = ToolRegistry(client)

    with pytest.raises(ToolArgumentsError, match="invalid arguments"):
        await registry.execute(
            "inventory.query", arguments, sample_run(), allowed_tools={"inventory.query"}
        )

    assert client.calls == []


async def test_tool_registry_requires_room_id_before_snapshot_rpc():
    client = FakeToolClient()
    registry = ToolRegistry(client)

    with pytest.raises(ToolArgumentsError, match="invalid arguments"):
        await registry.execute(
            "session.snapshot.get", {}, sample_run(), allowed_tools={"session.snapshot.get"}
        )

    assert client.calls == []


@pytest.mark.parametrize(
    ("name", "arguments", "argument_type"),
    (
        ("inventory.query", {"limit": 100}, InventoryQueryArguments),
        ("auction.history.query", {"limit": 1}, AuctionHistoryQueryArguments),
        ("session.snapshot.get", {"room_id": "room-1"}, SessionSnapshotGetArguments),
    ),
)
async def test_tool_registry_dispatches_validated_arguments_with_ten_second_deadline(
    name, arguments, argument_type
):
    client = FakeToolClient()
    registry = ToolRegistry(client)

    observation = await registry.execute(name, arguments, sample_run(), allowed_tools={name})

    assert observation.tool == name
    assert isinstance(client.calls[0].arguments, argument_type)
    assert client.calls[0].timeout == 10.0
    assert client.calls[0].run_context == sample_run()


def test_run_context_requires_nonblank_run_id():
    with pytest.raises(ValidationError):
        RunContext(run_id="   ")


class FakeUnaryCall:
    def __init__(self, reply=None, *, error: BaseException | None = None) -> None:
        self.reply = reply
        self.error = error
        self.calls: list[tuple[object, dict]] = []

    async def __call__(self, request, **kwargs):
        self.calls.append((request, kwargs))
        if self.error is not None:
            raise self.error
        return self.reply


def inventory_reply() -> tool_pb2.QueryInventoryReply:
    return tool_pb2.QueryInventoryReply(
        lots=[
            tool_pb2.InventoryLot(
                id="lot-1",
                room_id="room-1",
                title="Camera",
                category="electronics",
                description="Merchant-owned inventory",
                tags=["featured"],
                status="ready",
                stock=3,
                start_price=tool_pb2.Money(amount=1000, currency="CNY"),
                current_price=tool_pb2.Money(amount=1200, currency="CNY"),
                created_at=100,
                updated_at=200,
                duration_seconds=300,
            )
        ]
    )


async def test_grpc_client_sends_only_bearer_metadata_and_maps_inventory_evidence():
    query_inventory = FakeUnaryCall(inventory_reply())
    stub = SimpleNamespace(QueryInventory=query_inventory)
    client = GrpcToolClient(stub=stub, internal_token="internal-secret")

    observation = await client.query_inventory(
        InventoryQueryArguments(limit=50), sample_run(), timeout=10.0
    )

    request, kwargs = query_inventory.calls[0]
    assert request.limit == 50
    assert request.context.run_id == "run-1"
    assert request.context.request_id == "request-1"
    assert request.context.trace_id == "trace-1"
    assert request.context.tenant_id == "tenant-observation"
    assert request.context.operator_id == "operator-observation"
    assert kwargs == {
        "metadata": (("authorization", "Bearer internal-secret"),),
        "timeout": 10.0,
    }
    assert observation.model_dump() == {
        "tool": "inventory.query",
        "evidence": {
            "lots": [
                {
                    "id": "lot-1",
                    "room_id": "room-1",
                    "title": "Camera",
                    "category": "electronics",
                    "description": "Merchant-owned inventory",
                    "tags": ["featured"],
                    "status": "ready",
                    "stock": 3,
                    "start_price": {"amount": 1000, "currency": "CNY"},
                    "current_price": {"amount": 1200, "currency": "CNY"},
                    "created_at": 100,
                    "updated_at": 200,
                    "duration_seconds": 300,
                }
            ]
        },
    }


async def test_grpc_client_maps_history_without_buyer_sensitive_fields():
    query_history = FakeUnaryCall(
        tool_pb2.QueryAuctionHistoryReply(
            items=[
                tool_pb2.AuctionHistoryItem(
                    lot_id="lot-1",
                    room_id="room-1",
                    lot_status="sold",
                    final_price=tool_pb2.Money(amount=2200, currency="CNY"),
                    order_status="paid",
                    payment_status="settled",
                    participation_result="won",
                    settled_at_unix_ms=1234,
                )
            ]
        )
    )
    client = GrpcToolClient(
        stub=SimpleNamespace(QueryAuctionHistory=query_history), internal_token="token"
    )

    observation = await client.query_auction_history(
        AuctionHistoryQueryArguments(limit=20), sample_run(), timeout=10.0
    )

    dumped = observation.model_dump()
    assert dumped["tool"] == "auction.history.query"
    assert dumped["evidence"]["items"] == [
        {
            "lot_id": "lot-1",
            "room_id": "room-1",
            "lot_status": "sold",
            "final_price": {"amount": 2200, "currency": "CNY"},
            "order_status": "paid",
            "payment_status": "settled",
            "participation_result": "won",
            "settled_at_unix_ms": 1234,
        }
    ]
    assert "buyer" not in str(dumped).lower()


async def test_grpc_client_maps_session_snapshot_evidence():
    snapshot_call = FakeUnaryCall(
        tool_pb2.GetSessionSnapshotReply(
            snapshot=tool_pb2.SessionSnapshot(
                room_id="room-1",
                current_lot=inventory_reply().lots[0],
                queued_lots=[],
                updated_at_unix_ms=999,
            )
        )
    )
    client = GrpcToolClient(
        stub=SimpleNamespace(GetSessionSnapshot=snapshot_call), internal_token="token"
    )

    observation = await client.get_session_snapshot(
        SessionSnapshotGetArguments(room_id="room-1"), sample_run(), timeout=10.0
    )

    assert observation.tool == "session.snapshot.get"
    assert observation.evidence["snapshot"]["room_id"] == "room-1"
    assert observation.evidence["snapshot"]["current_lot"]["id"] == "lot-1"
    assert observation.evidence["snapshot"]["queued_lots"] == []
    assert observation.evidence["snapshot"]["updated_at_unix_ms"] == 999


def test_grpc_client_requires_nonblank_internal_token():
    with pytest.raises(ValueError, match="internal token must be nonblank"):
        GrpcToolClient(stub=SimpleNamespace(), internal_token="   ")


async def test_grpc_client_maps_deadline_exceeded_without_raw_rpc_detail():
    call = FakeUnaryCall(
        error=grpc.aio.AioRpcError(
            grpc.StatusCode.DEADLINE_EXCEEDED,
            details="raw deadline detail containing sensitive payload",
        )
    )
    client = GrpcToolClient(stub=SimpleNamespace(QueryInventory=call), internal_token="token")

    with pytest.raises(ToolTimeoutError, match="inventory.query.*timed out") as captured:
        await client.query_inventory(InventoryQueryArguments(), sample_run(), timeout=10.0)

    assert "raw deadline detail" not in str(captured.value)


async def test_grpc_client_maps_non_deadline_rpc_error_to_safe_transport_error():
    call = FakeUnaryCall(
        error=grpc.aio.AioRpcError(
            grpc.StatusCode.UNAVAILABLE,
            details="raw transport detail containing sensitive payload",
        )
    )
    client = GrpcToolClient(stub=SimpleNamespace(QueryInventory=call), internal_token="token")

    with pytest.raises(ToolTransportError, match="UNAVAILABLE") as captured:
        await client.query_inventory(InventoryQueryArguments(), sample_run(), timeout=10.0)

    assert "raw transport detail" not in str(captured.value)


async def test_grpc_client_rejects_missing_snapshot_reply():
    call = FakeUnaryCall(tool_pb2.GetSessionSnapshotReply())
    client = GrpcToolClient(
        stub=SimpleNamespace(GetSessionSnapshot=call), internal_token="token"
    )

    with pytest.raises(ToolResponseError, match="returned no snapshot"):
        await client.get_session_snapshot(
            SessionSnapshotGetArguments(room_id="room-1"), sample_run(), timeout=10.0
        )


async def test_grpc_client_propagates_asyncio_cancellation():
    call = FakeUnaryCall(error=asyncio.CancelledError())
    client = GrpcToolClient(stub=SimpleNamespace(QueryInventory=call), internal_token="token")

    with pytest.raises(asyncio.CancelledError):
        await client.query_inventory(InventoryQueryArguments(), sample_run(), timeout=10.0)
