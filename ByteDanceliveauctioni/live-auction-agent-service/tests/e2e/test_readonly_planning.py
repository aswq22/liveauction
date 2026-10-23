"""Docker Compose end-to-end coverage for the read-only planning workflow."""

from __future__ import annotations

from collections import deque
import json
import os
from pathlib import Path
import queue
import subprocess
import sys
import threading
import time
from typing import Any, Callable
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import Request, urlopen
import warnings
import uuid

import pytest


REPOSITORY_ROOT = Path(__file__).resolve().parents[3]
DEPLOY_DIR = REPOSITORY_ROOT / "live-auction-bid-backend" / "deploy"
BASE_URL = os.getenv(
    "AGENT_E2E_BASE_URL",
    os.getenv("AUCTION_E2E_BASE_URL", "http://127.0.0.1:18080"),
).rstrip("/")
TERMINAL_STATUSES = {"COMPLETED", "FAILED", "CANCELLED"}
INVENTORY_STATUSES = {"DRAFT", "READY", "QUEUED"}
LEGACY_COUNT_TABLES = (
    "auction_rooms",
    "auction_lots",
    "auction_bids",
    "auction_lot_stats",
    "auction_lot_participants",
    "auction_events",
    "user_orders",
    "user_order_items",
    "user_order_payments",
    "auction_deposit_holds",
)
AGENT_ORCHESTRATION_TABLES = {"agent_runs", "agent_run_events"}


def _compose_file() -> Path:
    configured = os.getenv(
        "AGENT_E2E_COMPOSE_FILE", str(DEPLOY_DIR / "docker-compose.yml")
    )
    try:
        return Path(configured).resolve(strict=True)
    except OSError as error:
        raise RuntimeError(
            f"AGENT_E2E_COMPOSE_FILE is not a readable file: {configured!r}"
        ) from error


def _compose(*arguments: str) -> subprocess.CompletedProcess[str]:
    compose_file = _compose_file()
    return subprocess.run(
        [
            "docker",
            "compose",
            "--file",
            str(compose_file),
            "--profile",
            "agent",
            *arguments,
        ],
        cwd=compose_file.parent,
        check=True,
        capture_output=True,
        text=True,
    )


def _compose_configuration() -> dict[str, Any]:
    completed = _compose("config", "--format", "json")
    return json.loads(completed.stdout)


def _request_json(
    method: str,
    path: str,
    *,
    token: str | None = None,
    body: dict[str, Any] | None = None,
    idempotency_key: str | None = None,
) -> dict[str, Any]:
    payload = None if body is None else json.dumps(body).encode("utf-8")
    headers = {"Accept": "application/json"}
    if payload is not None:
        headers["Content-Type"] = "application/json"
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if idempotency_key:
        headers["Idempotency-Key"] = idempotency_key
    request = Request(f"{BASE_URL}{path}", data=payload, headers=headers, method=method)
    try:
        with urlopen(request, timeout=10) as response:
            return json.loads(response.read().decode("utf-8"))
    except HTTPError as error:
        response_body = error.read().decode("utf-8", errors="replace")
        pytest.fail(f"{method} {path} returned HTTP {error.code}: {response_body}")
    except URLError as error:
        pytest.fail(f"{method} {path} could not reach the Compose backend: {error}")


def _assert_ok(reply: dict[str, Any]) -> None:
    result = reply.get("result")
    assert isinstance(result, dict), reply
    # Proto JSON and the custom agent HTTP transport may omit the zero-valued
    # success code; any explicit non-zero code is still a failure.
    assert int(result.get("code", 0)) == 0, reply


def _access_token_and_tenant() -> tuple[str, str]:
    reply = _request_json(
        "POST",
        "/api/users/login",
        body={"username": "main", "password": "main_dev_password"},
    )
    _assert_ok(reply)
    tokens = reply.get("tokens")
    user = reply.get("user")
    assert isinstance(tokens, dict), reply
    assert isinstance(user, dict), reply
    token = str(tokens.get("accessToken") or tokens.get("access_token") or "").strip()
    tenant_id = str(
        user.get("mainAccountId") or user.get("main_account_id") or user.get("id") or ""
    ).strip()
    assert token, reply
    assert tenant_id, reply
    return token, tenant_id


def _admin_inventory(token: str) -> list[dict[str, Any]]:
    query = urlencode({"page": 1, "pageSize": 100})
    reply = _request_json("GET", f"/api/admin/lots?{query}", token=token)
    _assert_ok(reply)
    lots = reply.get("lots")
    assert isinstance(lots, list), reply
    return [lot for lot in lots if isinstance(lot, dict)]


def _status_name(lot: dict[str, Any]) -> str:
    return str(lot.get("status", "")).removeprefix("LOT_STATUS_")


def _ensure_seeded_inventory_lot(token: str) -> list[dict[str, Any]]:
    inventory = [lot for lot in _admin_inventory(token) if _status_name(lot) in INVENTORY_STATUSES]
    if inventory:
        return inventory
    reply = _request_json(
        "POST",
        "/api/lots/drafts",
        token=token,
        body={
            "title": "Agent E2E seeded inventory lot",
            "description": "Reusable fixture for the read-only planning integration test",
        },
    )
    _assert_ok(reply)
    created = reply.get("lot")
    assert isinstance(created, dict) and str(created.get("id", "")).strip(), reply
    inventory = [lot for lot in _admin_inventory(token) if _status_name(lot) in INVENTORY_STATUSES]
    assert any(lot.get("id") == created.get("id") for lot in inventory)
    return inventory


def _legacy_business_table_counts() -> dict[str, int]:
    selects = [f"SELECT '{table}', COUNT(*) FROM {table}" for table in LEGACY_COUNT_TABLES]
    completed = _compose(
        "exec",
        "-T",
        "mysql",
        "mysql",
        "-N",
        "-B",
        "-uauction",
        "-pauction_dev",
        "live_auction",
        "-e",
        " UNION ALL ".join(selects),
    )
    counts: dict[str, int] = {}
    for line in completed.stdout.splitlines():
        table, raw_count = line.split("\t", 1)
        counts[table] = int(raw_count)
    assert set(counts) == set(LEGACY_COUNT_TABLES), completed.stdout
    return counts


def _business_table_snapshot(
    *, additional_excluded_tables: frozenset[str] = frozenset()
) -> dict[
    str, tuple[tuple[tuple[str, ...], ...], tuple[str, ...]]
]:
    def capture(session: _MySQLCLISession) -> dict[
        str, tuple[tuple[tuple[str, ...], ...], tuple[str, ...]]
    ]:
        excluded_tables = AGENT_ORCHESTRATION_TABLES | additional_excluded_tables
        excluded = ", ".join(_sql_literal(table) for table in sorted(excluded_tables))
        metadata = session.query(
            "SET SESSION TRANSACTION ISOLATION LEVEL REPEATABLE READ; "
            "START TRANSACTION WITH CONSISTENT SNAPSHOT; "
            "SELECT HEX(columns_meta.TABLE_NAME), columns_meta.ORDINAL_POSITION, "
            "HEX(columns_meta.COLUMN_NAME), HEX(columns_meta.COLUMN_TYPE), "
            "columns_meta.IS_NULLABLE, "
            "COALESCE(HEX(columns_meta.CHARACTER_SET_NAME), ''), "
            "COALESCE(HEX(columns_meta.COLLATION_NAME), '') "
            "FROM information_schema.COLUMNS AS columns_meta "
            "INNER JOIN information_schema.TABLES AS tables_meta "
            "ON tables_meta.TABLE_SCHEMA=columns_meta.TABLE_SCHEMA "
            "AND tables_meta.TABLE_NAME=columns_meta.TABLE_NAME "
            "WHERE columns_meta.TABLE_SCHEMA=DATABASE() "
            "AND tables_meta.TABLE_TYPE='BASE TABLE' "
            f"AND columns_meta.TABLE_NAME NOT IN ({excluded}) "
            "ORDER BY columns_meta.TABLE_NAME, columns_meta.ORDINAL_POSITION",
            "AGENT_SNAPSHOT_METADATA_END",
        )
        schemas: dict[str, list[tuple[str, ...]]] = {}
        for line in metadata.splitlines():
            fields = line.split("\t")
            assert len(fields) == 7, line
            (
                table_hex,
                ordinal,
                column_hex,
                column_type_hex,
                nullable,
                charset_hex,
                collation_hex,
            ) = fields
            table = bytes.fromhex(table_hex).decode("utf-8")
            schemas.setdefault(table, []).append(
                (
                    ordinal,
                    bytes.fromhex(column_hex).decode("utf-8"),
                    bytes.fromhex(column_type_hex).decode("utf-8"),
                    nullable,
                    bytes.fromhex(charset_hex).decode("utf-8"),
                    bytes.fromhex(collation_hex).decode("utf-8"),
                )
            )
        assert schemas, "the Compose MySQL schema must contain non-Agent tables"

        statements = []
        for table, columns in sorted(schemas.items()):
            table_hex = table.encode("utf-8").hex().upper()
            quoted_table = _sql_identifier(table)
            value_encodings = []
            for _, column, *_ in columns:
                quoted_column = _sql_identifier(column)
                binary_value = f"CAST({quoted_column} AS BINARY)"
                value_encodings.append(
                    "IF("
                    f"{quoted_column} IS NULL, 'N;', "
                    f"CONCAT('V', OCTET_LENGTH({binary_value}), ':', "
                    f"HEX({binary_value}), ';')"
                    ")"
                )
            encoded_row = "CONCAT(" + ", ".join(value_encodings) + ")"
            statements.append(f"SELECT 'T', '{table_hex}', COUNT(*) FROM {quoted_table}")
            statements.append(
                f"SELECT 'R', '{table_hex}', {encoded_row} FROM {quoted_table}"
            )
        statements.append("COMMIT")
        rows_output = session.query(
            "; ".join(statements), "AGENT_SNAPSHOT_ROWS_END"
        )

        rows: dict[str, list[str]] = {table: [] for table in schemas}
        expected_counts: dict[str, int] = {}
        for line in rows_output.splitlines():
            marker, table_hex, value = line.split("\t", 2)
            table = bytes.fromhex(table_hex).decode("utf-8")
            assert table in schemas, line
            if marker == "T":
                expected_counts[table] = int(value)
            else:
                assert marker == "R", line
                rows[table].append(value)
        assert set(expected_counts) == set(schemas)
        for table in schemas:
            assert len(rows[table]) == expected_counts[table], table
        return {
            table: (tuple(schemas[table]), tuple(sorted(rows[table])))
            for table in sorted(schemas)
        }

    return _mysql_output(capture)


class _MySQLCLISession:
    def __init__(
        self,
        *,
        process: Any | None = None,
        query_timeout_seconds: float = 30,
        close_timeout_seconds: float = 10,
    ) -> None:
        self._process = process or subprocess.Popen(
            [
                "docker",
                "compose",
                "--profile",
                "agent",
                "exec",
                "-T",
                "mysql",
                "mysql",
                "-N",
                "-B",
                "--unbuffered",
                "-uauction",
                "-pauction_dev",
                "live_auction",
            ],
            cwd=DEPLOY_DIR,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
        self._query_timeout_seconds = query_timeout_seconds
        self._close_timeout_seconds = close_timeout_seconds
        self._stdout_lines: queue.Queue[str | None] = queue.Queue()
        self._stderr_lines: deque[str] = deque(maxlen=64)
        self._reader_threads = [
            threading.Thread(
                target=self._drain_stdout,
                name=f"mysql-cli-stdout-{id(self)}",
                daemon=True,
            ),
            threading.Thread(
                target=self._drain_stderr,
                name=f"mysql-cli-stderr-{id(self)}",
                daemon=True,
            ),
        ]
        for thread in self._reader_threads:
            thread.start()

    def _drain_stdout(self) -> None:
        stdout = self._process.stdout
        if stdout is None:
            self._stdout_lines.put(None)
            return
        try:
            while True:
                line = stdout.readline()
                if line == "":
                    return
                self._stdout_lines.put(line)
        finally:
            self._stdout_lines.put(None)

    def _drain_stderr(self) -> None:
        stderr = self._process.stderr
        if stderr is None:
            return
        while True:
            line = stderr.readline()
            if line == "":
                return
            self._stderr_lines.append(line[-65536:])

    def query(self, sql: str, marker: str) -> str:
        stdin = self._process.stdin
        assert stdin is not None
        stdin.write(sql.rstrip().rstrip(";") + "; ")
        stdin.write(f"SELECT {_sql_literal(marker)};\n")
        stdin.flush()
        lines = []
        deadline = time.monotonic() + self._query_timeout_seconds
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise TimeoutError(
                    f"MySQL snapshot marker {marker!r} was not received within "
                    f"{self._query_timeout_seconds:g} seconds: {self._read_stderr()}"
                )
            try:
                line = self._stdout_lines.get(timeout=remaining)
            except queue.Empty as error:
                raise TimeoutError(
                    f"MySQL snapshot marker {marker!r} was not received within "
                    f"{self._query_timeout_seconds:g} seconds: {self._read_stderr()}"
                ) from error
            if line is None:
                self._join_readers(0.2)
                raise RuntimeError(
                    "MySQL snapshot session ended before marker "
                    f"{marker!r}: {self._read_stderr()}"
                )
            value = line.rstrip("\r\n")
            if value == marker:
                return "\n".join(lines)
            lines.append(value)

    def close(self) -> None:
        stdin = self._process.stdin
        if stdin is not None and not stdin.closed:
            try:
                stdin.write("ROLLBACK; QUIT;\n")
                stdin.flush()
            except (BrokenPipeError, OSError):
                pass
            finally:
                try:
                    stdin.close()
                except OSError:
                    pass
        escalated = False
        try:
            return_code = self._process.wait(timeout=self._close_timeout_seconds)
        except subprocess.TimeoutExpired:
            escalated = True
            self._process.terminate()
            try:
                return_code = self._process.wait(timeout=self._close_timeout_seconds)
            except subprocess.TimeoutExpired:
                self._process.kill()
                return_code = self._process.wait(timeout=self._close_timeout_seconds)
        self._join_readers(self._close_timeout_seconds)
        stderr = self._read_stderr()
        if return_code != 0 and not escalated:
            raise RuntimeError(
                f"MySQL snapshot session exited with {return_code}: {stderr}"
            )

    def _join_readers(self, timeout_seconds: float) -> None:
        deadline = time.monotonic() + timeout_seconds
        for thread in self._reader_threads:
            thread.join(timeout=max(0, deadline - time.monotonic()))
        alive = [thread.name for thread in self._reader_threads if thread.is_alive()]
        if alive:
            raise RuntimeError(f"MySQL snapshot reader threads did not exit: {alive}")

    def _read_stderr(self) -> str:
        return "".join(self._stderr_lines).strip()


def _mysql_output(
    request: str | Callable[[_MySQLCLISession], Any],
) -> Any:
    if callable(request):
        session = _MySQLCLISession()
        try:
            result = request(session)
        except BaseException:
            try:
                session.close()
            except Exception as close_error:
                warnings.warn(
                    f"MySQL snapshot rollback/close also failed: {close_error}",
                    RuntimeWarning,
                    stacklevel=2,
                )
            raise
        else:
            session.close()
            return result
    completed = subprocess.run(
        [
            "docker",
            "compose",
            "--profile",
            "agent",
            "exec",
            "-T",
            "mysql",
            "mysql",
            "-N",
            "-B",
            "-uauction",
            "-pauction_dev",
            "live_auction",
        ],
        cwd=DEPLOY_DIR,
        input=request,
        check=True,
        capture_output=True,
        text=True,
    )
    return completed.stdout.rstrip("\r\n")


def _mysql_scalar(query: str) -> str:
    value = _mysql_output(query)
    assert "\n" not in value, value
    return value


def _sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _sql_identifier(value: str) -> str:
    return "`" + value.replace("`", "``") + "`"


def _wait_for_terminal_run(token: str, run_id: str) -> dict[str, Any]:
    deadline = time.monotonic() + 45
    while time.monotonic() < deadline:
        reply = _request_json("GET", f"/api/admin/agent/runs/{run_id}", token=token)
        _assert_ok(reply)
        run = reply.get("run")
        assert isinstance(run, dict), reply
        if run.get("status") in TERMINAL_STATUSES:
            return run
        time.sleep(0.25)
    pytest.fail(f"agent run {run_id} did not reach a terminal status within 45 seconds")


def test_business_snapshot_uses_one_mysql_cli_session(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    table_hex = "auction_lots".encode("utf-8").hex().upper()
    metadata = (
        f"{table_hex}\t1\t6964\t7661726368617228363429\tNO\t"
        "757466386D6234\t757466386D62345F303930305F61695F6369"
    )
    rows = f"T\t{table_hex}\t1\nR\t{table_hex}\tV5:6C6F742D31;"
    calls: list[object] = []
    sessions: list[object] = []

    class FakeSession:
        def __init__(self) -> None:
            self.queries: list[str] = []

        def query(self, sql: str, marker: str) -> str:
            self.queries.append(sql)
            return metadata if len(self.queries) == 1 else rows

    def fake_mysql_output(request: object) -> object:
        calls.append(request)
        if callable(request):
            session = FakeSession()
            sessions.append(session)
            return request(session)
        return metadata if len(calls) == 1 else rows

    monkeypatch.setattr(
        __import__(__name__, fromlist=["_mysql_output"]),
        "_mysql_output",
        fake_mysql_output,
    )

    snapshot = _business_table_snapshot()

    assert snapshot["auction_lots"][1] == ("V5:6C6F742D31;",)
    assert len(calls) == 1, "one snapshot must open exactly one MySQL CLI/session"
    assert len(sessions) == 1
    session = sessions[0]
    assert isinstance(session, FakeSession)
    sql = "\n".join(session.queries)
    assert sql.index("START TRANSACTION WITH CONSISTENT SNAPSHOT") < sql.index(
        "information_schema.COLUMNS"
    )
    for metadata_field in (
        "ORDINAL_POSITION",
        "COLUMN_NAME",
        "COLUMN_TYPE",
        "IS_NULLABLE",
        "CHARACTER_SET_NAME",
        "COLLATION_NAME",
    ):
        assert metadata_field in sql
    assert sql.index("information_schema.COLUMNS") < sql.index("FROM `auction_lots`")
    assert sql.index("FROM `auction_lots`") < sql.index("COMMIT")


def _python_cli_process(source: str) -> subprocess.Popen[str]:
    return subprocess.Popen(
        [sys.executable, "-u", "-c", source],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )


def test_mysql_cli_query_reports_missing_marker_without_blocking_on_stderr() -> None:
    process = _python_cli_process(
        "import sys; sys.stdin.readline(); print('row', flush=True); "
        "print('marker missing diagnostic', file=sys.stderr, flush=True)"
    )
    session = _MySQLCLISession(
        process=process, query_timeout_seconds=1, close_timeout_seconds=0.2
    )
    try:
        with pytest.raises(RuntimeError, match="ended before marker") as captured:
            session.query("SELECT 1", "EXPECTED_MARKER")
        assert "marker missing diagnostic" in str(captured.value)
    finally:
        session.close()
    assert process.poll() is not None
    assert not any(thread.is_alive() for thread in session._reader_threads)


def test_mysql_cli_query_has_hard_deadline_while_stderr_floods() -> None:
    process = _python_cli_process(
        "import sys,time; sys.stderr.write('x' * 200000); sys.stderr.flush(); "
        "sys.stdin.readline(); time.sleep(60)"
    )
    session = _MySQLCLISession(
        process=process, query_timeout_seconds=0.2, close_timeout_seconds=0.1
    )
    started = time.monotonic()
    try:
        with pytest.raises(TimeoutError, match="EXPECTED_MARKER"):
            session.query("SELECT 1", "EXPECTED_MARKER")
    finally:
        session.close()
    assert time.monotonic() - started < 3
    assert process.poll() is not None
    assert not any(thread.is_alive() for thread in session._reader_threads)


def test_mysql_cli_close_escalates_from_terminate_to_kill() -> None:
    class FakePipe:
        closed = False

        def readline(self) -> str:
            return ""

        def write(self, _: str) -> int:
            return 1

        def flush(self) -> None:
            pass

        def close(self) -> None:
            self.closed = True

    class StubbornProcess:
        def __init__(self) -> None:
            self.stdin = FakePipe()
            self.stdout = FakePipe()
            self.stderr = FakePipe()
            self.wait_calls = 0
            self.terminated = False
            self.killed = False

        def wait(self, timeout: float) -> int:
            self.wait_calls += 1
            if self.wait_calls < 3:
                raise subprocess.TimeoutExpired("fake", timeout)
            return 137

        def terminate(self) -> None:
            self.terminated = True

        def kill(self) -> None:
            self.killed = True

    process = StubbornProcess()
    session = _MySQLCLISession(
        process=process, query_timeout_seconds=0.1, close_timeout_seconds=0.01
    )

    session.close()

    assert process.terminated is True
    assert process.killed is True
    assert process.wait_calls == 3
    assert not any(thread.is_alive() for thread in session._reader_threads)


def test_mysql_output_preserves_request_error_when_rollback_close_also_fails(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class BrokenSession:
        def close(self) -> None:
            raise RuntimeError("rollback close failed")

    monkeypatch.setattr(
        __import__(__name__, fromlist=["_MySQLCLISession"]),
        "_MySQLCLISession",
        BrokenSession,
    )

    with pytest.warns(RuntimeWarning, match="rollback close failed"):
        with pytest.raises(AssertionError, match="snapshot callback failed"):
            _mysql_output(
                lambda _: (_ for _ in ()).throw(
                    AssertionError("snapshot callback failed")
                )
            )


def test_business_snapshot_detects_equal_count_update() -> None:
    token, _ = _access_token_and_tenant()
    lot = _ensure_seeded_inventory_lot(token)[0]
    lot_id = str(lot["id"])
    quoted_lot_id = _sql_literal(lot_id)
    original_description_hex = _mysql_scalar(
        f"SELECT HEX(description) FROM auction_lots WHERE id={quoted_lot_id}"
    )
    before_counts = _legacy_business_table_counts()
    before_snapshot = _business_table_snapshot()
    assert AGENT_ORCHESTRATION_TABLES.isdisjoint(before_snapshot)
    assert {"auction_room_states", "shop_products", "shop_skus"} <= set(before_snapshot)
    try:
        affected = _mysql_scalar(
            "UPDATE auction_lots "
            "SET description=CONCAT(description, ' [snapshot-regression]') "
            f"WHERE id={quoted_lot_id}; SELECT ROW_COUNT()"
        )
        assert affected == "1"
        assert _legacy_business_table_counts() == before_counts, (
            "the regression fixture must preserve every old row-count assertion"
        )
        assert _business_table_snapshot() != before_snapshot, (
            "a complete snapshot must detect an equal-count UPDATE"
        )
    finally:
        restored = _mysql_scalar(
            "UPDATE auction_lots "
            "SET description=CONVERT(UNHEX("
            f"{_sql_literal(original_description_hex)}) USING utf8mb4) "
            f"WHERE id={quoted_lot_id}; SELECT ROW_COUNT()"
        )
        assert restored == "1"
    assert _business_table_snapshot() == before_snapshot, (
        "the UPDATE regression fixture must restore the complete business snapshot"
    )


def test_compose_keeps_internal_agent_grpc_ports_off_the_host() -> None:
    services = _compose_configuration()["services"]
    for service_name, target_port in (
        ("auction-backend", 19082),
        ("auction-agent", 19081),
    ):
        service = services[service_name]
        published_targets = {
            int(port["target"])
            for port in service.get("ports", [])
            if "published" in port
        }
        assert target_port not in published_targets, (
            f"{service_name} gRPC {target_port} must stay on the Compose network"
        )
        assert str(target_port) in service.get("expose", []), service


def test_compose_passes_external_model_configuration_to_agent() -> None:
    compose_text = _compose_file().read_text(encoding="utf-8")
    for variable in (
        "AGENT_MODEL_BASE_URL",
        "AGENT_MODEL_NAME",
        "AGENT_MODEL_API_KEY",
        "AGENT_MODEL_TOKEN_LIMIT_FIELD",
        "AGENT_MODEL_TIMEOUT_SECONDS",
        "AGENT_TOKEN_BUDGET",
    ):
        assert f"{variable}:" in compose_text, (
            f"auction-agent must receive {variable} from the Compose environment"
        )


def test_compose_agent_profile_runs_grounded_readonly_planning() -> None:
    configuration = _compose_configuration()
    services = configuration.get("services")
    assert isinstance(services, dict), configuration
    assert {"auction-backend", "pgvector", "auction-agent"} <= set(services), (
        "the Compose agent profile must include the backend, pgvector, and auction-agent"
    )

    token, tenant_id = _access_token_and_tenant()
    inventory = _ensure_seeded_inventory_lot(token)
    owned_inventory_ids = {
        str(lot["id"])
        for lot in inventory
        if str(lot.get("mainAccountId") or lot.get("main_account_id") or "") == tenant_id
    }
    assert owned_inventory_ids, "the seeded merchant must own at least one planning inventory lot"
    before_snapshot = _business_table_snapshot()

    created_reply = _request_json(
        "POST",
        "/api/admin/agent/runs",
        token=token,
        body={"goal": "为当前库存生成一份只读、可人工确认的直播竞拍排期方案"},
        idempotency_key=str(uuid.uuid4()),
    )
    _assert_ok(created_reply)
    created_run = created_reply.get("run")
    assert isinstance(created_run, dict), created_reply
    run_id = str(created_run.get("runId", "")).strip()
    assert run_id, created_reply

    completed_run = _wait_for_terminal_run(token, run_id)
    assert completed_run.get("status") == "COMPLETED", completed_run
    assert _mysql_scalar(
        f"SELECT status FROM agent_runs WHERE id={_sql_literal(run_id)}"
    ) == "COMPLETED"
    assert int(
        _mysql_scalar(
            "SELECT COUNT(*) FROM agent_run_events "
            f"WHERE run_id={_sql_literal(run_id)} AND event_type LIKE 'tool.%'"
        )
    ) > 0
    proposal = completed_run.get("proposal")
    assert isinstance(proposal, dict), completed_run
    proposal_lots = proposal.get("lots")
    assert isinstance(proposal_lots, list) and proposal_lots, proposal
    proposal_lot_ids = {str(lot.get("lot_id", "")) for lot in proposal_lots}
    assert "" not in proposal_lot_ids
    assert proposal_lot_ids <= owned_inventory_ids, (
        "every proposed lot must come from the JWT tenant's Go-owned inventory"
    )

    after_snapshot = _business_table_snapshot()
    changed_tables = sorted(
        table
        for table in set(before_snapshot) | set(after_snapshot)
        if before_snapshot.get(table) != after_snapshot.get(table)
    )
    assert after_snapshot == before_snapshot, (
        "read-only planning may persist agent runtime records, but must not mutate auction, "
        f"bid, event, order, payment, or deposit business tables; changed={changed_tables}"
    )
