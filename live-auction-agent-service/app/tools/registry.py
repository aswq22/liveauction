"""Closed registry for phase-one read-only auction tools."""

from __future__ import annotations

from collections.abc import Collection, Mapping
from dataclasses import dataclass
from types import MappingProxyType
from typing import Annotated, Any, Literal, Protocol

from pydantic import BaseModel, ConfigDict, Field, StringConstraints, ValidationError

from app.domain.models import Observation, RunContext


NonBlankText = Annotated[str, StringConstraints(strip_whitespace=True, min_length=1)]


class ToolError(RuntimeError):
    """Base class for typed tool-boundary failures."""


class ToolPolicyError(ToolError):
    """Raised when the selected Skill does not allow a requested tool."""


class ToolNotFoundError(ToolError):
    """Raised when a tool is outside the closed registry."""


class ToolArgumentsError(ToolError):
    """Raised before RPC when arguments do not satisfy a registered schema."""


class ToolExecutionError(ToolError):
    """Raised when a tool client violates the observation contract."""


class ToolArguments(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=True)


class InventoryQueryArguments(ToolArguments):
    limit: int = Field(default=50, ge=1, le=100)


class AuctionHistoryQueryArguments(ToolArguments):
    limit: int = Field(default=50, ge=1, le=100)


class SessionSnapshotGetArguments(ToolArguments):
    room_id: NonBlankText


class ToolClient(Protocol):
    async def query_inventory(
        self, arguments: InventoryQueryArguments, run_context: RunContext, *, timeout: float
    ) -> Observation: ...

    async def query_auction_history(
        self, arguments: AuctionHistoryQueryArguments, run_context: RunContext, *, timeout: float
    ) -> Observation: ...

    async def get_session_snapshot(
        self, arguments: SessionSnapshotGetArguments, run_context: RunContext, *, timeout: float
    ) -> Observation: ...


@dataclass(frozen=True, slots=True)
class ToolSpec:
    name: str
    arguments_model: type[ToolArguments]
    client_method: str
    risk: Literal["read_only"] = "read_only"
    deadline_seconds: float = 10.0


_SPECS = MappingProxyType(
    {
        "inventory.query": ToolSpec(
            name="inventory.query",
            arguments_model=InventoryQueryArguments,
            client_method="query_inventory",
        ),
        "auction.history.query": ToolSpec(
            name="auction.history.query",
            arguments_model=AuctionHistoryQueryArguments,
            client_method="query_auction_history",
        ),
        "session.snapshot.get": ToolSpec(
            name="session.snapshot.get",
            arguments_model=SessionSnapshotGetArguments,
            client_method="get_session_snapshot",
        ),
    }
)


class ToolRegistry:
    def __init__(self, client: ToolClient) -> None:
        self._client = client

    @property
    def specs(self) -> Mapping[str, ToolSpec]:
        return _SPECS

    async def execute(
        self,
        name: str,
        arguments: Mapping[str, Any],
        run_context: RunContext,
        *,
        allowed_tools: Collection[str],
    ) -> Observation:
        if name not in allowed_tools:
            raise ToolPolicyError(f"tool {name!r} is not allowed by the selected Skill")
        spec = _SPECS.get(name)
        if spec is None:
            raise ToolNotFoundError(f"tool {name!r} is not registered")
        try:
            validated_arguments = spec.arguments_model.model_validate(arguments)
        except (TypeError, ValidationError):
            raise ToolArgumentsError(f"invalid arguments for tool {name!r}") from None

        method = getattr(self._client, spec.client_method)
        observation = await method(
            validated_arguments,
            run_context,
            timeout=spec.deadline_seconds,
        )
        if not isinstance(observation, Observation) or observation.tool != name:
            raise ToolExecutionError(f"tool {name!r} returned an invalid observation")
        return observation
