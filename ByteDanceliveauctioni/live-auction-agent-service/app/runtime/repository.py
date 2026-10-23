"""Runtime execution/event mirror repositories.

These records mirror Python execution only. They do not establish tenant or
operator ownership; the Go ``agent_runs`` record remains authoritative.
"""

from __future__ import annotations

import asyncio
from dataclasses import dataclass, replace
import json
from time import time
from typing import Any, Protocol

from app.domain.models import RunContext, TurnResult
from app.generated.proto.agent.v1 import common_pb2
from app.harness.context import RunInput, RunStatus


_TERMINAL_STATUSES = frozenset(
    {RunStatus.COMPLETED, RunStatus.FAILED, RunStatus.CANCELLED}
)


class RunConflictError(RuntimeError):
    """The same run ID was presented with different immutable input."""


class RunNotFoundError(LookupError):
    """No runtime mirror exists for the requested run ID."""


@dataclass(frozen=True, slots=True)
class RuntimeRun:
    context: RunContext
    goal: str
    max_steps: int
    status: RunStatus
    run_type: int
    session_id: str
    session_version: int
    turn_input_json: str
    turn_result_json: str
    proposal_json: str
    error_code: str
    error_message: str
    cancel_requested: bool
    created_at_unix_ms: int
    updated_at_unix_ms: int

    @property
    def run_id(self) -> str:
        return self.context.run_id


@dataclass(frozen=True, slots=True)
class RuntimeEvent:
    run_id: str
    seq: int
    type: str
    status: RunStatus
    payload_json: str
    created_at_unix_ms: int


@dataclass(frozen=True, slots=True)
class CreateRunResult:
    run: RuntimeRun
    event: RuntimeEvent | None
    created: bool


@dataclass(frozen=True, slots=True)
class RunMutationResult:
    run: RuntimeRun
    event: RuntimeEvent | None


class RuntimeRepository(Protocol):
    async def setup(self) -> None: ...
    async def close(self) -> None: ...

    async def create_run(
        self, *, context: RunContext, goal: str, max_steps: int,
        run_type: int = common_pb2.RUN_TYPE_UNSPECIFIED,
        session_id: str = "", session_version: int = 0,
        turn_input_json: str = "",
    ) -> CreateRunResult: ...

    async def get_run(self, run_id: str) -> RuntimeRun | None: ...
    async def count_nonterminal_runs(self, *, tenant_id: str | None = None) -> int: ...
    async def fail_nonterminal_runs_on_restart(self) -> list[RunMutationResult]: ...

    async def transition_run(
        self,
        run_id: str,
        *,
        status: RunStatus,
        type: str,
        proposal_json: str = "",
        turn_result_json: str = "",
        error_code: str = "",
        error_message: str = "",
        payload_json: str = "{}",
    ) -> RunMutationResult: ...

    async def request_cancel(self, run_id: str) -> RunMutationResult: ...
    async def is_cancelled(self, run_id: str) -> bool: ...

    async def append_event(
        self,
        run_id: str,
        *,
        type: str,
        status: RunStatus,
        payload_json: str = "{}",
    ) -> RuntimeEvent: ...

    async def list_events(self, run_id: str, after_seq: int) -> list[RuntimeEvent]: ...


class InMemoryRuntimeRepository:
    """Deterministic repository fake with the same atomicity contract as PostgreSQL."""

    def __init__(self) -> None:
        self._runs: dict[str, RuntimeRun] = {}
        self._events: dict[str, list[RuntimeEvent]] = {}
        self._next_sequence: dict[str, int] = {}
        self._lock = asyncio.Lock()

    async def setup(self) -> None:
        return None

    async def close(self) -> None:
        return None

    async def create_run(
        self, *, context: RunContext, goal: str, max_steps: int,
        run_type: int = common_pb2.RUN_TYPE_UNSPECIFIED,
        session_id: str = "", session_version: int = 0,
        turn_input_json: str = "",
    ) -> CreateRunResult:
        run_input = RunInput(
            context=context, goal=goal, max_steps=max_steps,
            turn_input_json=turn_input_json,
        )
        async with self._lock:
            existing = self._runs.get(context.run_id)
            if existing is not None:
                if not _same_durable_run_spec(
                    existing, run_input, run_type, session_id, session_version
                ):
                    raise RunConflictError("run input conflicts with existing runtime mirror")
                return CreateRunResult(run=existing, event=None, created=False)
            now = _now_ms()
            run = RuntimeRun(
                context=run_input.context,
                goal=run_input.goal,
                max_steps=run_input.max_steps,
                status=RunStatus.CREATED,
                run_type=run_type,
                session_id=session_id,
                session_version=session_version,
                turn_input_json=run_input.turn_input_json,
                turn_result_json="",
                proposal_json="",
                error_code="",
                error_message="",
                cancel_requested=False,
                created_at_unix_ms=now,
                updated_at_unix_ms=now,
            )
            self._runs[run.run_id] = run
            self._events[run.run_id] = []
            self._next_sequence[run.run_id] = 1
            event = self._append_locked(
                run.run_id, type="run.created", status=RunStatus.CREATED
            )
            return CreateRunResult(run=run, event=event, created=True)

    async def get_run(self, run_id: str) -> RuntimeRun | None:
        async with self._lock:
            return self._runs.get(run_id)

    async def count_nonterminal_runs(self, *, tenant_id: str | None = None) -> int:
        async with self._lock:
            return sum(
                run.status not in _TERMINAL_STATUSES
                and (tenant_id is None or run.context.tenant_id == tenant_id)
                for run in self._runs.values()
            )

    async def fail_nonterminal_runs_on_restart(self) -> list[RunMutationResult]:
        async with self._lock:
            results: list[RunMutationResult] = []
            for run_id, current in tuple(self._runs.items()):
                if current.status in _TERMINAL_STATUSES:
                    continue
                updated = replace(
                    current,
                    status=RunStatus.FAILED,
                    proposal_json="",
                    turn_result_json="",
                    error_code="AGENT_RUNTIME_RESTARTED",
                    error_message="agent runtime restarted before completion",
                    updated_at_unix_ms=_now_ms(),
                )
                self._runs[run_id] = updated
                event = self._append_locked(
                    run_id,
                    type="run.failed",
                    status=RunStatus.FAILED,
                    payload_json='{"reason":"runtime_restarted"}',
                )
                results.append(RunMutationResult(run=updated, event=event))
            return results

    async def transition_run(
        self,
        run_id: str,
        *,
        status: RunStatus,
        type: str,
        proposal_json: str = "",
        turn_result_json: str = "",
        error_code: str = "",
        error_message: str = "",
        payload_json: str = "{}",
    ) -> RunMutationResult:
        async with self._lock:
            current = self._required_run_locked(run_id)
            if current.status in _TERMINAL_STATUSES:
                return RunMutationResult(run=current, event=None)
            (
                status,
                type,
                proposal_json,
                turn_result_json,
                error_code,
                error_message,
                payload_json,
            ) = _resolve_transition(
                current,
                status=status,
                type=type,
                proposal_json=proposal_json,
                turn_result_json=turn_result_json,
                error_code=error_code,
                error_message=error_message,
                payload_json=payload_json,
            )
            updated = replace(
                current,
                status=status,
                proposal_json=proposal_json,
                turn_result_json=turn_result_json,
                error_code=error_code,
                error_message=error_message,
                updated_at_unix_ms=_now_ms(),
            )
            self._runs[run_id] = updated
            event = self._append_locked(
                run_id, type=type, status=status, payload_json=payload_json
            )
            return RunMutationResult(run=updated, event=event)

    async def request_cancel(self, run_id: str) -> RunMutationResult:
        async with self._lock:
            current = self._required_run_locked(run_id)
            if current.status in _TERMINAL_STATUSES or current.cancel_requested:
                return RunMutationResult(run=current, event=None)
            updated = replace(
                current, cancel_requested=True, updated_at_unix_ms=_now_ms()
            )
            self._runs[run_id] = updated
            event = self._append_locked(
                run_id, type="run.cancel_requested", status=current.status
            )
            return RunMutationResult(run=updated, event=event)

    async def is_cancelled(self, run_id: str) -> bool:
        async with self._lock:
            run = self._runs.get(run_id)
            return bool(run and run.cancel_requested)

    async def append_event(
        self,
        run_id: str,
        *,
        type: str,
        status: RunStatus,
        payload_json: str = "{}",
    ) -> RuntimeEvent:
        async with self._lock:
            self._required_run_locked(run_id)
            return self._append_locked(
                run_id, type=type, status=status, payload_json=payload_json
            )

    async def list_events(self, run_id: str, after_seq: int) -> list[RuntimeEvent]:
        async with self._lock:
            self._required_run_locked(run_id)
            return [event for event in self._events[run_id] if event.seq > after_seq]

    def _required_run_locked(self, run_id: str) -> RuntimeRun:
        run = self._runs.get(run_id)
        if run is None:
            raise RunNotFoundError("runtime run not found")
        return run

    def _append_locked(
        self,
        run_id: str,
        *,
        type: str,
        status: RunStatus,
        payload_json: str = "{}",
    ) -> RuntimeEvent:
        sequence = self._next_sequence[run_id]
        self._next_sequence[run_id] = sequence + 1
        event = RuntimeEvent(
            run_id=run_id,
            seq=sequence,
            type=type,
            status=status,
            payload_json=payload_json,
            created_at_unix_ms=_now_ms(),
        )
        self._events[run_id].append(event)
        return event


class PostgresRuntimeRepository:
    """PostgreSQL repository using the run row as the event sequence lock."""

    def __init__(self, dsn: str, *, pool: Any | None = None) -> None:
        normalized_dsn = dsn.strip()
        if not normalized_dsn:
            raise ValueError("database DSN must be nonblank")
        self._dsn = normalized_dsn
        self._pool = pool
        self._owns_pool = pool is None

    async def setup(self) -> None:
        if self._pool is None:
            from psycopg_pool import AsyncConnectionPool

            self._pool = AsyncConnectionPool(
                conninfo=self._dsn,
                min_size=1,
                max_size=10,
                open=False,
            )
            await self._pool.open(wait=True)
        async with self._pool.connection() as connection:
            await connection.execute(
                """
                CREATE TABLE IF NOT EXISTS agent_runtime_runs (
                    run_id TEXT PRIMARY KEY,
                    context_json TEXT NOT NULL,
                    goal TEXT NOT NULL,
                    max_steps INTEGER NOT NULL CHECK (max_steps > 0),
                    status TEXT NOT NULL,
                    run_type INTEGER NOT NULL DEFAULT 0,
                    session_id TEXT NOT NULL DEFAULT '',
                    session_version BIGINT NOT NULL DEFAULT 0,
                    turn_input_json TEXT NOT NULL DEFAULT '',
                    turn_result_json TEXT NOT NULL DEFAULT '',
                    proposal_json TEXT NOT NULL DEFAULT '',
                    error_code TEXT NOT NULL DEFAULT '',
                    error_message TEXT NOT NULL DEFAULT '',
                    cancel_requested BOOLEAN NOT NULL DEFAULT FALSE,
                    next_event_seq BIGINT NOT NULL DEFAULT 1 CHECK (next_event_seq > 0),
                    created_at_unix_ms BIGINT NOT NULL,
                    updated_at_unix_ms BIGINT NOT NULL
                )
                """
            )
            # CREATE TABLE does not change older runtime volumes. Keep every
            # upgrade additive so a restart can safely read legacy run rows.
            for statement in (
                "ALTER TABLE agent_runtime_runs ADD COLUMN IF NOT EXISTS run_type INTEGER NOT NULL DEFAULT 0",
                "ALTER TABLE agent_runtime_runs ADD COLUMN IF NOT EXISTS session_id TEXT NOT NULL DEFAULT ''",
                "ALTER TABLE agent_runtime_runs ADD COLUMN IF NOT EXISTS session_version BIGINT NOT NULL DEFAULT 0",
                "ALTER TABLE agent_runtime_runs ADD COLUMN IF NOT EXISTS turn_input_json TEXT NOT NULL DEFAULT ''",
                "ALTER TABLE agent_runtime_runs ADD COLUMN IF NOT EXISTS turn_result_json TEXT NOT NULL DEFAULT ''",
            ):
                await connection.execute(statement)
            await connection.execute(
                """
                CREATE TABLE IF NOT EXISTS agent_runtime_events (
                    run_id TEXT NOT NULL REFERENCES agent_runtime_runs(run_id) ON DELETE CASCADE,
                    seq BIGINT NOT NULL,
                    type TEXT NOT NULL,
                    status TEXT NOT NULL,
                    payload_json TEXT NOT NULL DEFAULT '{}',
                    created_at_unix_ms BIGINT NOT NULL,
                    PRIMARY KEY (run_id, seq)
                )
                """
            )
            await connection.execute(
                """
                CREATE INDEX IF NOT EXISTS agent_runtime_events_run_created_idx
                ON agent_runtime_events (run_id, created_at_unix_ms)
                """
            )

    async def close(self) -> None:
        if self._owns_pool and self._pool is not None:
            await self._pool.close()

    async def create_run(
        self, *, context: RunContext, goal: str, max_steps: int,
        run_type: int = common_pb2.RUN_TYPE_UNSPECIFIED,
        session_id: str = "", session_version: int = 0,
        turn_input_json: str = "",
    ) -> CreateRunResult:
        run_input = RunInput(
            context=context, goal=goal, max_steps=max_steps,
            turn_input_json=turn_input_json,
        )
        now = _now_ms()
        context_json = run_input.context.model_dump_json()
        async with self._pool.connection() as connection:
            async with connection.transaction():
                cursor = await connection.execute(
                    """
                    INSERT INTO agent_runtime_runs (
                        run_id, context_json, goal, max_steps, status, run_type,
                        session_id, session_version, turn_input_json,
                        created_at_unix_ms, updated_at_unix_ms
                    ) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
                    ON CONFLICT (run_id) DO NOTHING
                    RETURNING run_id
                    """,
                    (
                        context.run_id,
                        context_json,
                        run_input.goal,
                        run_input.max_steps,
                        RunStatus.CREATED.value,
                        run_type,
                        session_id,
                        session_version,
                        run_input.turn_input_json,
                        now,
                        now,
                    ),
                )
                inserted = await cursor.fetchone()
                if inserted is None:
                    existing = await self._get_run_connection(
                        connection, context.run_id, for_update=True
                    )
                    if existing is None:
                        raise RunNotFoundError("runtime run not found")
                    if not _same_durable_run_spec(
                        existing, run_input, run_type, session_id, session_version
                    ):
                        raise RunConflictError(
                            "run input conflicts with existing runtime mirror"
                        )
                    return CreateRunResult(run=existing, event=None, created=False)
                run = await self._get_run_connection(connection, context.run_id)
                assert run is not None
                event = await self._append_event_connection(
                    connection,
                    context.run_id,
                    type="run.created",
                    status=RunStatus.CREATED,
                )
                return CreateRunResult(run=run, event=event, created=True)

    async def get_run(self, run_id: str) -> RuntimeRun | None:
        async with self._pool.connection() as connection:
            return await self._get_run_connection(connection, run_id)

    async def count_nonterminal_runs(self, *, tenant_id: str | None = None) -> int:
        params: tuple[Any, ...] = tuple(status.value for status in _TERMINAL_STATUSES)
        tenant_clause = ""
        if tenant_id is not None:
            tenant_clause = " AND context_json::jsonb ->> 'tenant_id' = %s"
            params = (*params, tenant_id)
        async with self._pool.connection() as connection:
            cursor = await connection.execute(
                "SELECT COUNT(*) FROM agent_runtime_runs "
                "WHERE status NOT IN (%s, %s, %s)" + tenant_clause,
                params,
            )
            row = await cursor.fetchone()
            return int(row[0])

    async def fail_nonterminal_runs_on_restart(self) -> list[RunMutationResult]:
        results: list[RunMutationResult] = []
        async with self._pool.connection() as connection:
            async with connection.transaction():
                cursor = await connection.execute(
                    """
                    SELECT run_id FROM agent_runtime_runs
                    WHERE status NOT IN (%s, %s, %s)
                    ORDER BY run_id
                    FOR UPDATE SKIP LOCKED
                    """,
                    tuple(status.value for status in _TERMINAL_STATUSES),
                )
                run_ids = [row[0] for row in await cursor.fetchall()]
                for run_id in run_ids:
                    current = await self._get_run_connection(
                        connection, run_id, for_update=True
                    )
                    if current is None or current.status in _TERMINAL_STATUSES:
                        continue
                    now = _now_ms()
                    await connection.execute(
                        """
                        UPDATE agent_runtime_runs
                        SET status = %s, proposal_json = '', turn_result_json = '', error_code = %s,
                            error_message = %s, updated_at_unix_ms = %s
                        WHERE run_id = %s AND status NOT IN (%s, %s, %s)
                        """,
                        (
                            RunStatus.FAILED.value,
                            "AGENT_RUNTIME_RESTARTED",
                            "agent runtime restarted before completion",
                            now,
                            run_id,
                            *(status.value for status in _TERMINAL_STATUSES),
                        ),
                    )
                    event = await self._append_event_connection(
                        connection,
                        run_id,
                        type="run.failed",
                        status=RunStatus.FAILED,
                        payload_json='{"reason":"runtime_restarted"}',
                    )
                    updated = replace(
                        current,
                        status=RunStatus.FAILED,
                        proposal_json="",
                        turn_result_json="",
                        error_code="AGENT_RUNTIME_RESTARTED",
                        error_message="agent runtime restarted before completion",
                        updated_at_unix_ms=now,
                    )
                    results.append(RunMutationResult(run=updated, event=event))
        return results

    async def transition_run(
        self,
        run_id: str,
        *,
        status: RunStatus,
        type: str,
        proposal_json: str = "",
        turn_result_json: str = "",
        error_code: str = "",
        error_message: str = "",
        payload_json: str = "{}",
    ) -> RunMutationResult:
        async with self._pool.connection() as connection:
            async with connection.transaction():
                current = await self._get_run_connection(
                    connection, run_id, for_update=True
                )
                if current is None:
                    raise RunNotFoundError("runtime run not found")
                if current.status in _TERMINAL_STATUSES:
                    return RunMutationResult(run=current, event=None)
                (
                    status,
                    type,
                    proposal_json,
                    turn_result_json,
                    error_code,
                    error_message,
                    payload_json,
                ) = _resolve_transition(
                    current,
                    status=status,
                    type=type,
                    proposal_json=proposal_json,
                    turn_result_json=turn_result_json,
                    error_code=error_code,
                    error_message=error_message,
                    payload_json=payload_json,
                )
                now = _now_ms()
                await connection.execute(
                    """
                    UPDATE agent_runtime_runs
                    SET status = %s, proposal_json = %s, turn_result_json = %s, error_code = %s,
                        error_message = %s, updated_at_unix_ms = %s
                    WHERE run_id = %s
                    """,
                    (
                        status.value,
                        proposal_json,
                        turn_result_json,
                        error_code,
                        error_message,
                        now,
                        run_id,
                    ),
                )
                event = await self._append_event_connection(
                    connection,
                    run_id,
                    type=type,
                    status=status,
                    payload_json=payload_json,
                )
                updated = replace(
                    current,
                    status=status,
                    proposal_json=proposal_json,
                    turn_result_json=turn_result_json,
                    error_code=error_code,
                    error_message=error_message,
                    updated_at_unix_ms=now,
                )
                return RunMutationResult(run=updated, event=event)

    async def request_cancel(self, run_id: str) -> RunMutationResult:
        async with self._pool.connection() as connection:
            async with connection.transaction():
                current = await self._get_run_connection(
                    connection, run_id, for_update=True
                )
                if current is None:
                    raise RunNotFoundError("runtime run not found")
                if current.status in _TERMINAL_STATUSES or current.cancel_requested:
                    return RunMutationResult(run=current, event=None)
                now = _now_ms()
                await connection.execute(
                    """
                    UPDATE agent_runtime_runs
                    SET cancel_requested = TRUE, updated_at_unix_ms = %s
                    WHERE run_id = %s
                    """,
                    (now, run_id),
                )
                event = await self._append_event_connection(
                    connection,
                    run_id,
                    type="run.cancel_requested",
                    status=current.status,
                )
                return RunMutationResult(
                    run=replace(
                        current, cancel_requested=True, updated_at_unix_ms=now
                    ),
                    event=event,
                )

    async def is_cancelled(self, run_id: str) -> bool:
        async with self._pool.connection() as connection:
            cursor = await connection.execute(
                "SELECT cancel_requested FROM agent_runtime_runs WHERE run_id = %s",
                (run_id,),
            )
            row = await cursor.fetchone()
            return bool(row and row[0])

    async def append_event(
        self,
        run_id: str,
        *,
        type: str,
        status: RunStatus,
        payload_json: str = "{}",
    ) -> RuntimeEvent:
        async with self._pool.connection() as connection:
            async with connection.transaction():
                return await self._append_event_connection(
                    connection,
                    run_id,
                    type=type,
                    status=status,
                    payload_json=payload_json,
                )

    async def list_events(self, run_id: str, after_seq: int) -> list[RuntimeEvent]:
        async with self._pool.connection() as connection:
            exists = await connection.execute(
                "SELECT 1 FROM agent_runtime_runs WHERE run_id = %s", (run_id,)
            )
            if await exists.fetchone() is None:
                raise RunNotFoundError("runtime run not found")
            cursor = await connection.execute(
                """
                SELECT run_id, seq, type, status, payload_json, created_at_unix_ms
                FROM agent_runtime_events
                WHERE run_id = %s AND seq > %s
                ORDER BY seq ASC
                """,
                (run_id, after_seq),
            )
            return [_row_to_event(row) for row in await cursor.fetchall()]

    async def _get_run_connection(
        self, connection, run_id: str, *, for_update: bool = False
    ) -> RuntimeRun | None:
        locking = " FOR UPDATE" if for_update else ""
        cursor = await connection.execute(
            """
            SELECT context_json, goal, max_steps, status, run_type, session_id,
                   session_version, turn_input_json, turn_result_json, proposal_json,
                   error_code, error_message, cancel_requested,
                   created_at_unix_ms, updated_at_unix_ms
            FROM agent_runtime_runs WHERE run_id = %s
            """
            + locking,
            (run_id,),
        )
        row = await cursor.fetchone()
        if row is None:
            return None
        return RuntimeRun(
            context=RunContext.model_validate(json.loads(row[0])),
            goal=row[1],
            max_steps=row[2],
            status=RunStatus(row[3]),
            run_type=row[4],
            session_id=row[5],
            session_version=row[6],
            turn_input_json=row[7],
            turn_result_json=row[8],
            proposal_json=row[9],
            error_code=row[10],
            error_message=row[11],
            cancel_requested=row[12],
            created_at_unix_ms=row[13],
            updated_at_unix_ms=row[14],
        )

    async def _append_event_connection(
        self,
        connection,
        run_id: str,
        *,
        type: str,
        status: RunStatus,
        payload_json: str = "{}",
    ) -> RuntimeEvent:
        # This UPDATE takes the per-run row lock and allocates a unique sequence
        # in the same transaction as the event INSERT. It never relies on MAX+1.
        cursor = await connection.execute(
            """
            UPDATE agent_runtime_runs
            SET next_event_seq = next_event_seq + 1
            WHERE run_id = %s
            RETURNING next_event_seq - 1
            """,
            (run_id,),
        )
        row = await cursor.fetchone()
        if row is None:
            raise RunNotFoundError("runtime run not found")
        event = RuntimeEvent(
            run_id=run_id,
            seq=row[0],
            type=type,
            status=status,
            payload_json=payload_json,
            created_at_unix_ms=_now_ms(),
        )
        await connection.execute(
            """
            INSERT INTO agent_runtime_events (
                run_id, seq, type, status, payload_json, created_at_unix_ms
            ) VALUES (%s, %s, %s, %s, %s, %s)
            """,
            (
                event.run_id,
                event.seq,
                event.type,
                event.status.value,
                event.payload_json,
                event.created_at_unix_ms,
            ),
        )
        return event


def _row_to_event(row) -> RuntimeEvent:
    return RuntimeEvent(
        run_id=row[0],
        seq=row[1],
        type=row[2],
        status=RunStatus(row[3]),
        payload_json=row[4],
        created_at_unix_ms=row[5],
    )


def _same_durable_run_spec(
    existing: RuntimeRun,
    candidate: RunInput,
    run_type: int,
    session_id: str,
    session_version: int,
) -> bool:
    """Compare immutable execution input while ignoring per-call correlation."""
    return (
        existing.context.run_id == candidate.context.run_id
        and existing.context.trace_id == candidate.context.trace_id
        and existing.context.tenant_id == candidate.context.tenant_id
        and existing.context.operator_id == candidate.context.operator_id
        and existing.goal == candidate.goal
        and existing.max_steps == candidate.max_steps
        and existing.run_type == run_type
        and existing.session_id == session_id
        and existing.session_version == session_version
        and existing.turn_input_json == candidate.turn_input_json
    )


def _resolve_transition(
    current: RuntimeRun,
    *,
    status: RunStatus,
    type: str,
    proposal_json: str,
    turn_result_json: str,
    error_code: str,
    error_message: str,
    payload_json: str,
) -> tuple[RunStatus, str, str, str, str, str, str]:
    """Resolve a terminal transition while holding the repository run lock."""
    if status in _TERMINAL_STATUSES and (
        current.cancel_requested or status is RunStatus.CANCELLED
    ):
        return (
            RunStatus.CANCELLED,
            "run.cancelled",
            "",
            "",
            "RUN_CANCELLED",
            "run cancelled",
            "{}",
        )
    if status is RunStatus.COMPLETED and _requires_turn_result(current):
        try:
            TurnResult.model_validate_json(turn_result_json)
        except (TypeError, ValueError):
            return (
                RunStatus.FAILED,
                "run.failed",
                "",
                "",
                "INVALID_TURN_RESULT",
                "agent runtime returned an invalid turn result",
                "{}",
            )
    # A terminal result is an all-or-nothing success artifact. Never expose a
    # partial turn result from a failed (or still-running) execution.
    if status is not RunStatus.COMPLETED:
        turn_result_json = ""
    return (
        status, type, proposal_json, turn_result_json, error_code, error_message,
        payload_json,
    )


def _requires_turn_result(run: RuntimeRun) -> bool:
    return (
        run.run_type != common_pb2.RUN_TYPE_UNSPECIFIED
        or bool(run.session_id)
        or run.session_version != 0
        or bool(run.turn_input_json)
    )


def _now_ms() -> int:
    return int(time() * 1000)
