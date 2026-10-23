"""Host-orchestrated failure isolation coverage for the planning agent."""

from __future__ import annotations

from contextlib import contextmanager
from dataclasses import dataclass, field
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import time
from typing import Iterator
from urllib.request import urlopen
import warnings

import pytest

from tests.e2e.test_readonly_planning import (
    BASE_URL,
    DEPLOY_DIR,
    _access_token_and_tenant,
    _admin_inventory,
    _assert_ok,
    _business_table_snapshot,
    _compose,
    _compose_file,
    _mysql_output,
    _request_json,
    _sql_literal,
    _wait_for_terminal_run,
)


SHARED_RUNTIME_COORDINATION_TABLES = frozenset(
    {"auction_runtime_projection_shard_offsets"}
)
RUNTIME_PROJECTION_SHARDS = 16


class _FileLock:
    def __init__(self, path: Path, *, timeout_seconds: float) -> None:
        self._path = path
        self._timeout_seconds = timeout_seconds
        self._handle: object | None = None

    def acquire(self) -> None:
        self._path.parent.mkdir(parents=True, exist_ok=True)
        handle = self._path.open("a+b")
        handle.seek(0, os.SEEK_END)
        if handle.tell() == 0:
            handle.write(b"\0")
            handle.flush()
        deadline = time.monotonic() + self._timeout_seconds
        while True:
            try:
                handle.seek(0)
                if os.name == "nt":
                    import msvcrt

                    msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl

                    fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                self._handle = handle
                return
            except (OSError, BlockingIOError):
                if time.monotonic() >= deadline:
                    handle.close()
                    raise TimeoutError(
                        f"timed out acquiring failure-isolation lock {self._path}"
                    )
                time.sleep(0.05)

    def release(self) -> None:
        handle = self._handle
        self._handle = None
        if handle is None:
            return
        try:
            handle.seek(0)
            if os.name == "nt":
                import msvcrt

                msvcrt.locking(handle.fileno(), msvcrt.LK_UNLCK, 1)
            else:
                import fcntl

                fcntl.flock(handle.fileno(), fcntl.LOCK_UN)
        finally:
            handle.close()

    def __enter__(self) -> _FileLock:
        self.acquire()
        return self

    def __exit__(self, *_: object) -> None:
        self.release()


@dataclass(frozen=True)
class _AgentState:
    container_id: str
    running: bool
    healthy: bool
    project: str = ""
    config_file: Path = Path()


@dataclass
class _FailureFixture:
    merchant_token: str = ""
    merchant_user_id: str = ""
    merchant_username: str = ""
    bidder_token: str = ""
    bidder_user_id: str = ""
    bidder_username: str = ""
    lot_id: str = ""
    room_id: str = ""
    address_id: str = ""
    run_id: str = ""
    merchant_nickname: str = ""
    bidder_nickname: str = ""
    lot_title: str = ""
    address_detail: str = ""
    address_tag: str = ""
    deposit_idempotency_key: str = ""
    bid_idempotency_key: str = ""
    bid_id: str = ""
    runtime_event_id: str = ""
    runtime_stream_id: str = ""
    runtime_stream_key: str = ""
    run_goal: str = ""
    run_idempotency_key: str = ""
    cleanup_counts: dict[str, int] = field(default_factory=dict)


def _new_failure_fixture() -> _FailureFixture:
    nonce = str(time.time_ns())
    return _FailureFixture(
        merchant_username=f"agent_iso_merchant_{nonce}",
        merchant_nickname=f"Agent isolation merchant {nonce}",
        bidder_username=f"agent_iso_bidder_{nonce}",
        bidder_nickname=f"Agent isolation bidder {nonce}",
        lot_title=f"Agent outage isolated bid lot {nonce}",
        address_detail=f"Agent failure isolation fixture {nonce}",
        address_tag=f"agent-{nonce[-12:]}",
        deposit_idempotency_key=f"agent-isolation-deposit-{nonce}",
        bid_idempotency_key=f"agent-isolation-bid-{nonce}",
        run_goal=f"verify agent outage does not affect successful bidding {nonce}",
        run_idempotency_key=f"agent-run-{nonce}",
    )


def _canonical_path(path: str | Path) -> Path:
    try:
        return Path(os.path.realpath(path)).resolve(strict=True)
    except OSError as error:
        raise RuntimeError(f"Compose path cannot be resolved exactly: {path!r}") from error


def _same_canonical_path(actual: str, expected: Path) -> bool:
    return os.path.normcase(str(_canonical_path(actual))) == os.path.normcase(
        str(_canonical_path(expected))
    )


def _validated_agent_state(
    inspect_data: dict[str, object], *, expected_config_file: Path,
    expected_config_hash: str, expected_project: str,
) -> _AgentState:
    config = inspect_data.get("Config")
    state = inspect_data.get("State")
    if not isinstance(config, dict) or not isinstance(state, dict):
        raise RuntimeError("auction-agent inspect result has no Config/State")
    labels = config.get("Labels")
    if not isinstance(labels, dict):
        raise RuntimeError("auction-agent inspect result has no Compose labels")

    expected_labels = {
        "com.docker.compose.service": "auction-agent",
        "com.docker.compose.project": expected_project,
    }
    for name, expected in expected_labels.items():
        actual = str(labels.get(name, ""))
        if actual != expected:
            raise RuntimeError(
                f"auction-agent Compose label {name!r} was {actual!r}, expected {expected!r}"
            )
    actual_config = str(labels.get("com.docker.compose.project.config_files", ""))
    try:
        config_matches = _same_canonical_path(actual_config, expected_config_file)
    except RuntimeError as error:
        raise RuntimeError(
            "auction-agent Compose label 'com.docker.compose.project.config_files' "
            f"cannot be resolved exactly: {actual_config!r}"
        ) from error
    if not config_matches:
        raise RuntimeError(
            "auction-agent Compose label 'com.docker.compose.project.config_files' "
            f"was {actual_config!r}, expected {str(expected_config_file)!r}"
        )
    actual_working_dir = str(labels.get("com.docker.compose.project.working_dir", ""))
    try:
        working_dir_matches = _same_canonical_path(
            actual_working_dir, expected_config_file.parent
        )
    except RuntimeError as error:
        raise RuntimeError(
            "auction-agent Compose label 'com.docker.compose.project.working_dir' "
            f"cannot be resolved exactly: {actual_working_dir!r}"
        ) from error
    if not working_dir_matches:
        raise RuntimeError(
            "auction-agent Compose label 'com.docker.compose.project.working_dir' "
            f"was {actual_working_dir!r}, expected {str(expected_config_file.parent)!r}"
        )
    actual_hash = str(labels.get("com.docker.compose.config-hash", ""))
    if actual_hash != expected_config_hash:
        raise RuntimeError(
            "auction-agent Compose label 'com.docker.compose.config-hash' was "
            f"{actual_hash!r}, expected {expected_config_hash!r}"
        )

    container_id = str(inspect_data.get("Id", "")).strip()
    if not container_id:
        raise RuntimeError("auction-agent inspect result has no container ID")
    health = state.get("Health")
    health_status = health.get("Status") if isinstance(health, dict) else ""
    return _AgentState(
        container_id=container_id,
        running=state.get("Running") is True,
        healthy=health_status == "healthy",
        project=expected_project,
        config_file=_canonical_path(expected_config_file),
    )


def _inspect_agent_state() -> _AgentState:
    expected_config_file = _compose_file()
    completed = _compose("ps", "--all", "-q", "auction-agent")
    container_ids = [line.strip() for line in completed.stdout.splitlines() if line.strip()]
    if len(container_ids) != 1:
        raise RuntimeError(
            "expected exactly one auction-agent container from this Compose project, "
            f"found {container_ids!r}"
        )
    inspected = subprocess.run(
        ["docker", "inspect", container_ids[0]],
        check=True,
        capture_output=True,
        text=True,
    )
    payload = json.loads(inspected.stdout)
    if not isinstance(payload, list) or len(payload) != 1 or not isinstance(payload[0], dict):
        raise RuntimeError("docker inspect returned an unexpected auction-agent payload")
    config_hash = _compose("config", "--hash", "auction-agent").stdout.split()
    if len(config_hash) != 2 or config_hash[0] != "auction-agent":
        raise RuntimeError("docker compose did not return the auction-agent config hash")
    configuration = _compose("config", "--format", "json")
    project = str(json.loads(configuration.stdout).get("name", "")).strip()
    if not project:
        raise RuntimeError("docker compose config returned no project name")
    state = _validated_agent_state(
        payload[0],
        expected_config_file=expected_config_file,
        expected_config_hash=config_hash[1],
        expected_project=project,
    )
    if not state.container_id.startswith(container_ids[0]):
        raise RuntimeError("docker inspect returned a different container than Compose selected")
    return state


def _agent_lock_path(
    *, container_id: str, canonical_config_file: Path, project: str
) -> Path:
    identity = "\0".join(
        (
            container_id,
            os.path.normcase(str(_canonical_path(canonical_config_file))),
            project,
        )
    )
    digest = hashlib.sha256(identity.encode("utf-8")).hexdigest()
    return Path(tempfile.gettempdir()) / "auction-agent-e2e-locks" / f"{digest}.lock"


@contextmanager
def _stopped_agent(
    *,
    lock_path: Path | None = None,
    lock_timeout_seconds: float = 30,
) -> Iterator[_AgentState]:
    candidate = _inspect_agent_state()
    if lock_path is None:
        lock_path = _agent_lock_path(
            container_id=candidate.container_id,
            canonical_config_file=candidate.config_file,
            project=candidate.project,
        )
    with _FileLock(lock_path, timeout_seconds=lock_timeout_seconds):
        initial = _inspect_agent_state()
        if initial.container_id != candidate.container_id:
            raise RuntimeError("auction-agent container changed before lifecycle lock acquired")
        try:
            if initial.running:
                _compose("stop", "auction-agent")
            yield initial
        finally:
            if initial.running:
                _compose("start", "auction-agent")
                if initial.healthy:
                    _wait_for_agent_health("healthy")
            elif _inspect_agent_state().running:
                _compose("stop", "auction-agent")


def _cleanup_delete(
    statements: list[str], table: str, predicate: str
) -> None:
    statements.append(f"DELETE FROM `{table}` WHERE {predicate}")
    statements.append(f"SELECT {_sql_literal(table)}, ROW_COUNT()")


def _redis_fixture_keys(fixture: _FailureFixture) -> tuple[str, ...]:
    if not fixture.lot_id:
        return ()
    tag = f"auction:lot:{{{fixture.lot_id}}}"
    keys = [
        f"{tag}:state",
        f"{tag}:ranking",
        f"{tag}:rankmeta",
        f"{tag}:participants",
        f"{tag}:recent",
    ]
    if fixture.bidder_user_id and fixture.bid_idempotency_key:
        keys.extend(
            (
                f"{tag}:idem:{fixture.bidder_user_id}:{fixture.bid_idempotency_key}",
                "auction:idem:"
                f"{fixture.lot_id}:{fixture.bidder_user_id}:"
                f"{fixture.bid_idempotency_key}",
            )
        )
    return tuple(keys)


def _stream_entry_matches_fixture(
    fixture: _FailureFixture, values: dict[str, object]
) -> bool:
    if not all(
        (
            fixture.merchant_user_id,
            fixture.bidder_user_id,
            fixture.lot_id,
            fixture.bid_idempotency_key,
            fixture.runtime_event_id,
        )
    ):
        return False
    if (
        str(values.get("event_id", "")) != fixture.runtime_event_id
        or str(values.get("lot_id", "")) != fixture.lot_id
    ):
        return False
    try:
        payload = json.loads(str(values.get("payload", "")))
    except (TypeError, ValueError):
        return False
    if not isinstance(payload, dict):
        return False
    bid = payload.get("bid")
    lot = payload.get("updated_lot")
    return (
        payload.get("event_id") == fixture.runtime_event_id
        and payload.get("lot_id") == fixture.lot_id
        and payload.get("idempotency_key") == fixture.bid_idempotency_key
        and isinstance(bid, dict)
        and bid.get("user_id") == fixture.bidder_user_id
        and isinstance(lot, dict)
        and lot.get("main_account_id") == fixture.merchant_user_id
    )


def _redis_json(*arguments: str) -> object:
    password = os.getenv("AUCTION_E2E_REDIS_PASSWORD", "auction_redis")
    completed = _compose(
        "exec",
        "-T",
        "redis",
        "redis-cli",
        "-a",
        password,
        "--no-auth-warning",
        "--json",
        *arguments,
    )
    output = completed.stdout.strip()
    return None if not output else json.loads(output)


def _redis_scan(pattern: str) -> set[str]:
    cursor = "0"
    keys: set[str] = set()
    while True:
        reply = _redis_json("SCAN", cursor, "MATCH", pattern, "COUNT", "500")
        if not isinstance(reply, list) or len(reply) != 2:
            raise RuntimeError(f"unexpected Redis SCAN reply: {reply!r}")
        cursor = str(reply[0])
        batch = reply[1]
        if not isinstance(batch, list):
            raise RuntimeError(f"unexpected Redis SCAN key batch: {reply!r}")
        keys.update(str(key) for key in batch)
        if cursor == "0":
            return keys


def _redis_stream_entries(stream: str) -> list[tuple[str, dict[str, object]]]:
    reply = _redis_json("XRANGE", stream, "-", "+")
    if reply is None:
        return []
    if not isinstance(reply, list):
        raise RuntimeError(f"unexpected Redis XRANGE reply for {stream!r}: {reply!r}")
    entries: list[tuple[str, dict[str, object]]] = []
    for item in reply:
        if not isinstance(item, list) or len(item) != 2 or not isinstance(item[1], list):
            raise RuntimeError(f"unexpected Redis stream entry in {stream!r}: {item!r}")
        fields = item[1]
        if len(fields) % 2:
            raise RuntimeError(f"odd Redis stream field list in {stream!r}: {item!r}")
        values = {
            str(fields[index]): fields[index + 1]
            for index in range(0, len(fields), 2)
        }
        entries.append((str(item[0]), values))
    return entries


def _fixture_stream_entries(
    fixture: _FailureFixture,
) -> list[tuple[str, str]]:
    matches: list[tuple[str, str]] = []
    for shard in range(RUNTIME_PROJECTION_SHARDS):
        stream = f"auction:runtime_events:shard:{shard}"
        for entry_id, values in _redis_stream_entries(stream):
            if _stream_entry_matches_fixture(fixture, values):
                matches.append((stream, entry_id))
    return matches


def _recover_redis_bid_metadata(fixture: _FailureFixture) -> None:
    keys = _redis_fixture_keys(fixture)
    if len(keys) < 6:
        return
    raw = _redis_json("GET", keys[5])
    if raw is None:
        return
    if not isinstance(raw, str):
        raise RuntimeError(f"unexpected runtime idempotency payload: {raw!r}")
    try:
        reply = json.loads(raw)
    except ValueError as error:
        raise RuntimeError("runtime idempotency payload is not JSON") from error
    if not isinstance(reply, dict):
        raise RuntimeError(f"unexpected runtime idempotency object: {reply!r}")
    lot = reply.get("lot")
    bid = reply.get("bid")
    if not isinstance(lot, dict) or not isinstance(bid, dict):
        raise RuntimeError("runtime idempotency payload has no lot/bid identity")
    expected = {
        "lot_id": (lot.get("lot_id"), fixture.lot_id),
        "main_account_id": (lot.get("main_account_id"), fixture.merchant_user_id),
        "user_id": (bid.get("user_id"), fixture.bidder_user_id),
    }
    mismatches = {
        name: (actual, wanted)
        for name, (actual, wanted) in expected.items()
        if str(actual or "") != wanted
    }
    if mismatches:
        raise RuntimeError(f"runtime idempotency identity mismatch: {mismatches}")
    fixture.bid_id = fixture.bid_id or str(bid.get("id", ""))
    fixture.runtime_event_id = fixture.runtime_event_id or str(
        reply.get("runtime_event_id", "")
    )
    fixture.runtime_stream_id = fixture.runtime_stream_id or str(
        reply.get("runtime_stream_id", "")
    )
    if not all((fixture.bid_id, fixture.runtime_event_id, fixture.runtime_stream_id)):
        raise RuntimeError("runtime idempotency payload has incomplete bid/event identity")
    matches = _fixture_stream_entries(fixture)
    if len(matches) != 1:
        raise RuntimeError(
            f"expected one exact Redis stream entry for fixture, found {matches!r}"
        )
    fixture.runtime_stream_key, matched_id = matches[0]
    if matched_id != fixture.runtime_stream_id:
        raise RuntimeError(
            "runtime idempotency stream ID does not match the exact stream payload"
        )


def _stream_id_at_least(actual: str, expected: str) -> bool:
    try:
        actual_parts = tuple(int(part) for part in actual.split("-", 1))
        expected_parts = tuple(int(part) for part in expected.split("-", 1))
    except ValueError:
        return False
    return len(actual_parts) == len(expected_parts) == 2 and actual_parts >= expected_parts


def _wait_for_fixture_projection(
    fixture: _FailureFixture, timeout_seconds: float = 10
) -> None:
    if not all(
        (
            fixture.bid_id,
            fixture.lot_id,
            fixture.bidder_user_id,
            fixture.bid_idempotency_key,
            fixture.runtime_stream_key,
            fixture.runtime_stream_id,
        )
    ):
        return
    shard_text = fixture.runtime_stream_key.rsplit(":", 1)[-1]
    try:
        shard = int(shard_text)
    except ValueError as error:
        raise RuntimeError(
            f"unexpected runtime stream key: {fixture.runtime_stream_key!r}"
        ) from error
    deadline = time.monotonic() + timeout_seconds
    last = ""
    while time.monotonic() < deadline:
        output = str(
            _mysql_output(
                "SELECT (SELECT COUNT(*) FROM auction_bids WHERE id="
                f"{_sql_literal(fixture.bid_id)} AND lot_id="
                f"{_sql_literal(fixture.lot_id)} AND user_id="
                f"{_sql_literal(fixture.bidder_user_id)} AND idempotency_key="
                f"{_sql_literal(fixture.bid_idempotency_key)}), "
                "COALESCE((SELECT last_stream_id FROM "
                "auction_runtime_projection_shard_offsets WHERE shard_id="
                f"{shard}), '0-0')"
            )
        ).strip()
        last = output.splitlines()[-1] if output else ""
        parts = last.split("\t")
        if (
            len(parts) == 2
            and parts[0] == "1"
            and _stream_id_at_least(parts[1], fixture.runtime_stream_id)
        ):
            return
        time.sleep(0.1)
    raise RuntimeError(
        "fixture bid projection did not reach its exact stream entry before cleanup: "
        f"last={last!r}"
    )


def _cleanup_redis_fixture(fixture: _FailureFixture) -> dict[str, int]:
    counts: dict[str, int] = {}
    if not fixture.bid_idempotency_key:
        return counts
    if fixture.runtime_stream_key and fixture.runtime_stream_id:
        deleted = _redis_json(
            "XDEL", fixture.runtime_stream_key, fixture.runtime_stream_id
        )
        counts["redis_stream_entries"] = int(deleted or 0)
    keys = _redis_fixture_keys(fixture)
    if keys:
        deleted = _redis_json("DEL", *keys)
        counts["redis_keys"] = int(deleted or 0)
    return counts


def _redis_fixture_residual_counts(fixture: _FailureFixture) -> dict[str, int]:
    if not fixture.bid_idempotency_key:
        return {"redis_keys": 0, "redis_stream_entries": 0}
    marker_keys: set[str] = set()
    for marker in (
        fixture.lot_id,
        fixture.bidder_user_id,
        fixture.bid_idempotency_key,
    ):
        if marker:
            marker_keys.update(_redis_scan(f"*{marker}*"))
    stream_entries = 0
    markers = tuple(
        marker
        for marker in (
            fixture.lot_id,
            fixture.bidder_user_id,
            fixture.bid_idempotency_key,
        )
        if marker
    )
    if markers:
        for shard in range(RUNTIME_PROJECTION_SHARDS):
            stream = f"auction:runtime_events:shard:{shard}"
            for _, values in _redis_stream_entries(stream):
                payload = str(values.get("payload", ""))
                if any(marker in payload for marker in markers):
                    stream_entries += 1
    return {
        "redis_keys": len(marker_keys),
        "redis_stream_entries": stream_entries,
    }


def _recover_fixture_ids(fixture: _FailureFixture) -> None:
    if not any(
        (
            fixture.merchant_username,
            fixture.bidder_username,
            fixture.lot_title,
            fixture.address_detail,
            fixture.address_tag,
            fixture.run_goal,
        )
    ):
        return
    merchant = (
        "SELECT id FROM auction_users WHERE username="
        f"{_sql_literal(fixture.merchant_username)} LIMIT 1"
    )
    bidder = (
        "SELECT id FROM auction_users WHERE username="
        f"{_sql_literal(fixture.bidder_username)} LIMIT 1"
    )
    lot = (
        "SELECT id FROM auction_lots WHERE title="
        f"{_sql_literal(fixture.lot_title)} AND main_account_id=({merchant}) LIMIT 1"
    )
    room = (
        "SELECT id FROM auction_rooms WHERE main_account_id=("
        f"{merchant}) AND created_by_user_id=({merchant}) AND name='直播间' LIMIT 1"
    )
    address = (
        "SELECT id FROM user_delivery_addresses WHERE user_id=("
        f"{bidder}) AND detail={_sql_literal(fixture.address_detail)} "
        f"AND tag={_sql_literal(fixture.address_tag)} LIMIT 1"
    )
    run = (
        "SELECT id FROM agent_runs WHERE tenant_id=("
        f"{merchant}) AND operator_id=({merchant}) "
        f"AND goal={_sql_literal(fixture.run_goal)} LIMIT 1"
    )
    output = str(
        _mysql_output(
            "SELECT "
            + ", ".join(
                f"COALESCE(({query}), '')"
                for query in (merchant, bidder, lot, room, address, run)
            )
        )
    ).strip()
    if not output:
        return
    values = output.splitlines()[-1].split("\t")
    if len(values) != 6:
        raise RuntimeError(f"unexpected fixture marker lookup result: {output!r}")
    for field_name, value in zip(
        (
            "merchant_user_id",
            "bidder_user_id",
            "lot_id",
            "room_id",
            "address_id",
            "run_id",
        ),
        values,
        strict=True,
    ):
        if value:
            setattr(fixture, field_name, value)


def _cleanup_fixture(fixture: _FailureFixture) -> dict[str, int]:
    cleanup_errors: list[str] = []
    try:
        _wait_for_fixture_projection(fixture)
    except Exception as error:
        cleanup_errors.append(f"projection wait failed: {error}")
    if fixture.lot_id and fixture.merchant_token:
        try:
            fetched = _request_json(
                "GET", f"/api/lots/{fixture.lot_id}", token=fixture.merchant_token
            )
            _assert_ok(fetched)
            lot = fetched.get("lot")
            status = (
                str(lot.get("status", "")).removeprefix("LOT_STATUS_")
                if isinstance(lot, dict)
                else ""
            )
            if status in {"LIVE", "EXTENDED"}:
                cancelled = _request_json(
                    "POST",
                    f"/api/lots/{fixture.lot_id}/cancel",
                    token=fixture.merchant_token,
                    body={
                        "operator_id": fixture.merchant_user_id,
                        "reason": "failure-isolation fixture cleanup",
                    },
                )
                _assert_ok(cancelled)
        except BaseException as error:
            cleanup_errors.append(f"LIVE lot cancellation failed: {error}")

    def literal(value: str) -> str | None:
        return _sql_literal(value) if value else None

    def either(*predicates: str | None) -> str | None:
        present = [predicate for predicate in predicates if predicate]
        return " OR ".join(present) or None

    merchant, bidder = map(
        literal, (fixture.merchant_user_id, fixture.bidder_user_id)
    )
    lot, room, address, run = map(
        literal, (fixture.lot_id, fixture.room_id, fixture.address_id, fixture.run_id)
    )
    user_values = [value for value in (merchant, bidder) if value]
    users = f"({', '.join(user_values)})" if user_values else None
    exact_order = (
        f"source='auction' AND main_account_id={merchant} AND user_id={bidder}"
        if merchant and bidder
        else None
    )
    order_lookup = (
        f"order_id IN (SELECT id FROM user_orders WHERE {exact_order})"
        if exact_order
        else None
    )
    scoped = [
        ("agent_run_events", f"run_id={run}" if run else None),
        ("agent_runs", f"id={run}" if run else None),
        ("user_order_items", either(f"lot_id={lot}" if lot else None, order_lookup)),
        (
            "user_order_payments",
            either(
                f"lot_id={lot}" if lot else None,
                f"user_id={bidder}" if bidder else None,
                order_lookup,
            ),
        ),
        ("user_orders", exact_order),
        (
            "auction_deposit_holds",
            f"lot_id={lot} AND buyer_user_id={bidder}" if lot and bidder else None,
        ),
        ("auction_lot_participants", f"lot_id={lot}" if lot else None),
        ("auction_lot_stats", f"lot_id={lot}" if lot else None),
        ("auction_runtime_projection_offsets", f"lot_id={lot}" if lot else None),
        (
            "auction_bids",
            f"lot_id={lot}" + (f" AND user_id={bidder}" if bidder else "")
            if lot
            else None,
        ),
        (
            "auction_events",
            either(f"lot_id={lot}" if lot else None, f"room_id={room}" if room else None),
        ),
        (
            "asset_files",
            either(
                f"owner_user_id IN {users}" if users else None,
                f"main_account_id={merchant}" if merchant else None,
                f"room_id={room}" if room else None,
                f"attached_lot_id={lot}" if lot else None,
            ),
        ),
        ("auction_room_states", f"room_id={room}" if room else None),
        (
            "auction_lots",
            f"id={lot}" + (f" AND main_account_id={merchant}" if merchant else "")
            if lot
            else None,
        ),
        (
            "auction_rooms",
            f"id={room}" + (f" AND main_account_id={merchant}" if merchant else "")
            if room
            else None,
        ),
        (
            "user_delivery_addresses",
            f"id={address}" + (f" AND user_id={bidder}" if bidder else "")
            if address
            else None,
        ),
        ("auction_user_permissions", f"user_id IN {users}" if users else None),
        ("auction_user_roles", f"user_id IN {users}" if users else None),
        ("auction_user_sessions", f"user_id IN {users}" if users else None),
        ("auction_users", f"id IN {users}" if users else None),
    ]
    statements = ["START TRANSACTION"]
    for table, predicate in scoped:
        if predicate:
            _cleanup_delete(statements, table, predicate)
    statements.append("COMMIT")
    counts: dict[str, int] = {}
    try:
        output = _mysql_output("; ".join(statements) + ";")
        for line in str(output).splitlines():
            table, count = line.split("\t", 1)
            counts[table] = int(count)
    except Exception as error:
        cleanup_errors.append(f"exact MySQL cleanup failed: {error}")
    try:
        counts.update(_cleanup_redis_fixture(fixture))
    except Exception as error:
        cleanup_errors.append(f"exact Redis cleanup failed: {error}")
    fixture.cleanup_counts = counts
    if cleanup_errors:
        raise RuntimeError("; ".join(cleanup_errors))
    return counts


@contextmanager
def _preserve_error_while_cleaning(
    fixture: _FailureFixture,
) -> Iterator[None]:
    def finish() -> None:
        errors: list[str] = []
        try:
            _recover_fixture_ids(fixture)
        except Exception as error:
            errors.append(f"marker recovery failed: {error}")
        try:
            _recover_redis_bid_metadata(fixture)
        except Exception as error:
            errors.append(f"Redis marker recovery failed: {error}")
        try:
            fixture.cleanup_counts = _cleanup_fixture(fixture)
        except Exception as error:
            errors.append(f"fixture cleanup failed: {error}")
        try:
            _assert_fixture_clean(fixture)
        except Exception as error:
            errors.append(f"residual check failed: {error}")
        if errors:
            raise RuntimeError("; ".join(errors))

    try:
        yield
    except BaseException:
        try:
            finish()
        except Exception as cleanup_error:
            warnings.warn(
                f"failure-isolation cleanup also failed: {cleanup_error}",
                RuntimeWarning,
                stacklevel=2,
            )
        raise
    else:
        finish()


def test_file_lock_excludes_another_process_and_releases_after_exception(
    tmp_path: Path,
) -> None:
    lock_path = tmp_path / "failure-isolation.lock"
    child = (
        "from pathlib import Path; "
        "from tests.e2e.test_failure_isolation import _FileLock; "
        "_FileLock(Path(__import__('sys').argv[1]), timeout_seconds=0.15).acquire(); "
        "print('acquired')"
    )

    with pytest.raises(RuntimeError, match="boom"):
        with _FileLock(lock_path, timeout_seconds=1):
            blocked = subprocess.run(
                [sys.executable, "-c", child, str(lock_path)],
                cwd=Path(__file__).resolve().parents[2],
                capture_output=True,
                text=True,
                timeout=5,
            )
            assert blocked.returncode != 0
            assert "timed out" in blocked.stderr
            raise RuntimeError("boom")

    released = subprocess.run(
        [sys.executable, "-c", child, str(lock_path)],
        cwd=Path(__file__).resolve().parents[2],
        capture_output=True,
        text=True,
        timeout=5,
    )
    assert released.returncode == 0, released.stderr
    assert released.stdout.strip() == "acquired"


def test_agent_state_rejects_container_from_another_compose_project() -> None:
    inspect_data = {
        "Id": "wrong-container",
        "Config": {
            "Labels": {
                "com.docker.compose.service": "auction-agent",
                "com.docker.compose.project.working_dir": str(DEPLOY_DIR),
                "com.docker.compose.project.config_files": str(
                    DEPLOY_DIR / "other-compose.yml"
                ),
                "com.docker.compose.project": "deploy",
                "com.docker.compose.config-hash": "current-hash",
            }
        },
        "State": {"Running": True, "Health": {"Status": "healthy"}},
    }

    with pytest.raises(RuntimeError, match="config_files"):
        _validated_agent_state(
            inspect_data,
            expected_config_file=DEPLOY_DIR / "docker-compose.yml",
            expected_config_hash="current-hash",
            expected_project="deploy",
        )


def test_agent_state_rejects_other_checkout_with_same_suffix_and_hash(
    tmp_path: Path,
) -> None:
    expected = tmp_path / "checkout-a" / "repo" / "deploy" / "docker-compose.yml"
    wrong = tmp_path / "checkout-b" / "repo" / "deploy" / "docker-compose.yml"
    expected.parent.mkdir(parents=True)
    wrong.parent.mkdir(parents=True)
    expected.write_text("services: {}", encoding="utf-8")
    wrong.write_text("services: {}", encoding="utf-8")
    inspect_data = {
        "Id": "wrong-checkout-container",
        "Config": {
            "Labels": {
                "com.docker.compose.service": "auction-agent",
                "com.docker.compose.project.working_dir": str(wrong.parent),
                "com.docker.compose.project.config_files": str(wrong),
                "com.docker.compose.project": "deploy",
                "com.docker.compose.config-hash": "same-hash",
            }
        },
        "State": {"Running": True, "Health": {"Status": "healthy"}},
    }

    with pytest.raises(RuntimeError, match="config_files"):
        _validated_agent_state(
            inspect_data,
            expected_config_file=expected,
            expected_config_hash="same-hash",
            expected_project="deploy",
        )


def test_agent_lock_path_is_host_temporary_and_target_identity_based(
    tmp_path: Path,
) -> None:
    config_file = tmp_path / "checkout" / "deploy" / "docker-compose.yml"
    config_file.parent.mkdir(parents=True)
    config_file.write_text("services: {}", encoding="utf-8")
    first = _agent_lock_path(
        container_id="container-1",
        canonical_config_file=config_file,
        project="deploy",
    )
    second = _agent_lock_path(
        container_id="container-1",
        canonical_config_file=config_file,
        project="deploy",
    )

    assert first == second
    assert first.parent == Path(tempfile.gettempdir()) / "auction-agent-e2e-locks"
    assert first.name != ".agent-failure-isolation.lock"


def test_agent_state_rejects_container_with_stale_compose_config_hash() -> None:
    inspect_data = {
        "Id": "stale-container",
        "Config": {
            "Labels": {
                "com.docker.compose.service": "auction-agent",
                "com.docker.compose.project.working_dir": str(DEPLOY_DIR),
                "com.docker.compose.project.config_files": str(
                    DEPLOY_DIR / "docker-compose.yml"
                ),
                "com.docker.compose.config-hash": "stale-hash",
                "com.docker.compose.project": "deploy",
            }
        },
        "State": {"Running": True, "Health": {"Status": "healthy"}},
    }

    with pytest.raises(RuntimeError, match="config-hash"):
        _validated_agent_state(
            inspect_data,
            expected_config_file=DEPLOY_DIR / "docker-compose.yml",
            expected_config_hash="current-hash",
            expected_project="deploy",
        )


def test_agent_guard_restores_initial_running_state_when_stop_raises(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    calls: list[tuple[str, ...]] = []

    monkeypatch.setattr(
        __import__(__name__, fromlist=["_inspect_agent_state"]),
        "_inspect_agent_state",
        lambda: _AgentState(container_id="agent-1", running=True, healthy=True),
    )

    def fake_compose(*arguments: str) -> subprocess.CompletedProcess[str]:
        calls.append(arguments)
        if arguments[0] == "stop":
            raise subprocess.CalledProcessError(1, arguments)
        return subprocess.CompletedProcess(arguments, 0, "", "")

    monkeypatch.setattr(
        __import__(__name__, fromlist=["_compose"]), "_compose", fake_compose
    )
    monkeypatch.setattr(
        __import__(__name__, fromlist=["_wait_for_agent_health"]),
        "_wait_for_agent_health",
        lambda expected: None,
    )

    with pytest.raises(subprocess.CalledProcessError):
        with _stopped_agent(lock_path=tmp_path / "guard.lock"):
            raise AssertionError("body must not run")

    assert calls == [("stop", "auction-agent"), ("start", "auction-agent")]


def test_agent_guard_preserves_initially_stopped_state(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    calls: list[tuple[str, ...]] = []
    monkeypatch.setattr(
        __import__(__name__, fromlist=["_inspect_agent_state"]),
        "_inspect_agent_state",
        lambda: _AgentState(container_id="agent-1", running=False, healthy=False),
    )
    monkeypatch.setattr(
        __import__(__name__, fromlist=["_compose"]),
        "_compose",
        lambda *arguments: calls.append(arguments),
    )

    with _stopped_agent(lock_path=tmp_path / "guard.lock") as initial:
        assert initial.running is False

    assert calls == []


def test_fixture_cleanup_cancels_live_lot_before_exact_database_deletes(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    fixture = _FailureFixture(
        merchant_token="merchant-token",
        merchant_user_id="merchant-unique-id",
        bidder_user_id="bidder-unique-id",
        lot_id="lot-unique-id",
        room_id="room-unique-id",
        address_id="address-unique-id",
        run_id="run-unique-id",
    )
    calls: list[tuple[str, str]] = []

    def fake_request(method: str, path: str, **_: object) -> dict[str, object]:
        calls.append((method, path))
        if method == "GET":
            return {"result": {}, "lot": {"status": "LOT_STATUS_LIVE"}}
        return {"result": {}, "lot": {"status": "LOT_STATUS_CANCELLED"}}

    def fake_mysql(sql: object) -> str:
        assert isinstance(sql, str)
        calls.append(("SQL", sql))
        return "agent_runs\t1\nauction_users\t2"

    module = __import__(__name__, fromlist=["_request_json"])
    monkeypatch.setattr(module, "_request_json", fake_request)
    monkeypatch.setattr(module, "_mysql_output", fake_mysql)

    counts = _cleanup_fixture(fixture)

    assert calls[0] == ("GET", "/api/lots/lot-unique-id")
    assert calls[1] == ("POST", "/api/lots/lot-unique-id/cancel")
    assert calls[2][0] == "SQL"
    cleanup_sql = calls[2][1]
    for exact_id in (
        "merchant-unique-id",
        "bidder-unique-id",
        "lot-unique-id",
        "room-unique-id",
        "address-unique-id",
        "run-unique-id",
    ):
        assert exact_id in cleanup_sql
    assert "LIKE" not in cleanup_sql.upper()
    assert counts == {"agent_runs": 1, "auction_users": 2}


def test_cleanup_failure_does_not_mask_original_test_error(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    module = __import__(__name__, fromlist=["_cleanup_fixture"])
    monkeypatch.setattr(
        module,
        "_cleanup_fixture",
        lambda _: (_ for _ in ()).throw(RuntimeError("cleanup failed")),
    )

    with pytest.warns(RuntimeWarning, match="cleanup failed"):
        with pytest.raises(AssertionError, match="original failure"):
            with _preserve_error_while_cleaning(_FailureFixture()):
                raise AssertionError("original failure")


def test_cleanup_failure_is_raised_when_test_body_succeeded(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    module = __import__(__name__, fromlist=["_cleanup_fixture"])
    monkeypatch.setattr(
        module,
        "_cleanup_fixture",
        lambda _: (_ for _ in ()).throw(RuntimeError("cleanup failed")),
    )

    with pytest.raises(RuntimeError, match="cleanup failed"):
        with _preserve_error_while_cleaning(_FailureFixture()):
            pass


def test_api_cancel_pytest_failure_still_runs_exact_database_cleanup(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    mysql_calls: list[str] = []
    module = __import__(__name__, fromlist=["_request_json"])
    monkeypatch.setattr(
        module,
        "_request_json",
        lambda *_, **__: pytest.fail("cancel endpoint unavailable"),
    )
    monkeypatch.setattr(
        module,
        "_mysql_output",
        lambda sql: mysql_calls.append(str(sql)) or "auction_lots\t1",
    )
    fixture = _FailureFixture(
        merchant_token="token", merchant_user_id="merchant", lot_id="lot"
    )

    with pytest.raises(RuntimeError, match="cancel endpoint unavailable"):
        _cleanup_fixture(fixture)

    assert len(mysql_calls) == 1
    assert "DELETE FROM `auction_lots` WHERE id='lot'" in mysql_calls[0]


def test_partial_fixture_cleanup_never_uses_blank_identifiers(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: list[str] = []
    module = __import__(__name__, fromlist=["_mysql_output"])
    monkeypatch.setattr(
        module,
        "_mysql_output",
        lambda sql: captured.append(str(sql)) or "auction_users\t1",
    )

    _cleanup_fixture(_FailureFixture(merchant_user_id="merchant-only"))

    assert len(captured) == 1
    assert "merchant-only" in captured[0]
    assert "''" not in captured[0]


def test_fixture_records_created_ids_before_later_setup_step_fails(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    fixture = _FailureFixture(merchant_token="merchant", bidder_token="bidder")
    lot_calls = iter(
        [
            {"result": {}, "lot": {"id": "lot-created", "roomId": "room-created"}},
            RuntimeError("lot patch failed"),
        ]
    )

    def fake_lot_request(*_: object, **__: object) -> dict[str, object]:
        result = next(lot_calls)
        if isinstance(result, Exception):
            raise result
        return result

    module = __import__(__name__, fromlist=["_request_json"])
    monkeypatch.setattr(module, "_request_json", fake_lot_request)
    with pytest.raises(RuntimeError, match="lot patch failed"):
        _create_active_lot(fixture)
    assert (fixture.lot_id, fixture.room_id) == ("lot-created", "room-created")

    address_calls = iter(
        [
            {"result": {}, "address": {"id": "address-created"}},
            RuntimeError("deposit failed"),
        ]
    )

    def fake_address_request(*_: object, **__: object) -> dict[str, object]:
        result = next(address_calls)
        if isinstance(result, Exception):
            raise result
        return result

    fixture.lot_id = "lot-created"
    monkeypatch.setattr(module, "_request_json", fake_address_request)
    with pytest.raises(RuntimeError, match="deposit failed"):
        _hold_bidder_deposit(fixture)
    assert fixture.address_id == "address-created"


def test_fixture_records_all_request_markers_before_any_request() -> None:
    fixture = _new_failure_fixture()

    markers = (
        fixture.merchant_username,
        fixture.merchant_nickname,
        fixture.bidder_username,
        fixture.bidder_nickname,
        fixture.lot_title,
        fixture.address_detail,
        fixture.address_tag,
        fixture.deposit_idempotency_key,
        fixture.bid_idempotency_key,
        fixture.run_goal,
    )
    assert all(markers)
    assert len(set(markers)) == len(markers)


def test_redis_fixture_keys_cover_only_exact_lot_and_idempotency_state() -> None:
    fixture = _FailureFixture(
        lot_id="lot-exact",
        bidder_user_id="bidder-exact",
        bid_idempotency_key="idem-exact",
    )

    assert _redis_fixture_keys(fixture) == (
        "auction:lot:{lot-exact}:state",
        "auction:lot:{lot-exact}:ranking",
        "auction:lot:{lot-exact}:rankmeta",
        "auction:lot:{lot-exact}:participants",
        "auction:lot:{lot-exact}:recent",
        "auction:lot:{lot-exact}:idem:bidder-exact:idem-exact",
        "auction:idem:lot-exact:bidder-exact:idem-exact",
    )


def test_stream_entry_match_requires_every_fixture_identity_marker() -> None:
    fixture = _FailureFixture(
        merchant_user_id="tenant-exact",
        bidder_user_id="bidder-exact",
        lot_id="lot-exact",
        bid_idempotency_key="idem-exact",
        runtime_event_id="event-exact",
    )
    values = {
        "event_id": "event-exact",
        "lot_id": "lot-exact",
        "payload": json.dumps(
            {
                "event_id": "event-exact",
                "lot_id": "lot-exact",
                "idempotency_key": "idem-exact",
                "bid": {"user_id": "bidder-exact"},
                "updated_lot": {"main_account_id": "tenant-exact"},
            }
        ),
    }

    assert _stream_entry_matches_fixture(fixture, values)
    for field, wrong_value in (
        ("event_id", "other-event"),
        ("lot_id", "other-lot"),
        ("idempotency_key", "other-idem"),
    ):
        changed = json.loads(str(values["payload"]))
        changed[field] = wrong_value
        assert not _stream_entry_matches_fixture(
            fixture, {**values, "payload": json.dumps(changed)}
        )
    changed = json.loads(str(values["payload"]))
    changed["bid"]["user_id"] = "other-bidder"
    assert not _stream_entry_matches_fixture(
        fixture, {**values, "payload": json.dumps(changed)}
    )
    changed = json.loads(str(values["payload"]))
    changed["updated_lot"]["main_account_id"] = "other-tenant"
    assert not _stream_entry_matches_fixture(
        fixture, {**values, "payload": json.dumps(changed)}
    )


def test_exact_marker_recovery_finds_ids_after_ambiguous_commits(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    fixture = _new_failure_fixture()
    captured: list[str] = []

    def fake_mysql(sql: object) -> str:
        captured.append(str(sql))
        return "merchant-id\tbidder-id\tlot-id\troom-id\taddress-id\trun-id"

    monkeypatch.setattr(
        __import__(__name__, fromlist=["_mysql_output"]), "_mysql_output", fake_mysql
    )
    _recover_fixture_ids(fixture)

    assert (
        fixture.merchant_user_id,
        fixture.bidder_user_id,
        fixture.lot_id,
        fixture.room_id,
        fixture.address_id,
        fixture.run_id,
    ) == ("merchant-id", "bidder-id", "lot-id", "room-id", "address-id", "run-id")
    assert len(captured) == 1
    lookup_sql = captured[0]
    for marker in (
        fixture.merchant_username,
        fixture.bidder_username,
        fixture.lot_title,
        fixture.address_detail,
        fixture.address_tag,
        fixture.run_goal,
    ):
        assert marker in lookup_sql
    assert "LIKE" not in lookup_sql.upper()


@pytest.mark.parametrize(
    ("stage", "recovered_field", "recovered_id"),
    (
        ("register", "merchant_user_id", "committed-user"),
        ("lot", "lot_id", "committed-lot"),
        ("run", "run_id", "committed-run"),
    ),
)
def test_ambiguous_commit_still_recovers_cleans_and_checks_residuals(
    monkeypatch: pytest.MonkeyPatch,
    stage: str,
    recovered_field: str,
    recovered_id: str,
) -> None:
    fixture = _new_failure_fixture()
    calls: list[str] = []
    residuals = {recovered_id: 1}
    module = __import__(__name__, fromlist=["_recover_fixture_ids"])

    def fake_recover(value: _FailureFixture) -> None:
        calls.append("recover")
        setattr(value, recovered_field, recovered_id)

    def fake_cleanup(value: _FailureFixture) -> dict[str, int]:
        assert getattr(value, recovered_field) == recovered_id
        calls.append("cleanup")
        residuals[recovered_id] = 0
        return {stage: 1}

    def fake_residual_check(_: _FailureFixture) -> None:
        assert not any(residuals.values())
        calls.append("residual")

    monkeypatch.setattr(module, "_recover_fixture_ids", fake_recover)
    monkeypatch.setattr(module, "_cleanup_fixture", fake_cleanup)
    monkeypatch.setattr(
        module,
        "_assert_fixture_clean",
        fake_residual_check,
    )

    with pytest.raises(ValueError, match=f"{stage} response lost"):
        with _preserve_error_while_cleaning(fixture):
            raise ValueError(f"{stage} response lost")

    assert calls == ["recover", "cleanup", "residual"]
    assert residuals == {recovered_id: 0}


def _wait_for_agent_health(expected: str, timeout_seconds: float = 45) -> None:
    deadline = time.monotonic() + timeout_seconds
    last_health = "missing"
    while time.monotonic() < deadline:
        completed = _compose("ps", "--format", "json", "auction-agent")
        rows = [json.loads(line) for line in completed.stdout.splitlines() if line.strip()]
        last_health = str(rows[0].get("Health", "")) if rows else "missing"
        if last_health == expected:
            return
        time.sleep(0.25)
    raise AssertionError(f"auction-agent health stayed {last_health!r}, expected {expected!r}")


def _register(
    path: str, *, username: str, nickname: str, password: str
) -> tuple[str, str]:
    reply = _request_json(
        "POST",
        path,
        body={"username": username, "password": password, "nickname": nickname},
    )
    _assert_ok(reply)
    tokens = reply.get("tokens")
    user = reply.get("user")
    assert isinstance(tokens, dict), reply
    assert isinstance(user, dict), reply
    access_token = str(tokens.get("accessToken", ""))
    user_id = str(user.get("id", ""))
    assert access_token and user_id, reply
    return access_token, user_id


def _create_active_lot(fixture: _FailureFixture) -> None:
    created = _request_json(
        "POST",
        "/api/lots/drafts",
        token=fixture.merchant_token,
        body={
            "room_id": "",
            "title": fixture.lot_title,
            "description": "Unique host-orchestrated failure-isolation fixture",
        },
    )
    _assert_ok(created)
    lot = created.get("lot")
    assert isinstance(lot, dict), created
    fixture.lot_id = str(lot.get("id", ""))
    fixture.room_id = str(lot.get("roomId") or lot.get("room_id") or "")
    assert fixture.lot_id and fixture.room_id, created

    patched = _request_json(
        "PATCH",
        f"/api/lots/{fixture.lot_id}/draft",
        token=fixture.merchant_token,
        body={
            "title": fixture.lot_title,
            "description": "Unique host-orchestrated failure-isolation fixture",
            "image_url": "https://example.com/agent-outage-bid.jpg",
            "rule": {
                "start_price": {"amount": 10000, "currency": "CNY"},
                "min_increment": {"amount": 1000, "currency": "CNY"},
                "duration_seconds": 300,
                "anti_snipe_window_seconds": 15,
                "anti_snipe_extend_seconds": 15,
                "max_extend_count": 3,
            },
        },
    )
    _assert_ok(patched)
    started = _request_json(
        "POST", f"/api/lots/{fixture.lot_id}/start", token=fixture.merchant_token, body={}
    )
    _assert_ok(started)
    started_lot = started.get("lot")
    assert isinstance(started_lot, dict), started
    assert str(started_lot.get("status", "")).endswith("LIVE"), started
    assert str(started_lot.get("roomId") or started_lot.get("room_id") or "") == fixture.room_id


def _hold_bidder_deposit(fixture: _FailureFixture) -> None:
    address = _request_json(
        "POST",
        "/api/shop/addresses",
        token=fixture.bidder_token,
        body={
            "address": {
                "receiverName": "Agent Isolation Bidder",
                "phone": "13800138000",
                "province": "上海市",
                "city": "上海市",
                "district": "浦东新区",
                "street": "世纪大道",
                "detail": fixture.address_detail,
                "tag": fixture.address_tag,
                "isDefault": True,
            }
        },
    )
    _assert_ok(address)
    address_value = address.get("address")
    assert isinstance(address_value, dict), address
    fixture.address_id = str(address_value.get("id", ""))
    assert fixture.address_id, address

    hold = _request_json(
        "POST",
        f"/api/lots/{fixture.lot_id}/deposit-holds/mock-pay",
        token=fixture.bidder_token,
        body={
            "addressId": fixture.address_id,
            "idempotencyKey": fixture.deposit_idempotency_key,
        },
    )
    _assert_ok(hold)
    assert hold.get("paid") is True, hold


def _fixture_residual_counts(fixture: _FailureFixture) -> dict[str, int]:
    queries: dict[str, str] = {}
    user_ids = [
        _sql_literal(value)
        for value in (fixture.merchant_user_id, fixture.bidder_user_id)
        if value
    ]
    if user_ids:
        queries["users_by_id"] = (
            "SELECT COUNT(*) FROM auction_users WHERE id IN ("
            + ", ".join(user_ids)
            + ")"
        )
    usernames = [
        _sql_literal(value)
        for value in (fixture.merchant_username, fixture.bidder_username)
        if value
    ]
    if usernames:
        queries["users_by_name"] = (
            "SELECT COUNT(*) FROM auction_users WHERE username IN ("
            + ", ".join(usernames)
            + ")"
        )
    if fixture.lot_id:
        lot = _sql_literal(fixture.lot_id)
        queries["lot"] = f"SELECT COUNT(*) FROM auction_lots WHERE id={lot}"
        queries["live_lot"] = (
            f"SELECT COUNT(*) FROM auction_lots WHERE id={lot} AND status IN (2, 7)"
        )
    if fixture.room_id:
        queries["room"] = (
            "SELECT COUNT(*) FROM auction_rooms WHERE id="
            f"{_sql_literal(fixture.room_id)}"
        )
    if fixture.address_id:
        queries["address"] = (
            "SELECT COUNT(*) FROM user_delivery_addresses WHERE id="
            f"{_sql_literal(fixture.address_id)}"
        )
    if fixture.run_id:
        run = _sql_literal(fixture.run_id)
        queries["agent_run"] = f"SELECT COUNT(*) FROM agent_runs WHERE id={run}"
        queries["agent_events"] = (
            f"SELECT COUNT(*) FROM agent_run_events WHERE run_id={run}"
        )
    if fixture.merchant_user_id and fixture.lot_title:
        queries["lot_by_marker"] = (
            "SELECT COUNT(*) FROM auction_lots WHERE main_account_id="
            f"{_sql_literal(fixture.merchant_user_id)} AND title="
            f"{_sql_literal(fixture.lot_title)}"
        )
    if fixture.merchant_user_id and fixture.run_goal:
        merchant = _sql_literal(fixture.merchant_user_id)
        queries["run_by_marker"] = (
            "SELECT COUNT(*) FROM agent_runs WHERE tenant_id="
            f"{merchant} AND operator_id={merchant} AND goal="
            f"{_sql_literal(fixture.run_goal)}"
        )
    if not queries:
        return {}
    output = _mysql_output(
        " UNION ALL ".join(
            f"SELECT {_sql_literal(name)}, ({query})" for name, query in queries.items()
        )
    )
    return {
        name: int(count)
        for name, count in (line.split("\t", 1) for line in str(output).splitlines())
    }


def _assert_fixture_clean(fixture: _FailureFixture) -> None:
    if not any(
        (
            fixture.merchant_user_id,
            fixture.bidder_user_id,
            fixture.merchant_username,
            fixture.bidder_username,
            fixture.lot_id,
            fixture.run_id,
        )
    ):
        return
    residuals = _fixture_residual_counts(fixture)
    redis_residuals = _redis_fixture_residual_counts(fixture)
    if any(residuals.values()) or any(redis_residuals.values()):
        raise AssertionError(
            "failure-isolation residuals: "
            f"mysql={residuals}, redis={redis_residuals}"
        )


def test_agent_outage_preserves_successful_bid_path_and_keeps_new_runs_projectable() -> None:
    with _stopped_agent() as initial_agent_state:
        with urlopen(f"{BASE_URL}/readyz", timeout=10) as response:
            ready = json.loads(response.read())
        assert ready == {"ok": True}

        merchant_token, _ = _access_token_and_tenant()
        lots = _admin_inventory(merchant_token)
        assert isinstance(lots, list)
        before_snapshot = _business_table_snapshot(
            additional_excluded_tables=SHARED_RUNTIME_COORDINATION_TABLES
        )
        fixture = _new_failure_fixture()
        with _preserve_error_while_cleaning(fixture):
            (
                fixture.merchant_token,
                fixture.merchant_user_id,
            ) = _register(
                "/api/merchants/register",
                username=fixture.merchant_username,
                nickname=fixture.merchant_nickname,
                password="merchantpass123",
            )
            (
                fixture.bidder_token,
                fixture.bidder_user_id,
            ) = _register(
                "/api/users/register",
                username=fixture.bidder_username,
                nickname=fixture.bidder_nickname,
                password="password123",
            )
            _create_active_lot(fixture)
            _hold_bidder_deposit(fixture)
            bid = _request_json(
                "POST",
                f"/api/lots/{fixture.lot_id}/bid",
                token=fixture.bidder_token,
                body={
                    "amount": {"amount": 11000, "currency": "CNY"},
                    "idempotency_key": fixture.bid_idempotency_key,
                },
            )
            _assert_ok(bid)
            assert bid.get("accepted") is True, bid
            bid_value = bid.get("bid")
            assert isinstance(bid_value, dict), bid
            assert str(bid_value.get("userId", "")) == fixture.bidder_user_id, bid
            fixture.bid_id = str(bid_value.get("id", ""))
            assert fixture.bid_id, bid

            created_run = _request_json(
                "POST",
                "/api/admin/agent/runs",
                token=fixture.merchant_token,
                body={"goal": fixture.run_goal},
                idempotency_key=fixture.run_idempotency_key,
            )
            _assert_ok(created_run)
            run = created_run.get("run")
            assert isinstance(run, dict), created_run
            fixture.run_id = str(run.get("runId") or run.get("run_id") or "")
            assert fixture.run_id, run
            assert run.get("status") == "CREATED", run
            assert not run.get("errorCode"), run

            queried_run = _request_json(
                "GET",
                f"/api/admin/agent/runs/{fixture.run_id}",
                token=fixture.merchant_token,
            )
            _assert_ok(queried_run)
            durable_run = queried_run.get("run")
            assert isinstance(durable_run, dict), queried_run
            assert (
                str(durable_run.get("runId") or durable_run.get("run_id") or "")
                == fixture.run_id
            )
            assert durable_run.get("status") == "CREATED", durable_run
            assert not durable_run.get("errorCode"), durable_run

        after_snapshot = _business_table_snapshot(
            additional_excluded_tables=SHARED_RUNTIME_COORDINATION_TABLES
        )
        changed_tables = sorted(
            table
            for table in set(before_snapshot) | set(after_snapshot)
            if before_snapshot.get(table) != after_snapshot.get(table)
        )
        assert after_snapshot == before_snapshot, (
            "failure-isolation fixture cleanup must restore every business table; "
            f"changed={changed_tables}"
        )
        residuals = _fixture_residual_counts(fixture)
        redis_residuals = _redis_fixture_residual_counts(fixture)
        assert not any(residuals.values()), residuals
        assert not any(redis_residuals.values()), redis_residuals
        print(
            "failure-isolation evidence "
            + json.dumps(
                {
                    "initial_agent": {
                        "running": initial_agent_state.running,
                        "healthy": initial_agent_state.healthy,
                    },
                    "cleanup_rows": fixture.cleanup_counts,
                    "mysql_residuals": residuals,
                    "redis_residuals": redis_residuals,
                },
                sort_keys=True,
            )
        )


def test_ambiguous_outage_run_reconciles_after_agent_returns() -> None:
    fixture = _new_failure_fixture()
    fixture.merchant_token, _ = _access_token_and_tenant()

    with _preserve_error_while_cleaning(fixture):
        with _stopped_agent():
            created_reply = _request_json(
                "POST",
                "/api/admin/agent/runs",
                token=fixture.merchant_token,
                body={"goal": fixture.run_goal},
                idempotency_key=fixture.run_idempotency_key,
            )
            _assert_ok(created_reply)
            created_run = created_reply.get("run")
            assert isinstance(created_run, dict), created_reply
            fixture.run_id = str(
                created_run.get("runId") or created_run.get("run_id") or ""
            )
            assert fixture.run_id, created_run
            assert created_run.get("status") == "CREATED", created_run
            assert not created_run.get("errorCode"), created_run

        completed_run = _wait_for_terminal_run(
            fixture.merchant_token, fixture.run_id
        )
        assert str(
            completed_run.get("runId") or completed_run.get("run_id") or ""
        ) == fixture.run_id
        assert completed_run.get("status") == "COMPLETED", completed_run
        assert isinstance(completed_run.get("proposal"), dict), completed_run

        durable = str(
            _mysql_output(
                "SELECT status, projection_complete, last_runtime_source_seq, "
                "(SELECT COUNT(*) FROM agent_run_events WHERE run_id="
                f"{_sql_literal(fixture.run_id)} AND event_type LIKE 'tool.%'), "
                "(SELECT COUNT(*) FROM agent_run_events WHERE run_id="
                f"{_sql_literal(fixture.run_id)} AND event_type='run.completed') "
                "FROM agent_runs WHERE id="
                f"{_sql_literal(fixture.run_id)}"
            )
        ).strip()
        status, projection_complete, source_seq, tool_events, terminal_events = (
            durable.splitlines()[-1].split("\t")
        )
        assert status == "COMPLETED", durable
        assert projection_complete == "1", durable
        assert int(source_seq) > 0, durable
        assert int(tool_events) > 0, durable
        assert terminal_events == "1", durable
