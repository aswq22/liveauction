"""gRPC implementation of the phase-one read-only tool client."""

from __future__ import annotations

from typing import Any

import grpc

from app.domain.models import Observation, RunContext
from app.generated.proto.agent.v1 import common_pb2, tool_pb2, tool_pb2_grpc
from app.tools.registry import (
    AuctionHistoryQueryArguments,
    InventoryQueryArguments,
    SessionSnapshotGetArguments,
    ToolError,
)


class ToolTransportError(ToolError):
    """Raised when the internal tool service RPC fails."""


class ToolTimeoutError(ToolTransportError):
    """Raised when the registered tool deadline expires."""


class ToolResponseError(ToolError):
    """Raised when a required protobuf response field is missing."""


class GrpcToolClient:
    def __init__(
        self,
        *,
        internal_token: str,
        stub: Any | None = None,
        target: str | None = None,
    ) -> None:
        normalized_token = internal_token.strip()
        if not normalized_token:
            raise ValueError("internal token must be nonblank")
        if stub is None:
            if not target or not target.strip():
                raise ValueError("target must be nonblank when stub is not injected")
            self._channel = grpc.aio.insecure_channel(target.strip())
            stub = tool_pb2_grpc.AuctionAgentToolServiceStub(self._channel)
        else:
            self._channel = None
        self._stub = stub
        self._metadata = (("authorization", f"Bearer {normalized_token}"),)

    async def close(self) -> None:
        if self._channel is not None:
            await self._channel.close()

    async def query_inventory(
        self,
        arguments: InventoryQueryArguments,
        run_context: RunContext,
        *,
        timeout: float,
    ) -> Observation:
        request = tool_pb2.QueryInventoryRequest(
            context=_request_context(run_context),
            limit=arguments.limit,
        )
        reply = await self._invoke(
            self._stub.QueryInventory,
            request,
            tool="inventory.query",
            timeout=timeout,
        )
        return Observation(
            tool="inventory.query",
            evidence={"lots": [_inventory_lot(lot) for lot in reply.lots]},
        )

    async def query_auction_history(
        self,
        arguments: AuctionHistoryQueryArguments,
        run_context: RunContext,
        *,
        timeout: float,
    ) -> Observation:
        request = tool_pb2.QueryAuctionHistoryRequest(
            context=_request_context(run_context),
            limit=arguments.limit,
        )
        reply = await self._invoke(
            self._stub.QueryAuctionHistory,
            request,
            tool="auction.history.query",
            timeout=timeout,
        )
        return Observation(
            tool="auction.history.query",
            evidence={"items": [_history_item(item) for item in reply.items]},
        )

    async def get_session_snapshot(
        self,
        arguments: SessionSnapshotGetArguments,
        run_context: RunContext,
        *,
        timeout: float,
    ) -> Observation:
        request = tool_pb2.GetSessionSnapshotRequest(
            context=_request_context(run_context),
            room_id=arguments.room_id,
        )
        reply = await self._invoke(
            self._stub.GetSessionSnapshot,
            request,
            tool="session.snapshot.get",
            timeout=timeout,
        )
        if not reply.HasField("snapshot"):
            raise ToolResponseError("session.snapshot.get returned no snapshot")
        snapshot = reply.snapshot
        current_lot = (
            _inventory_lot(snapshot.current_lot) if snapshot.HasField("current_lot") else None
        )
        return Observation(
            tool="session.snapshot.get",
            evidence={
                "snapshot": {
                    "room_id": snapshot.room_id,
                    "current_lot": current_lot,
                    "queued_lots": [_inventory_lot(lot) for lot in snapshot.queued_lots],
                    "updated_at_unix_ms": snapshot.updated_at_unix_ms,
                }
            },
        )

    async def _invoke(self, call, request, *, tool: str, timeout: float):
        if timeout <= 0:
            raise ValueError("tool timeout must be positive")
        try:
            return await call(request, metadata=self._metadata, timeout=timeout)
        except grpc.aio.AioRpcError as error:
            if error.code() == grpc.StatusCode.DEADLINE_EXCEEDED:
                raise ToolTimeoutError(f"tool {tool!r} timed out") from None
            status = error.code().name
            raise ToolTransportError(f"tool {tool!r} RPC failed with status {status}") from None


def _request_context(run_context: RunContext) -> common_pb2.RequestContext:
    return common_pb2.RequestContext(
        request_id=run_context.request_id or "",
        trace_id=run_context.trace_id or "",
        run_id=run_context.run_id,
        tenant_id=run_context.tenant_id or "",
        operator_id=run_context.operator_id or "",
    )


def _money(money: tool_pb2.Money) -> dict[str, object]:
    return {"amount": money.amount, "currency": money.currency}


def _inventory_lot(lot: tool_pb2.InventoryLot) -> dict[str, object]:
    return {
        "id": lot.id,
        "room_id": lot.room_id,
        "title": lot.title,
        "category": lot.category,
        "description": lot.description,
        "tags": list(lot.tags),
        "status": lot.status,
        "stock": lot.stock,
        "start_price": _money(lot.start_price),
        "current_price": _money(lot.current_price),
        "created_at": lot.created_at,
        "updated_at": lot.updated_at,
        "duration_seconds": lot.duration_seconds,
    }


def _history_item(item: tool_pb2.AuctionHistoryItem) -> dict[str, object]:
    return {
        "lot_id": item.lot_id,
        "room_id": item.room_id,
        "lot_status": item.lot_status,
        "final_price": _money(item.final_price),
        "order_status": item.order_status,
        "payment_status": item.payment_status,
        "participation_result": item.participation_result,
        "settled_at_unix_ms": item.settled_at_unix_ms,
    }
