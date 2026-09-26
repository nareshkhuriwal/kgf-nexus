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
import ssl
import subprocess
import tempfile
import threading
from http.server import BaseHTTPRequestHandler
from typing import Any

from nexus._taskapi import TaskAPIClient


class _Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True
    fake: "FakeTaskAPI"


class _TLSServer(socketserver.ThreadingMixIn, socketserver.TCPServer):
    """The agent's network listener (F148): HTTPS on loopback."""

    daemon_threads = True
    allow_reuse_address = True
    fake: "FakeTaskAPI"

    def handle_error(self, request: Any, client_address: Any) -> None:
        # A client that refuses our certificate aborts the handshake; that is
        # the behaviour under test, not a server fault worth a traceback.
        pass


def make_ca_and_cert(directory: str, host: str = "127.0.0.1") -> tuple[str, str, str]:
    """A throwaway CA and a serving certificate for ``host`` signed by it, the
    shape the agent mints in memory. Returns (ca_pem, cert_file, key_file).
    Needs the ``openssl`` CLI; callers skip when it is missing."""
    def run(*args: str) -> None:
        subprocess.run(["openssl", *args], check=True, capture_output=True, cwd=directory)

    run("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", "ca.key")
    run("req", "-x509", "-new", "-key", "ca.key", "-subj", "/CN=fake agent CA", "-days", "1",
        "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign",
        "-out", "ca.pem")
    run("ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", "srv.key")
    run("req", "-new", "-key", "srv.key", "-subj", f"/CN={host}", "-out", "srv.csr")
    san = f"IP:{host}" if host.replace(".", "").isdigit() else f"DNS:{host}"
    with open(os.path.join(directory, "ext.cnf"), "w") as f:
        f.write(f"subjectAltName={san}\nextendedKeyUsage=serverAuth\n")
    run("x509", "-req", "-in", "srv.csr", "-CA", "ca.pem", "-CAkey", "ca.key", "-CAcreateserial",
        "-days", "1", "-extfile", "ext.cnf", "-out", "srv.pem")
    with open(os.path.join(directory, "ca.pem")) as f:
        ca_pem = f.read()
    return ca_pem, os.path.join(directory, "srv.pem"), os.path.join(directory, "srv.key")


def openssl_available() -> bool:
    return shutil.which("openssl") is not None


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server: _Server | _TLSServer

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
                 capabilities: dict[str, bool] | None = None, tls: bool = False) -> None:
        # sun_path is 104 bytes on Darwin: pytest's tmp_path is too long.
        self.dir = tempfile.mkdtemp(prefix="nxapi", dir="/tmp")
        self.path = os.path.join(self.dir, "api.sock")
        self.tls = tls
        self.ca_pem = ""
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
        self._server: _Server | _TLSServer
        if tls:
            self.ca_pem, cert, key = make_ca_and_cert(self.dir)
            ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            ctx.minimum_version = ssl.TLSVersion.TLSv1_2
            ctx.load_cert_chain(cert, key)
            self._server = _TLSServer(("127.0.0.1", 0), _Handler, bind_and_activate=True)
            self._server.socket = ctx.wrap_socket(self._server.socket, server_side=True)
            self.port = self._server.server_address[1]
        else:
            self._server = _Server(self.path, _Handler)
        self._server.fake = self
        self._thread = threading.Thread(target=self._server.serve_forever, kwargs={"poll_interval": 0.05}, daemon=True)

    def force_status(self, path: str, status: int, times: int = 1) -> None:
        """The next ``times`` requests to ``path`` get ``status``."""
        with self.lock:
            self.force[path] = (status, times)

    @property
    def url(self) -> str:
        return f"https://127.0.0.1:{self.port}"

    @property
    def env(self) -> dict[str, str]:
        if self.tls:
            return {"NEXUS_TASK_API": self.url, "NEXUS_TASK_API_TOKEN": self.token,
                    "NEXUS_TASK_API_CA": self.ca_pem}
        return {"NEXUS_TASK_API": f"unix://{self.path}", "NEXUS_TASK_API_TOKEN": self.token}

    def client(self, **kw: Any) -> TaskAPIClient:
        if self.tls:
            return TaskAPIClient.https(self.url, self.token, ca_pem=self.ca_pem, **kw)
        return TaskAPIClient(self.path, self.token, **kw)

    def __enter__(self) -> "FakeTaskAPI":
        self._thread.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._server.shutdown()
        self._server.server_close()
        shutil.rmtree(self.dir, ignore_errors=True)
