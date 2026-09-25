# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Schedule and backfill client methods, against a real local HTTP server."""

from __future__ import annotations

import datetime as dt
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from nexus import Conflict, NexusAPIError, NexusClient, NotFound

SCHEDULE = {
    "name": "nightly", "spec_name": "etl", "cron": "0 2 * * *", "timezone": "Europe/London",
    "params": {"source": "s3://a"}, "priority": 3, "overlap_policy": "skip", "catchup_limit": 2,
    "enabled": False, "spec_version": 4, "next_fire_at": None,
}


def _backfill(state: str) -> dict:
    return {"id": "bf-1", "state": state, "from": "2026-09-01T00:00:00Z", "to": "2026-09-08T00:00:00Z",
            "runs_created": 0, "max_runs": 1000, "max_active": 1}


class _Server:
    def __init__(self) -> None:
        self.requests: list[dict] = []
        self.routes: dict[tuple[str, str], tuple[int, object]] = {}

    def reply(self, method: str, path: str, status: int, body: object = None) -> None:
        self.routes[(method, "/v1/namespaces/prod" + path)] = (status, body)

    def sent(self, method: str, path: str) -> list[dict]:
        return [r for r in self.requests if r["method"] == method and r["path"] == "/v1/namespaces/prod" + path]


@pytest.fixture
def server():
    srv = _Server()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _handle(self) -> None:
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else b""
            path, _, query = self.path.partition("?")
            srv.requests.append({"method": self.command, "path": path, "query": query, "body": body})
            status, payload = srv.routes.get((self.command, path), (404, {"message": "no route"}))
            raw = b"" if payload is None else json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        do_GET = do_POST = do_PUT = do_DELETE = _handle

    httpd = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    srv.endpoint = f"http://127.0.0.1:{httpd.server_address[1]}"  # type: ignore[attr-defined]
    yield srv
    httpd.shutdown()
    httpd.server_close()


def _client(server) -> NexusClient:
    return NexusClient(server.endpoint, api_key="k", namespace="prod", max_retries=0)


# ── schedules ───────────────────────────────────────────────────────────────


def test_list_and_get_schedules(server) -> None:
    server.reply("GET", "/schedules", 200, {"schedules": [SCHEDULE]})
    server.reply("GET", "/schedules/nightly", 200, {**SCHEDULE, "next_fires": ["2026-09-26T02:00:00+01:00"]})
    c = _client(server)
    assert [s["name"] for s in c.list_schedules(limit=5)] == ["nightly"]
    assert server.requests[-1]["query"] == "limit=5"
    got = c.get_schedule("nightly", preview_count=1)
    assert got["next_fires"] == ["2026-09-26T02:00:00+01:00"]
    assert server.requests[-1]["query"] == "count=1"


def test_create_schedule_sends_the_gateway_body(server) -> None:
    server.reply("POST", "/schedules", 201, SCHEDULE)
    _client(server).create_schedule(
        "nightly", workflow="etl", cron="0 2 * * *", timezone="Europe/London",
        params={"source": "s3://a"}, spec_version=4, overlap_policy="queue_one", catchup_limit=2,
    )
    assert json.loads(server.requests[-1]["body"]) == {
        "name": "nightly", "spec_name": "etl", "cron": "0 2 * * *", "timezone": "Europe/London",
        "params": {"source": "s3://a"}, "spec_version": 4, "overlap_policy": "queue_one",
        "catchup_limit": 2, "enabled": True,
    }


@pytest.mark.parametrize(
    "kwargs, match",
    [
        ({"workflow": "", "cron": "* * * * *"}, "required"),
        ({"workflow": "w", "cron": "* * * * *", "overlap_policy": "sometimes"}, "overlap_policy"),
        ({"workflow": "w", "cron": "* * * * *", "catchup_limit": -1}, "catchup_limit"),
    ],
)
def test_create_schedule_rejects_bad_input_locally(server, kwargs, match) -> None:
    with pytest.raises(ValueError, match=match):
        _client(server).create_schedule("n", **kwargs)
    assert server.requests == []


def test_create_schedule_errors_are_typed(server) -> None:
    server.reply("POST", "/schedules", 409, {"error": "schedule_exists", "message": "a schedule with this name already exists"})
    with pytest.raises(Conflict, match="already exists"):
        _client(server).create_schedule("n", workflow="w", cron="* * * * *")
    server.reply("POST", "/schedules", 400, {"error": "unknown_spec_name", "message": 'spec_name "w" does not name a workflow'})
    with pytest.raises(NexusAPIError, match="does not name a workflow") as e:
        _client(server).create_schedule("n", workflow="w", cron="* * * * *")
    assert e.value.status == 400


def test_update_schedule_keeps_what_it_is_not_told_to_change(server) -> None:
    server.reply("GET", "/schedules/nightly", 200, SCHEDULE)
    server.reply("PUT", "/schedules/nightly", 200, SCHEDULE)
    _client(server).update_schedule("nightly", cron="0 3 * * *")
    body = json.loads(server.sent("PUT", "/schedules/nightly")[0]["body"])
    assert body == {
        "name": "nightly", "spec_name": "etl", "cron": "0 3 * * *", "timezone": "Europe/London",
        "params": {"source": "s3://a"}, "priority": 3, "overlap_policy": "skip", "catchup_limit": 2,
        "spec_version": 4,
        # Preserved, and always sent: the gateway requires it on update.
        "enabled": False,
    }


def test_update_schedule_can_unpin_and_replace_params(server) -> None:
    server.reply("GET", "/schedules/nightly", 200, SCHEDULE)
    server.reply("PUT", "/schedules/nightly", 200, SCHEDULE)
    _client(server).update_schedule("nightly", spec_version=None, params={"source": "s3://b"})
    body = json.loads(server.sent("PUT", "/schedules/nightly")[0]["body"])
    assert "spec_version" not in body
    assert body["params"] == {"source": "s3://b"}


def test_update_of_a_missing_schedule_is_not_found(server) -> None:
    with pytest.raises(NotFound):
        _client(server).update_schedule("ghost", cron="* * * * *")
    assert server.sent("PUT", "/schedules/ghost") == []


def test_delete_enable_disable(server) -> None:
    server.reply("DELETE", "/schedules/nightly", 204)
    server.reply("POST", "/schedules/nightly/enable", 200, {**SCHEDULE, "enabled": True})
    server.reply("POST", "/schedules/nightly/disable", 200, SCHEDULE)
    c = _client(server)
    assert c.delete_schedule("nightly") is None
    assert c.enable_schedule("nightly")["enabled"] is True
    assert c.disable_schedule("nightly")["enabled"] is False
    assert [r["method"] for r in server.requests] == ["DELETE", "POST", "POST"]


def test_preview_returns_invalid_as_an_answer(server) -> None:
    server.reply("GET", "/schedules/preview", 200, {"cron": "61 * * * *", "valid": False, "error": "minute 61"})
    out = _client(server).preview_schedule("61 * * * *", timezone="Europe/London", count=3)
    assert out["valid"] is False and "61" in out["error"]
    q = server.requests[-1]["query"]
    assert "cron=61+%2A+%2A+%2A+%2A" in q and "timezone=Europe%2FLondon" in q and "count=3" in q


# ── backfills ───────────────────────────────────────────────────────────────


def test_start_backfill_serializes_aware_instants(server) -> None:
    server.reply("POST", "/schedules/nightly/backfills", 201, _backfill("RUNNING"))
    start = dt.datetime(2026, 9, 1, tzinfo=dt.timezone.utc)
    out = _client(server).start_backfill("nightly", start=start, end="2026-09-08T00:00:00Z", max_active=2)
    assert out["id"] == "bf-1"
    assert json.loads(server.requests[-1]["body"]) == {
        "from": "2026-09-01T00:00:00+00:00", "to": "2026-09-08T00:00:00+00:00", "max_active": 2,
    }


@pytest.mark.parametrize(
    "kwargs, match",
    [
        ({"start": dt.datetime(2026, 9, 1), "end": "2026-09-08T00:00:00Z"}, "no timezone"),
        ({"start": "last tuesday", "end": "2026-09-08T00:00:00Z"}, "RFC 3339"),
        ({"start": "2026-09-08T00:00:00Z", "end": "2026-09-01T00:00:00Z"}, "before"),
        ({"start": "2026-09-01T00:00:00Z", "end": "2026-09-08T00:00:00Z", "max_active": 51}, "max_active"),
        ({"start": "2026-09-01T00:00:00Z", "end": "2026-09-08T00:00:00Z", "max_runs": 0}, "max_runs"),
    ],
)
def test_start_backfill_rejects_bad_input_locally(server, kwargs, match) -> None:
    with pytest.raises(ValueError, match=match):
        _client(server).start_backfill("nightly", **kwargs)
    assert server.requests == []


def test_list_and_get_backfills(server) -> None:
    server.reply("GET", "/schedules/nightly/backfills", 200, {"backfills": [_backfill("PAUSED")]})
    server.reply("GET", "/backfills/bf-1", 200, _backfill("RUNNING"))
    c = _client(server)
    assert [b["state"] for b in c.list_backfills("nightly", limit=3)] == ["PAUSED"]
    assert c.get_backfill("bf-1")["from"] == "2026-09-01T00:00:00Z"


@pytest.mark.parametrize(
    "action, state, allowed",
    [
        ("pause", "RUNNING", True),
        ("pause", "PAUSED", False),
        ("resume", "PAUSED", True),
        ("resume", "COMPLETED", False),
        ("resume", "CANCELLED", False),
        ("cancel", "PAUSED", True),
        ("cancel", "FAILED", False),
    ],
)
def test_backfill_actions_check_the_transition(server, action, state, allowed) -> None:
    server.reply("GET", "/backfills/bf-1", 200, _backfill(state))
    server.reply("POST", f"/backfills/bf-1/{action}", 200, _backfill("X"))
    call = getattr(_client(server), f"{action}_backfill")
    if allowed:
        call("bf-1")
        assert len(server.sent("POST", f"/backfills/bf-1/{action}")) == 1
    else:
        with pytest.raises(Conflict, match=state):
            call("bf-1")
        assert server.sent("POST", f"/backfills/bf-1/{action}") == []
