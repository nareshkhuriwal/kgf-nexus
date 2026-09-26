# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""An in-memory fake of the agent's workload API (ADR-0032 §1), for tests.

A threaded HTTP/1.1 server on a Unix socket implementing every endpoint the
SDK calls, with the state a test wants to assert on (usage totals, progress
messages, permit asks and settlements, checkpoints, declared errors) and the
switches it wants to flip: a stale lease, per-path forced statuses, scripted
permit denials, a checkpoint size limit, disabled capabilities.
"""

from __future__ import annotations

import hashlib
import json
import os
import shutil
import socketserver
import tempfile
import threading
from http.server import BaseHTTPRequestHandler
from typing import Any

from nexus._taskapi import TaskAPIClient


class _Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    fake: "FakeTaskAPI"


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server: _Server

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002
        pass

    def _send(self, status: int, doc: Any = None, *, body: bytes | None = None,
              headers: dict[str, str] | None = None) -> None:
        data = body if body is not None else (b"" if doc is None else json.dumps(doc).encode())
        self.send_response(status)
        if doc is not None:
            self.send_header("Content-Type", "application/json")
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _handle(self, method: str) -> None:
        fake = self.server.fake
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        path = self.path
        with fake.lock:
            fake.calls.append((method, path))
        if self.headers.get("Authorization") != f"Bearer {fake.token}":
            self._send(401, {"error": "unauthorized", "message": "bad token"})
            return
        if path == "/v1/info" and method == "GET":
            self._send(200, {"attempt_id": fake.attempt_id, "capabilities": dict(fake.capabilities)})
            return
        with fake.lock:
            forced = fake.force.get(path)
            if forced and forced[1] > 0:
                fake.force[path] = (forced[0], forced[1] - 1)
                status = forced[0]
                code = {409: "stale_lease", 413: "too_large", 501: "unsupported",
                        503: "unavailable", 400: "invalid_usage"}.get(status, "error")
                self._send(status, {"error": code, "message": "forced by test"})
                return
        if fake.stale and path != "/v1/error":
            self._send(409, {"error": "stale_lease", "message": "lease reclaimed"})
            return
        cap_for = {"/v1/usage": "usage", "/v1/progress": "progress", "/v1/permits": "rate_limits",
                   "/v1/permits/settle": "rate_limits", "/v1/checkpoint": "checkpoints"}
        cap = cap_for.get(path)
        if cap and not fake.capabilities.get(cap):
            self._send(501, {"error": "unsupported", "message": f"{cap} not offered"})
            return
        doc: Any = json.loads(raw) if raw and path != "/v1/checkpoint" else None
        route = (method, path)
        with fake.lock:
            if route == ("POST", "/v1/usage"):
                for k, v in doc["units"].items():
                    fake.usage[k] = fake.usage.get(k, 0) + v
                self._send(200, {"totals": dict(fake.usage)})
            elif route == ("POST", "/v1/progress"):
                fake.progress.append((doc.get("message"), doc.get("fraction")))
                self._send(204)
            elif route == ("POST", "/v1/permits"):
                fake.permit_asks.append(doc)
                decision = fake.permit_script.pop(0) if fake.permit_script else {"granted": True}
                decision.setdefault("retry_after_ms", 0)
                decision.setdefault("unlimited_keys", [])
                self._send(200, decision)
            elif route == ("POST", "/v1/permits/settle"):
                fake.settles.append(doc)
                self._send(204)
            elif route == ("PUT", "/v1/checkpoint"):
                if len(raw) > fake.max_checkpoint_bytes:
                    self._send(413, {"error": "too_large", "message": "checkpoint too large"})
                    return
                fake.seq += 1
                key = f"default/checkpoints/idem/{fake.attempt_id}/{fake.seq}"
                fake.checkpoints.append((fake.seq, raw))
                fake.current = (fake.seq, fake.attempt_id, raw)
                self._send(200, {"seq": fake.seq, "key": key, "sha256": hashlib.sha256(raw).hexdigest(),
                                 "size_bytes": len(raw)})
            elif route == ("GET", "/v1/checkpoint"):
                if fake.current is None:
                    self._send(404, {"error": "not_found", "message": "no checkpoint"})
                    return
                seq, attempt, data = fake.current
                self._send(200, body=data, headers={
                    "Content-Type": "application/octet-stream",
                    "X-Nexus-Checkpoint-Seq": str(seq), "X-Nexus-Checkpoint-Attempt": attempt,
                    "X-Nexus-Checkpoint-Saved-At": "2026-09-26T00:00:00Z",
                })
            elif route == ("POST", "/v1/error"):
                fake.errors.append(doc)
                self._send(204)
            else:
                self._send(404, {"error": "not_found", "message": f"no route {method} {path}"})

    def do_GET(self) -> None:  # noqa: N802
        self._handle("GET")

    def do_POST(self) -> None:  # noqa: N802
        self._handle("POST")

    def do_PUT(self) -> None:  # noqa: N802
        self._handle("PUT")


class FakeTaskAPI:
    """Start with ``with FakeTaskAPI() as api:``; ``api.client()`` / ``api.env``."""

    def __init__(self, *, token: str = "test-token", attempt_id: str = "att-1",
                 capabilities: dict[str, bool] | None = None) -> None:
        # sun_path is 104 bytes on Darwin: pytest's tmp_path is too long.
        self.dir = tempfile.mkdtemp(prefix="nxapi", dir="/tmp")
        self.path = os.path.join(self.dir, "api.sock")
        self.token = token
        self.attempt_id = attempt_id
        self.capabilities = capabilities or {"usage": True, "rate_limits": True,
                                             "checkpoints": True, "progress": True}
        self.lock = threading.Lock()
        self.calls: list[tuple[str, str]] = []
        self.usage: dict[str, int] = {}
        self.progress: list[tuple[str | None, float | None]] = []
        self.permit_asks: list[dict[str, Any]] = []
        self.permit_script: list[dict[str, Any]] = []
        self.settles: list[dict[str, Any]] = []
        self.checkpoints: list[tuple[int, bytes]] = []
        self.current: tuple[int, str, bytes] | None = None
        self.errors: list[dict[str, Any]] = []
        self.force: dict[str, tuple[int, int]] = {}
        self.stale = False
        self.seq = 0
        self.max_checkpoint_bytes = 1 << 20
        self._server = _Server(self.path, _Handler)
        self._server.fake = self
        self._thread = threading.Thread(target=self._server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)

    def force_status(self, path: str, status: int, times: int = 1) -> None:
        """The next ``times`` requests to ``path`` get ``status``."""
        with self.lock:
            self.force[path] = (status, times)

    @property
    def env(self) -> dict[str, str]:
        return {"NEXUS_TASK_API": f"unix://{self.path}", "NEXUS_TASK_API_TOKEN": self.token}

    def client(self, **kw: Any) -> TaskAPIClient:
        return TaskAPIClient(self.path, self.token, **kw)

    def __enter__(self) -> "FakeTaskAPI":
        self._thread.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._server.shutdown()
        self._server.server_close()
        shutil.rmtree(self.dir, ignore_errors=True)
