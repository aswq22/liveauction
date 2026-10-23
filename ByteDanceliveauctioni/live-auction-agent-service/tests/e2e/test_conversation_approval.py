"""Cross-service acceptance for conversational draft approval.

The tests create a dedicated merchant/room for each case and remove only rows
owned by that marker.  They never use the shared ``main`` account.
"""

from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
import base64
import json
import os
from pathlib import Path
import subprocess
from threading import Barrier
import time
from typing import Any
from urllib.error import HTTPError
from urllib.request import Request, urlopen
import uuid

import pytest


ROOT = Path(__file__).resolve().parents[3]
DEPLOY = ROOT / "live-auction-bid-backend" / "deploy"
BASE_URL = os.getenv("AGENT_E2E_BASE_URL", os.getenv("AUCTION_E2E_BASE_URL", "http://127.0.0.1:18080")).rstrip("/")
TERMINAL = {"COMPLETED", "FAILED", "CANCELLED"}
PNG = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")


def _sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def _mysql(sql: str) -> str:
    completed = subprocess.run(
        ["docker", "compose", "--profile", "agent", "exec", "-T", "mysql", "mysql", "-N", "-B", "-uauction", "-pauction_dev", "live_auction"],
        cwd=DEPLOY,
        input=sql,
        text=True,
        check=True,
        capture_output=True,
    )
    return completed.stdout.strip()


def _request(method: str, path: str, *, token: str | None = None, body: dict[str, Any] | None = None, key: str | None = None, headers: dict[str, str] | None = None) -> tuple[int, dict[str, Any]]:
    raw = None if body is None else json.dumps(body, ensure_ascii=False).encode()
    request_headers = {"Accept": "application/json", **(headers or {})}
    if raw is not None:
        request_headers["Content-Type"] = "application/json"
    if token:
        request_headers["Authorization"] = f"Bearer {token}"
    if key:
        request_headers["Idempotency-Key"] = key
    request = Request(BASE_URL + path, data=raw, headers=request_headers, method=method)
    try:
        with urlopen(request, timeout=15) as response:
            return response.status, json.loads(response.read())
    except HTTPError as error:
        return error.code, json.loads(error.read())


def _ok(reply: tuple[int, dict[str, Any]]) -> dict[str, Any]:
    status, body = reply
    assert status == 200, body
    assert int(body.get("result", {}).get("code", 0)) == 0, body
    return body


def _multipart_upload(token: str, room_id: str) -> str:
    boundary = "agent-e2e-" + uuid.uuid4().hex
    chunks = [
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"roomId\"\r\n\r\n{room_id}\r\n".encode(),
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"bizType\"\r\n\r\nagent_lot_image\r\n".encode(),
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"agent-e2e.png\"\r\nContent-Type: image/png\r\n\r\n".encode(),
        PNG,
        f"\r\n--{boundary}--\r\n".encode(),
    ]
    request = Request(
        BASE_URL + "/api/uploads/images",
        data=b"".join(chunks),
        headers={"Authorization": f"Bearer {token}", "Content-Type": f"multipart/form-data; boundary={boundary}"},
        method="POST",
    )
    with urlopen(request, timeout=15) as response:
        body = json.loads(response.read())
    _ok((200, body))
    asset_id = str(body["data"]["asset"]["id"])
    assert asset_id
    return asset_id


@dataclass
class Merchant:
    token: str = field(repr=False)
    tenant_id: str
    username: str
    room_id: str

    def create_lot(self, title: str, *, queue: bool = False) -> str:
        body = _ok(_request("POST", "/api/lots/drafts", token=self.token, body={"title": title, "description": "agent approval e2e"}))
        lot_id = str(body["lot"]["id"])
        self.room_id = str(body["lot"].get("roomId") or body["lot"].get("room_id"))
        _ok(_request("PATCH", f"/api/lots/{lot_id}/draft", token=self.token, body={
            "title": title,
            "description": "agent approval e2e",
            "image_url": "https://example.invalid/agent-e2e.png",
            "rule": {
                "start_price": {"amount": 10000, "currency": "CNY"},
                "min_increment": {"amount": 1000, "currency": "CNY"},
                "duration_seconds": 300,
                "anti_snipe_window_seconds": 10,
                "anti_snipe_extend_seconds": 15,
                "max_extend_count": 5,
            },
        }))
        if queue:
            _ok(_request("POST", f"/api/lots/{lot_id}/queue", token=self.token, body={"lotId": lot_id}))
        return lot_id

    def session(self) -> dict[str, Any]:
        return _ok(_request("POST", "/api/admin/agent/sessions", token=self.token, body={"roomId": self.room_id}))["session"]

    def recover(self, session_id: str) -> dict[str, Any]:
        last: tuple[int, dict[str, Any]] | None = None
        for _ in range(5):
            last = _request("GET", f"/api/admin/agent/sessions/{session_id}", token=self.token)
            if int(last[1].get("result", {}).get("code", 0)) == 0:
                return _ok(last)
            time.sleep(0.1)
        assert last is not None
        return _ok(last)

    def message(self, session: dict[str, Any], content: str, run_type: str) -> dict[str, Any]:
        body = _ok(_request("POST", f"/api/admin/agent/sessions/{session['id']}/messages", token=self.token, key=str(uuid.uuid4()), body={"expectedVersion": session["version"], "content": content, "runType": run_type}))
        run_id = str(body["run"]["runId"])
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            aggregate = self.recover(session["id"])
            status = _mysql(f"SELECT status FROM agent_runs WHERE id={_sql_literal(run_id)}")
            if status in TERMINAL:
                # Recover once more to force durable merge if projection just finished.
                return self.recover(session["id"])
            time.sleep(0.2)
        raise AssertionError(f"run {run_id} did not complete")


@pytest.fixture
def merchant() -> Merchant:
    suffix = uuid.uuid4().hex[:12]
    username = "agent_e2e_" + suffix
    registered = _ok(_request("POST", "/api/merchants/register", body={"username": username, "password": "merchantpass123", "nickname": "Agent E2E"}))
    token = str(registered["tokens"]["accessToken"])
    tenant_id = str(registered["user"]["id"])
    value = Merchant(token=token, tenant_id=tenant_id, username=username, room_id="")
    value.create_lot("fixture-room-anchor")
    try:
        yield value
    finally:
        tenant = _sql_literal(tenant_id)
        _mysql(
            "SET FOREIGN_KEY_CHECKS=0; "
            f"DELETE FROM agent_approval_outbox WHERE session_id IN (SELECT id FROM agent_sessions WHERE tenant_id={tenant}); "
            f"DELETE FROM agent_approval_audits WHERE tenant_id={tenant}; "
            f"DELETE FROM agent_proposal_executions WHERE tenant_id={tenant}; "
            f"DELETE FROM agent_approval_snapshots WHERE session_id IN (SELECT id FROM agent_sessions WHERE tenant_id={tenant}); "
            f"DELETE FROM agent_candidate_assets WHERE tenant_id={tenant}; "
            f"DELETE FROM agent_draft_candidates WHERE session_id IN (SELECT id FROM agent_sessions WHERE tenant_id={tenant}); "
            f"DELETE FROM agent_session_messages WHERE session_id IN (SELECT id FROM agent_sessions WHERE tenant_id={tenant}); "
            f"DELETE FROM agent_run_events WHERE run_id IN (SELECT id FROM agent_runs WHERE tenant_id={tenant}); "
            f"DELETE FROM agent_runs WHERE tenant_id={tenant}; DELETE FROM agent_sessions WHERE tenant_id={tenant}; "
            f"DELETE FROM asset_files WHERE main_account_id={tenant}; DELETE FROM auction_lot_stats WHERE main_account_id={tenant}; "
            f"DELETE FROM auction_events WHERE main_account_id={tenant}; DELETE FROM auction_lots WHERE main_account_id={tenant}; "
            f"DELETE FROM auction_room_states WHERE main_account_id={tenant}; DELETE FROM auction_rooms WHERE main_account_id={tenant}; "
            f"DELETE FROM auction_user_permissions WHERE user_id={tenant}; DELETE FROM auction_user_roles WHERE user_id={tenant}; "
            f"DELETE FROM auction_user_sessions WHERE user_id={tenant}; DELETE FROM auction_users WHERE id={tenant}; SET FOREIGN_KEY_CHECKS=1;"
        )


def _normalize_candidate(item: dict[str, Any]) -> dict[str, Any]:
    candidate = item.get("candidate") or item.get("Candidate")
    assert isinstance(candidate, dict), item
    raw_validation = item.get("validationJSON", item.get("ValidationJSON", "{}"))
    validation = json.loads(raw_validation) if isinstance(raw_validation, str) and raw_validation.strip() else {}
    return {
        "candidate": candidate,
        "version": int(item.get("version", item.get("Version", 0))),
        "selectedByDefault": bool(item.get("selectedByDefault", item.get("SelectedByDefault", False))),
        "validation": validation,
    }


def _candidates(aggregate: dict[str, Any]) -> list[dict[str, Any]]:
    return [_normalize_candidate(item) for item in aggregate["candidates"]]


def _candidate(aggregate: dict[str, Any], source: str = "NEW") -> dict[str, Any]:
    matches = [item for item in _candidates(aggregate) if item["candidate"]["sourceType"] == source]
    assert matches, aggregate
    return matches[0]


def _ready_ocean_heart(merchant: Merchant) -> tuple[dict[str, Any], dict[str, Any], str]:
    session = merchant.session()
    aggregate = merchant.message(session, "我要拍卖名为海洋之心的宝石，价格10000，时长50分钟，封顶价格100000", "LOT_DRAFT_ASSIST")
    candidate = _candidate(aggregate)
    draft = candidate["candidate"]["draft"]
    assert draft["title"] == "海洋之心"
    assert draft["startPriceAmount"] == 1_000_000
    assert draft["durationSeconds"] == 3_000
    assert draft["capPriceAmount"] == 10_000_000
    assert not draft["mainImageAssetId"] and not draft["mainImageUrl"]
    assert draft["minIncrementAmount"] == 10_000
    assert draft["stock"] == 1
    assert (draft["antiSnipeWindowSeconds"], draft["antiSnipeExtendSeconds"], draft["maxExtendCount"]) == (30, 15, 5)
    assert int(_mysql(f"SELECT COUNT(*) FROM auction_lots WHERE main_account_id={_sql_literal(merchant.tenant_id)} AND title='海洋之心'")) == 0
    asset_id = _multipart_upload(merchant.token, merchant.room_id)
    aggregate = _ok(_request("POST", f"/api/admin/agent/sessions/{session['id']}/candidates/{candidate['candidate']['id']}/images", token=merchant.token, body={"expectedSessionVersion": aggregate["session"]["version"], "expectedCandidateVersion": candidate["version"], "assetId": asset_id}))
    return aggregate, _candidate(aggregate), asset_id


def _snapshot(merchant: Merchant, aggregate: dict[str, Any], ordered: list[str]) -> dict[str, Any]:
    return _ok(_request("POST", f"/api/admin/agent/sessions/{aggregate['session']['id']}/review-snapshots", token=merchant.token, body={"expectedSessionVersion": aggregate["session"]["version"], "selectedCandidateIds": ordered, "orderedCandidateIds": ordered}))


def _apply(merchant: Merchant, session_id: str, snapshot: dict[str, Any], key: str) -> tuple[int, dict[str, Any]]:
    ids = [candidate["candidateId"] for candidate in snapshot["candidates"]]
    return _request("POST", f"/api/admin/agent/sessions/{session_id}/apply", token=merchant.token, key=key, body={"snapshotVersion": snapshot["snapshotVersion"], "contentHash": snapshot["contentHash"], "selectedCandidateIds": ids, "orderedCandidateIds": ids})


def _seed_new_candidate_session(merchant: Merchant, titles: list[str]) -> tuple[dict[str, Any], list[str], list[str]]:
    session = merchant.session()
    sources = json.dumps({field: "USER" for field in (
        "antiSnipeExtendSeconds", "antiSnipeWindowSeconds", "capPriceAmount", "category",
        "depositAmount", "description", "durationSeconds", "mainImageAssetId", "maxExtendCount",
        "minIncrementAmount", "startPriceAmount", "stock", "tags", "title",
    )}, separators=(",", ":"))
    candidate_ids: list[str] = []
    asset_ids: list[str] = []
    statements: list[str] = []
    for position, title in enumerate(titles, start=1):
        candidate_id = "candidate-e2e-" + uuid.uuid4().hex
        asset_id = _multipart_upload(merchant.token, merchant.room_id)
        candidate_ids.append(candidate_id)
        asset_ids.append(asset_id)
        candidate_json = json.dumps({
            "id": candidate_id, "sourceType": "NEW", "startPriceSpecified": True,
            "draft": {"title": title, "startPriceAmount": 10_000, "durationSeconds": 600,
                "mainImageAssetId": asset_id, "mainImageUrl": "", "minIncrementAmount": 1_000,
                "capPriceAmount": None, "category": "e2e", "description": "isolated fixture",
                "tags": ["e2e"], "stock": 1, "antiSnipeWindowSeconds": 10,
                "antiSnipeExtendSeconds": 15, "maxExtendCount": 5, "depositAmount": 0},
        }, separators=(",", ":"))
        statements.append(
            "INSERT INTO agent_draft_candidates "
            "(id,session_id,source_type,existing_lot_id,position,selected_by_default,draft_json,field_sources_json,validation_json,version,created_at_unix_ms,updated_at_unix_ms,created_at,updated_at) VALUES "
            f"({_sql_literal(candidate_id)},{_sql_literal(session['id'])},'NEW','',{position},1,{_sql_literal(candidate_json)},{_sql_literal(sources)},'{{}}',1,1,1,NOW(3),NOW(3))"
        )
        statements.append(
            "INSERT INTO agent_candidate_assets (candidate_id,session_id,asset_id,tenant_id,operator_id,created_at_unix_ms,updated_at_unix_ms,created_at,updated_at) VALUES "
            f"({_sql_literal(candidate_id)},{_sql_literal(session['id'])},{_sql_literal(asset_id)},{_sql_literal(merchant.tenant_id)},{_sql_literal(merchant.tenant_id)},1,1,NOW(3),NOW(3))"
        )
    _mysql("; ".join(statements) + f"; UPDATE agent_sessions SET version=version+1,status='READY_FOR_REVIEW' WHERE id={_sql_literal(session['id'])};")
    return merchant.recover(session["id"]), candidate_ids, asset_ids


def test_ocean_heart_only_asks_for_image_and_is_created_only_by_apply(merchant: Merchant) -> None:
    aggregate, candidate, _ = _ready_ocean_heart(merchant)
    session_id = aggregate["session"]["id"]
    before = _mysql(f"SELECT COUNT(*) FROM auction_lots WHERE main_account_id={_sql_literal(merchant.tenant_id)}")
    snapshot = _snapshot(merchant, aggregate, [candidate["candidate"]["id"]])
    assert _mysql(f"SELECT COUNT(*) FROM auction_lots WHERE main_account_id={_sql_literal(merchant.tenant_id)}") == before
    applied = _ok(_apply(merchant, session_id, snapshot, str(uuid.uuid4())))
    assert len(applied["createdLotIds"]) == 1
    # A browser refresh recovers the durable execution directly; APPLIED does
    # not depend on the READY-only active snapshot endpoint.
    recovered = merchant.recover(session_id)
    execution = recovered["appliedExecution"]
    assert execution["executionId"] == applied["executionId"]
    assert execution["snapshotVersion"] == snapshot["snapshotVersion"]
    assert execution["createdLotIds"] == applied["createdLotIds"]
    assert execution["queuedLots"][0]["sourceType"] == "NEW"
    assert execution["queuedLots"][0]["candidateId"] == candidate["candidate"]["id"]
    row = _mysql(f"SELECT status, queue_position FROM auction_lots WHERE id={_sql_literal(applied['createdLotIds'][0])}").split("\t")
    assert row[0] != "2", "approval queues but never starts the lot"
    assert int(row[1]) > 0


@pytest.mark.parametrize("message", ["缺价格，时长10分钟", "缺时长，价格100"])
def test_missing_required_input_blocks_snapshot(merchant: Merchant, message: str) -> None:
    aggregate = merchant.message(merchant.session(), message, "LOT_DRAFT_ASSIST")
    candidate = _candidate(aggregate)
    status, reply = _request("POST", f"/api/admin/agent/sessions/{aggregate['session']['id']}/review-snapshots", token=merchant.token, body={"expectedSessionVersion": aggregate["session"]["version"], "selectedCandidateIds": [candidate["candidate"]["id"]], "orderedCandidateIds": [candidate["candidate"]["id"]]})
    assert status in {200, 400}
    assert int(reply["result"]["code"]) != 0
    assert _mysql(f"SELECT COUNT(*) FROM agent_approval_snapshots WHERE session_id={_sql_literal(aggregate['session']['id'])}") == "0"


def test_invalid_asset_rolls_back_entire_mixed_batch(merchant: Merchant) -> None:
    aggregate, ordered, asset_ids = _seed_new_candidate_session(merchant, ["valid member", "invalid member"])
    snapshot = _snapshot(merchant, aggregate, ordered)
    tenant = _sql_literal(merchant.tenant_id)
    session = _sql_literal(aggregate["session"]["id"])
    before_lots = _mysql(f"SELECT COUNT(*), COALESCE(SUM(queue_position),0) FROM auction_lots WHERE main_account_id={tenant}")
    before_assets = _mysql(f"SELECT COUNT(*), COALESCE(SUM(attached_at_unix_ms),0) FROM asset_files WHERE main_account_id={tenant} AND attached_lot_id<>''")
    _mysql(f"UPDATE asset_files SET status='deleted' WHERE id={_sql_literal(asset_ids[1])}")
    status, reply = _apply(merchant, aggregate["session"]["id"], snapshot, str(uuid.uuid4()))
    assert status in {200, 409}
    assert int(reply["result"]["code"]) != 0
    assert _mysql(f"SELECT COUNT(*), COALESCE(SUM(queue_position),0) FROM auction_lots WHERE main_account_id={tenant}") == before_lots
    assert _mysql(f"SELECT COUNT(*), COALESCE(SUM(attached_at_unix_ms),0) FROM asset_files WHERE main_account_id={tenant} AND attached_lot_id<>''") == before_assets
    assert _mysql(f"SELECT COUNT(*) FROM agent_proposal_executions WHERE session_id={session}") == "0"
    assert _mysql(f"SELECT COUNT(*) FROM agent_approval_audits WHERE session_id={session}") == "0"
    assert _mysql(f"SELECT COUNT(*) FROM agent_approval_outbox WHERE session_id={session}") == "0"


def test_existing_selection_appends_in_requested_order_and_replay_is_stable(merchant: Merchant) -> None:
    tail = merchant.create_lot("existing-tail", queue=True)
    first = merchant.create_lot("existing-first")
    omitted = merchant.create_lot("existing-omitted")
    second = merchant.create_lot("existing-second")
    session = merchant.session()
    aggregate = merchant.message(session, "为当前库存生成竞拍方案", "SESSION_PLAN")
    candidates = [item for item in _candidates(aggregate) if item["candidate"]["sourceType"] == "EXISTING"]
    by_lot = {item["candidate"]["existingLotId"]: item for item in candidates}
    assert {first, omitted, second}.issubset(by_lot), "SESSION_PLAN must materialize candidates from real owned inventory"
    ordered = [by_lot[second]["candidate"]["id"], by_lot[first]["candidate"]["id"]]
    snapshot = _snapshot(merchant, aggregate, ordered)
    key = str(uuid.uuid4())
    first_reply = _ok(_apply(merchant, aggregate["session"]["id"], snapshot, key))
    replay = _ok(_apply(merchant, aggregate["session"]["id"], snapshot, key))
    assert replay["executionId"] == first_reply["executionId"]
    assert replay["queuedLots"] == first_reply["queuedLots"]
    positions = {lot: int(pos) for lot, pos in (line.split("\t") for line in _mysql(f"SELECT id, queue_position FROM auction_lots WHERE id IN ({','.join(map(_sql_literal, [tail, first, omitted, second]))})").splitlines())}
    assert positions[tail] > 0
    assert positions[second] == positions[tail] + 1
    assert positions[first] == positions[second] + 1
    assert positions[omitted] == 0


def test_session_plan_default_selection_contains_only_appendable_inventory(merchant: Merchant) -> None:
    eligible_draft = merchant.create_lot("eligible-draft")
    eligible_ready = merchant.create_lot("eligible-ready")
    queued = merchant.create_lot("excluded-queued", queue=True)
    live = merchant.create_lot("excluded-live")
    settled = merchant.create_lot("excluded-settled")
    _mysql(
        f"UPDATE auction_lots SET status=5,queue_status=1,queue_position=0 WHERE id={_sql_literal(eligible_ready)}; "
        f"UPDATE auction_lots SET status=2,queue_status=1,queue_position=0 WHERE id={_sql_literal(live)}; "
        f"UPDATE auction_lots SET status=3,queue_status=1,queue_position=0 WHERE id={_sql_literal(settled)}"
    )
    aggregate = merchant.message(merchant.session(), "为当前库存生成竞拍方案", "SESSION_PLAN")
    candidates = [item for item in _candidates(aggregate) if item["candidate"]["sourceType"] == "EXISTING"]
    exposed = {item["candidate"]["existingLotId"] for item in candidates}
    assert {eligible_draft, eligible_ready}.issubset(exposed)
    assert exposed.isdisjoint({queued, live, settled})
    assert all(item["selectedByDefault"] is True for item in candidates)
    snapshot = _snapshot(merchant, aggregate, [item["candidate"]["id"] for item in candidates])
    applied = _ok(_apply(merchant, aggregate["session"]["id"], snapshot, str(uuid.uuid4())))
    assert len(applied["queuedLots"]) == len(candidates)


def test_two_sessions_allocate_unique_contiguous_positions(merchant: Merchant) -> None:
    candidates: list[tuple[dict[str, Any], dict[str, Any]]] = []
    for title in ("concurrent-a", "concurrent-b"):
        aggregate, candidate_ids, _ = _seed_new_candidate_session(merchant, [title + "-1", title + "-2"])
        candidates.append((aggregate, _snapshot(merchant, aggregate, candidate_ids)))
    with ThreadPoolExecutor(max_workers=2) as pool:
        replies = list(pool.map(lambda pair: _ok(_apply(merchant, pair[0]["session"]["id"], pair[1], str(uuid.uuid4()))), candidates))
    batches = [[lot["queuePosition"] for lot in reply["queuedLots"]] for reply in replies]
    assert all(len(batch) == 2 and batch[1] == batch[0] + 1 for batch in batches)
    positions = sorted(position for batch in batches for position in batch)
    assert len(set(positions)) == 4
    assert positions == list(range(positions[0], positions[0] + 4))


def test_python_turns_leave_business_tables_unchanged_until_go_apply(merchant: Merchant) -> None:
    tenant = _sql_literal(merchant.tenant_id)
    before = _mysql(f"SELECT COUNT(*), COALESCE(SUM(queue_position),0) FROM auction_lots WHERE main_account_id={tenant}")
    aggregate, candidate, _ = _ready_ocean_heart(merchant)
    assert _mysql(f"SELECT COUNT(*), COALESCE(SUM(queue_position),0) FROM auction_lots WHERE main_account_id={tenant}") == before
    assets_before = _mysql(f"SELECT COUNT(*), SUM(attached_lot_id<>''), SUM(status='attached') FROM asset_files WHERE main_account_id={tenant}")
    room_before = _mysql(f"SELECT CONCAT('[',active_lot_id,']'), active_lot_version, next_queue_position FROM auction_room_states WHERE main_account_id={tenant}")
    events_before = _mysql(f"SELECT COUNT(*) FROM auction_events WHERE main_account_id={tenant}")
    snapshot = _snapshot(merchant, aggregate, [candidate["candidate"]["id"]])
    assert _mysql(f"SELECT COUNT(*), COALESCE(SUM(queue_position),0) FROM auction_lots WHERE main_account_id={tenant}") == before
    assert _mysql(f"SELECT COUNT(*), SUM(attached_lot_id<>''), SUM(status='attached') FROM asset_files WHERE main_account_id={tenant}") == assets_before
    assert _mysql(f"SELECT CONCAT('[',active_lot_id,']'), active_lot_version, next_queue_position FROM auction_room_states WHERE main_account_id={tenant}") == room_before
    assert _mysql(f"SELECT COUNT(*) FROM auction_events WHERE main_account_id={tenant}") == events_before
    applied = _ok(_apply(merchant, aggregate["session"]["id"], snapshot, str(uuid.uuid4())))
    after_count, after_sum = map(int, _mysql(f"SELECT COUNT(*), COALESCE(SUM(queue_position),0) FROM auction_lots WHERE main_account_id={tenant}").split("\t"))
    before_count, before_sum = map(int, before.split("\t"))
    assert after_count == before_count + 1
    assert after_sum == before_sum + int(applied["queuedLots"][0]["queuePosition"])
    asset_count, attached_lots, attached_statuses = map(int, _mysql(f"SELECT COUNT(*), SUM(attached_lot_id<>''), SUM(status='attached') FROM asset_files WHERE main_account_id={tenant}").split("\t"))
    before_asset_count, before_attached_lots, before_attached_statuses = map(int, assets_before.split("\t"))
    assert (asset_count, attached_lots, attached_statuses) == (before_asset_count, before_attached_lots + 1, before_attached_statuses + 1)
    active_lot, active_version, next_position = _mysql(f"SELECT CONCAT('[',active_lot_id,']'), active_lot_version, next_queue_position FROM auction_room_states WHERE main_account_id={tenant}").split("\t")
    before_active_lot, before_active_version, before_next_position = room_before.split("\t")
    assert (active_lot, active_version) == (before_active_lot, before_active_version)
    assert int(next_position) == int(before_next_position) + 1
    assert int(_mysql(f"SELECT COUNT(*) FROM auction_events WHERE main_account_id={tenant}")) == int(events_before) + 1
    assert _mysql(f"SELECT COUNT(*) FROM auction_events WHERE main_account_id={tenant} AND lot_id={_sql_literal(applied['createdLotIds'][0])}") == "1"


def test_product_snapshot_repository_serializes_two_concurrent_requests(merchant: Merchant) -> None:
    """Two HTTP workers enter the real Go Repository/CreateReviewSnapshot path."""
    aggregate, candidate_ids, _ = _seed_new_candidate_session(merchant, ["snapshot-race"])
    body = {"expectedSessionVersion": aggregate["session"]["version"], "selectedCandidateIds": candidate_ids, "orderedCandidateIds": candidate_ids}
    path = f"/api/admin/agent/sessions/{aggregate['session']['id']}/review-snapshots"
    barrier = Barrier(2)

    def create_snapshot() -> tuple[int, dict[str, Any]]:
        barrier.wait(timeout=5)
        return _request("POST", path, token=merchant.token, body=body)

    with ThreadPoolExecutor(max_workers=2) as pool:
        replies = list(pool.map(lambda _: create_snapshot(), range(2)))
    successes = [reply for status, reply in replies if status == 200 and int(reply.get("result", {}).get("code", 0)) == 0]
    conflicts = [reply for _, reply in replies if int(reply.get("result", {}).get("code", 0)) != 0]
    assert len(successes) == 1 and len(conflicts) == 1
    assert successes[0]["snapshotVersion"] == 1
    session = _sql_literal(aggregate["session"]["id"])
    assert _mysql(f"SELECT active_snapshot_version FROM agent_sessions WHERE id={session}") == "1"
    assert _mysql(f"SELECT COUNT(*) FROM agent_approval_snapshots WHERE session_id={session}") == "1"
