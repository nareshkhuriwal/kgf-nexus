"""The REST client, exercised against a real local HTTP server.

A stub server rather than mock patching, because the things worth testing here are
wire-level: that the canonical bytes are what gets POSTed, that the
Idempotency-Key lands in a HEADER and not the body, and that an error response
becomes an actionable exception.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from nexus import NexusClient, task, workflow
from nexus.canonical import canonical_bytes
from nexus.client import AuthError, Conflict, NexusAPIError, NexusError, NotFound, _parse_sse
from nexus.task import registry


class _Recorder:
    """Captures what the client sent and dictates what the server replies."""

    def __init__(self) -> None:
        self.requests: list[dict] = []
        self.responses: dict[tuple[str, str], tuple[int, bytes, dict]] = {}

    def reply(self, method: str, path: str, status: int, body, headers: dict | None = None) -> None:
        raw = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.responses[(method, path)] = (status, raw, headers or {})


@pytest.fixture
def server():
    rec = _Recorder()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *a):  # keep pytest output clean
            pass

        def _handle(self, method: str) -> None:
            length = int(self.headers.get("Content-Length") or 0)
            body = self.rfile.read(length) if length else b""
            path = self.path.split("?")[0]
            rec.requests.append(
                {
                    "method": method,
                    "path": path,
                    "full_path": self.path,
                    "headers": dict(self.headers),
                    "body": body,
                }
            )
            status, raw, extra = rec.responses.get((method, path), (200, b"{}", {}))
            self.send_response(status)
            self.send_header("Content-Type", extra.pop("Content-Type", "application/json"))
            self.send_header("Content-Length", str(len(raw)))
            for k, v in extra.items():
                self.send_header(k, v)
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self):
            self._handle("GET")

        def do_POST(self):
            self._handle("POST")

    httpd = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=httpd.serve_forever, daemon=True)
    thread.start()
    rec.endpoint = f"http://127.0.0.1:{httpd.server_address[1]}"  # type: ignore[attr-defined]
    yield rec
    httpd.shutdown()
    httpd.server_close()


@pytest.fixture(autouse=True)
def _isolate_registry():
    registry.clear()
    yield
    registry.clear()


def _client(server, **kw) -> NexusClient:
    return NexusClient(server.endpoint, api_key="secret-key", namespace="prod", max_retries=0, **kw)


def _workflow():
    @task(task_ref="c.one")
    def one(ctx, x):
        return x

    @workflow(name="client-demo", namespace="prod")
    def wf(x):
        return one(x)

    return wf


def test_submit_spec_posts_the_canonical_bytes(server) -> None:
    """The server content-addresses what it receives.

    Re-serializing here would risk a hash that differs from the one we printed for
    the user, which would be a very confusing bug to chase.
    """
    compiled = _workflow().compile()
    server.reply("POST", "/v1/namespaces/prod/specs", 200, {"version": 1, "spec_hash": compiled.hash})

    result = _client(server).submit_spec(compiled)
    assert result["spec_hash"] == compiled.hash

    sent = server.requests[-1]
    assert sent["body"] == canonical_bytes(compiled.spec)
    assert sent["headers"]["Authorization"] == "Bearer secret-key"


def test_dry_run_is_a_query_parameter(server) -> None:
    server.reply("POST", "/v1/namespaces/prod/specs", 200, {"valid": True})
    _client(server).validate_spec(_workflow())
    assert "dry_run=true" in server.requests[-1]["full_path"]


def test_create_run_sends_idempotency_key_as_a_header(server) -> None:
    """Per §8.2 it is a HEADER. In the body it would not reach the gateway's dedup."""
    server.reply("POST", "/v1/namespaces/prod/runs", 200, {"id": "run-1", "state": "PENDING"})
    _client(server).create_run("client-demo", params={"x": 1}, idempotency_key="daily-2026-09-12")

    sent = server.requests[-1]
    assert sent["headers"]["Idempotency-Key"] == "daily-2026-09-12"
    assert json.loads(sent["body"])["params"] == {"x": 1}
    assert "idempotency_key" not in json.loads(sent["body"])


def test_create_run_omits_the_header_when_no_key_is_given(server) -> None:
    server.reply("POST", "/v1/namespaces/prod/runs", 200, {"id": "run-2"})
    _client(server).create_run("client-demo")
    assert "Idempotency-Key" not in server.requests[-1]["headers"]


def test_run_lifecycle_paths(server) -> None:
    c = _client(server)
    for method, path, call in [
        ("GET", "/v1/namespaces/prod/runs/run-1", lambda: c.get_run("run-1")),
        ("GET", "/v1/namespaces/prod/runs", lambda: c.list_runs(state="RUNNING")),
        ("POST", "/v1/namespaces/prod/runs/run-1/cancel", lambda: c.cancel_run("run-1", reason="oops")),
        ("POST", "/v1/namespaces/prod/runs/run-1/retry", lambda: c.retry_run("run-1", nodes=["n1"])),
        ("POST", "/v1/namespaces/prod/runs/run-1/pause", lambda: c.pause_run("run-1")),
        ("POST", "/v1/namespaces/prod/runs/run-1/resume", lambda: c.resume_run("run-1")),
        ("GET", "/v1/namespaces/prod/workers", lambda: c.list_workers()),
        ("GET", "/v1/namespaces/prod/capacity", lambda: c.capacity()),
        ("GET", "/v1/namespaces/prod/specs/client-demo/versions", lambda: c.list_spec_versions("client-demo")),
    ]:
        server.reply(method, path, 200, {"ok": True})
        call()
        assert (server.requests[-1]["method"], server.requests[-1]["path"]) == (method, path)


def test_namespace_is_url_encoded(server) -> None:
    c = NexusClient(server.endpoint, api_key="k", namespace="team/a", max_retries=0)
    server.reply("GET", "/v1/namespaces/team%2Fa/capacity", 200, {"ok": True})
    c.capacity()
    assert server.requests[-1]["path"] == "/v1/namespaces/team%2Fa/capacity"


def test_401_becomes_an_actionable_auth_error(server) -> None:
    server.reply("GET", "/v1/namespaces/prod/capacity", 401, {"message": "invalid api key"})
    with pytest.raises(AuthError) as e:
        _client(server).capacity()
    # "HTTP 401" alone tells an operator nothing they can act on.
    assert "NEXUS_API_KEY" in str(e.value)
    assert e.value.status == 401


def test_404_and_409_get_specific_types(server) -> None:
    server.reply("GET", "/v1/namespaces/prod/runs/gone", 404, {"message": "no such run"})
    with pytest.raises(NotFound):
        _client(server).get_run("gone")

    server.reply("POST", "/v1/namespaces/prod/runs", 409, {"message": "idempotency key reused"})
    with pytest.raises(Conflict):
        _client(server).create_run("client-demo", idempotency_key="dup")


def test_error_body_details_and_request_id_are_surfaced(server) -> None:
    server.reply(
        "POST",
        "/v1/namespaces/prod/specs",
        400,
        {"message": "spec invalid", "details": {"tasks[0].resources": "cpu required"}},
        {"X-Request-Id": "req-abc"},
    )
    with pytest.raises(NexusAPIError) as e:
        _client(server).submit_spec(_workflow())
    msg = str(e.value)
    assert "spec invalid" in msg and "cpu required" in msg and "req-abc" in msg


def test_non_json_body_says_what_is_probably_wrong(server) -> None:
    server.reply(
        "GET", "/v1/namespaces/prod/capacity", 200, b"<html>nginx</html>", {"Content-Type": "text/html"}
    )
    with pytest.raises(NexusError, match="NEXUS_ENDPOINT"):
        _client(server).capacity()


def test_unreachable_endpoint_names_the_endpoint() -> None:
    c = NexusClient("http://127.0.0.1:1", api_key="k", max_retries=0, timeout=1.0)
    with pytest.raises(NexusError, match="cannot reach Nexus at http://127.0.0.1:1"):
        c.capacity()


def test_repr_never_contains_key_material() -> None:
    c = NexusClient("http://x", api_key="super-secret-value")
    # Reprs end up in logs and tracebacks, which end up in tickets.
    assert "super-secret-value" not in repr(c)
    assert "http://x" in repr(c)


def test_retries_5xx_then_succeeds(server) -> None:
    """Retrying a mutating call is safe here: spec submission is content-addressed."""
    calls = {"n": 0}
    compiled = _workflow().compile()

    class Flaky(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def do_POST(self):
            calls["n"] += 1
            length = int(self.headers.get("Content-Length") or 0)
            self.rfile.read(length)
            body = b'{"ok":true}' if calls["n"] > 2 else b'{"message":"unavailable"}'
            self.send_response(200 if calls["n"] > 2 else 503)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    httpd = HTTPServer(("127.0.0.1", 0), Flaky)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        c = NexusClient(f"http://127.0.0.1:{httpd.server_address[1]}", api_key="k", max_retries=3)
        assert c.submit_spec(compiled)["ok"] is True
        assert calls["n"] == 3
    finally:
        httpd.shutdown()
        httpd.server_close()


def test_4xx_is_not_retried(server) -> None:
    server.reply("GET", "/v1/namespaces/prod/capacity", 400, {"message": "bad"})
    c = NexusClient(server.endpoint, api_key="k", namespace="prod", max_retries=3)
    with pytest.raises(NexusAPIError):
        c.capacity()
    # A 400 will not change its mind; retrying only delays the error.
    assert len(server.requests) == 1


def test_stream_events_parses_sse(server) -> None:
    body = (
        b'data: {"seq":1,"type":"RunStarted"}\n\n'
        b": keep-alive\n\n"
        b'data: {"seq":2,"type":"NodeSucceeded"}\n\n'
    )
    server.reply(
        "GET",
        "/v1/namespaces/prod/runs/run-1/events",
        200,
        body,
        {"Content-Type": "text/event-stream"},
    )
    events = list(_client(server).stream_events("run-1", after_seq=0))
    assert [e["seq"] for e in events] == [1, 2]
    assert "watch=true" in server.requests[-1]["full_path"]


def test_sse_parser_handles_multiline_and_malformed_data() -> None:
    import io

    stream = io.BytesIO(
        b'data: {"a":\ndata: 1}\n\n'
        b"data: not json\n\n"          # dropped rather than raised
        b': comment\n\n'
        b'data: {"b":2}\n\n'
    )
    assert list(_parse_sse(stream)) == [{"a": 1}, {"b": 2}]


def test_get_logs_yields_lines(server) -> None:
    server.reply(
        "GET",
        "/v1/namespaces/prod/attempts/att-1/logs",
        200,
        b"line one\nline two\n",
        {"Content-Type": "text/plain"},
    )
    assert list(_client(server).get_logs("att-1", follow=True)) == ["line one", "line two"]
    assert "follow=true" in server.requests[-1]["full_path"]


def test_endpoint_and_key_come_from_the_environment(monkeypatch) -> None:
    monkeypatch.setenv("NEXUS_ENDPOINT", "http://env-host:9090/")
    monkeypatch.setenv("NEXUS_API_KEY", "env-key")
    monkeypatch.setenv("NEXUS_NAMESPACE", "env-ns")
    c = NexusClient()
    assert c.endpoint == "http://env-host:9090"  # trailing slash trimmed
    assert c.namespace == "env-ns"
    assert c._api_key == "env-key"


def test_submit_and_run_compiles_submits_then_creates(server) -> None:
    server.reply("POST", "/v1/namespaces/prod/specs", 200, {"version": 1})
    server.reply("POST", "/v1/namespaces/prod/runs", 200, {"id": "run-9"})
    assert _client(server).submit_and_run(_workflow(), params={"x": 2})["id"] == "run-9"
    assert [r["path"] for r in server.requests] == [
        "/v1/namespaces/prod/specs",
        "/v1/namespaces/prod/runs",
    ]


def test_wait_for_run_returns_on_a_terminal_state(server) -> None:
    server.reply("GET", "/v1/namespaces/prod/runs/run-1", 200, {"id": "run-1", "state": "SUCCEEDED"})
    assert _client(server).wait_for_run("run-1", poll_interval=0.01)["state"] == "SUCCEEDED"


def test_wait_for_run_times_out_with_the_last_known_state(server) -> None:
    server.reply("GET", "/v1/namespaces/prod/runs/run-1", 200, {"id": "run-1", "state": "RUNNING"})
    with pytest.raises(NexusError, match="still RUNNING"):
        _client(server).wait_for_run("run-1", poll_interval=0.01, timeout=0.05)
