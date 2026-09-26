# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""An OpenAI- and Anthropic-compatible mock model server, for tests and demos.

    from nexus.testing import MockLLMServer

    with MockLLMServer(rate_limit_every=5) as mock:
        llm = LLMClient(ctx, provider="openai", model="mock-large",
                        base_url=mock.base_url, api_key="test")
        llm.complete("What is 12 + 7?").text      # "19"
        mock.stats.requests["/v1/chat/completions"]

or as a compose sidecar:

    python -m nexus.testing.mock_llm --host 0.0.0.0 --port 8089 [--rate-limit-every N]

Nothing here is a model. Every answer is a pure function of the request, so a
pipeline built on it is reproducible to the byte, and every token count is the
whitespace word count of the text (at least 1), so usage assertions are exact.

ENDPOINTS
  POST /v1/chat/completions         OpenAI chat (Bearer auth)
  POST /v1/messages                 Anthropic messages (x-api-key or Bearer;
                                    ``max_tokens`` required, as upstream)
  POST /v1/files                    OpenAI-style upload, as a JSON body
                                    ``{"purpose", "filename", "content"}``
                                    (multipart is NOT implemented; it is
                                    answered 400 with a pointer to the JSON form)
  POST /v1/fine_tuning/jobs         ``{"model", "training_file", "hyperparameters"}``
  GET  /v1/fine_tuning/jobs/{id}    each poll advances the status one step:
                                    validating_files -> queued -> running -> succeeded
  GET  /v1/fine_tuning/jobs         list
  GET  /v1/models                   base models plus succeeded fine-tunes
  GET  /healthz                     no auth

Any non-empty key is accepted; a missing key is 401, so a client that forgets
its auth header fails here exactly as it would against a real provider.

THE RESPONDER (:func:`respond`), in order:
  a. a grader prompt -- system (or prompt) containing "You are a strict
     grader", with lines "Reference: X" and "Candidate: Y" -> "PASS" when X and
     Y are equal after normalising case, whitespace and trailing punctuation,
     else "FAIL";
  b. arithmetic, "What is 12 + 7?" (+ - * x /) -> "19";
  c. "Classify the sentiment ..." -> positive / negative / neutral by keyword
     counts over the text after the instruction;
  d. anything else -> "Summary: <first 8 words>".

THE QUALITY KNOB. A base model whose name contains "small" (``mock-small``)
answers a deterministic ~30% of arithmetic questions wrongly (off by one,
chosen by a hash of the question), so an evaluation gate has something to
fail. Fine-tuned models (``ft:`` prefix) and every other model answer
correctly -- fine-tuning ``mock-small`` "fixes" it, which is the story an
eval-gated fine-tuning pipeline wants to demonstrate.

THROTTLING. Completion requests (both chat endpoints) are counted: the first
``fail_first`` get 503, and every ``rate_limit_every``-th gets 429, both with
``Retry-After: retry_after_s``. A fractional ``retry_after_s`` is sent as a
decimal (real providers send integers; ``nexus.llm`` accepts both), which
keeps tests fast.
"""

from __future__ import annotations

import argparse
import dataclasses
import hashlib
import json
import re
import signal
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Mapping, Sequence

__all__ = ["MockLLMServer", "MockStats", "respond", "count_tokens", "main", "BASE_MODELS"]

BASE_MODELS = ("mock-small", "mock-large")
_FT_STATUSES = ("validating_files", "queued", "running", "succeeded")
_WRONG_PERCENT = 30

_ARITH = re.compile(r"what\s+is\s+(-?\d+(?:\.\d+)?)\s*([-+*/x×])\s*(-?\d+(?:\.\d+)?)", re.IGNORECASE)
_SENTIMENT = re.compile(r"classify the sentiment", re.IGNORECASE)
_GRADER = "you are a strict grader"
_POSITIVE = frozenset(
    "good great excellent love loved loves amazing wonderful fantastic happy best delightful perfect "
    "awesome enjoyed enjoy recommend brilliant superb pleased".split()
)
_NEGATIVE = frozenset(
    "bad terrible awful hate hated hates worst poor disappointing disappointed horrible broken "
    "boring refund useless slow angry rude waste".split()
)


def count_tokens(text: str) -> int:
    """The mock's token count: whitespace-separated words, at least 1."""
    return max(1, len(text.split()))


def _normalize(s: str) -> str:
    return " ".join(s.strip().lower().rstrip(".!?;:,").split())


def _fmt(x: float) -> str:
    if x == int(x):
        return str(int(x))
    return str(round(x, 4))


def _arithmetic(m: "re.Match[str]", model: str) -> str:
    a, op, b = float(m.group(1)), m.group(2), float(m.group(3))
    if op == "+":
        r = a + b
    elif op == "-":
        r = a - b
    elif op in ("*", "x", "×"):
        r = a * b
    else:
        if b == 0:
            return "undefined"
        r = a / b
    question = f"{_fmt(a)}{op}{_fmt(b)}"
    if "small" in model and not model.startswith("ft:"):
        h = int(hashlib.sha256(question.encode("utf-8")).hexdigest()[:8], 16)
        if h % 100 < _WRONG_PERCENT:
            r += 1
    return _fmt(r)


def _sentiment(text: str) -> str:
    m = _SENTIMENT.search(text)
    assert m is not None
    rest = text[m.end():]
    cut = min((i for i in (rest.find(":"), rest.find("\n")) if i >= 0), default=-1)
    payload = rest[cut + 1:] if cut >= 0 else rest
    words = [w.strip(".,!?;:\"'()[]").lower() for w in payload.split()]
    pos = sum(1 for w in words if w in _POSITIVE)
    neg = sum(1 for w in words if w in _NEGATIVE)
    if pos > neg:
        return "positive"
    if neg > pos:
        return "negative"
    return "neutral"


def _grade(text: str) -> str | None:
    ref = cand = None
    for line in text.splitlines():
        s = line.strip()
        if s.lower().startswith("reference:"):
            ref = s[len("reference:"):]
        elif s.lower().startswith("candidate:"):
            cand = s[len("candidate:"):]
    if ref is None or cand is None:
        return None
    return "PASS" if _normalize(ref) == _normalize(cand) else "FAIL"


def respond(prompt: str, *, system: str = "", model: str = "mock-large") -> str:
    """The mock's deterministic answer to ``prompt`` (see the module doc)."""
    if _GRADER in system.lower() or _GRADER in prompt.lower():
        verdict = _grade(f"{system}\n{prompt}")
        if verdict is not None:
            return verdict
    m = _ARITH.search(prompt)
    if m is not None:
        return _arithmetic(m, model)
    if _SENTIMENT.search(prompt):
        return _sentiment(prompt)
    words = prompt.split()
    return "Summary: " + " ".join(words[:8]) if words else "Summary:"


@dataclasses.dataclass(frozen=True, slots=True)
class MockStats:
    """A snapshot of what the server has seen.

    ``requests`` counts requests per path (``/v1/fine_tuning/jobs/{id}`` for
    every job poll); the token totals cover successful completions only.
    """

    requests: Mapping[str, int]
    total_requests: int
    completions: int
    input_tokens: int
    output_tokens: int
    throttled: int
    unavailable: int
    unauthorized: int

    @property
    def total_tokens(self) -> int:
        return self.input_tokens + self.output_tokens


def _content_text(content: Any) -> str:
    """A message's text: a string, or the text blocks of a content list."""
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "".join(
            str(b.get("text") or "") for b in content if isinstance(b, Mapping) and b.get("type") in (None, "text")
        )
    return ""


class _Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "NexusMockLLM/1"
    mock: "MockLLMServer"

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A002 - stdlib signature
        if self.mock.verbose:
            # Request line and status only: never headers, so never a key.
            sys.stderr.write("mock-llm: " + (format % args) + "\n")

    # ── plumbing ─────────────────────────────────────────────────────────────
    def _send(self, status: int, doc: Any, headers: Mapping[str, str] | None = None) -> None:
        body = json.dumps(doc).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        for k, v in (headers or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status: int, message: str, kind: str, headers: Mapping[str, str] | None = None) -> None:
        if self.path.startswith("/v1/messages"):
            self._send(status, {"type": "error", "error": {"type": kind, "message": message}}, headers)
        else:
            self._send(status, {"error": {"message": message, "type": kind, "code": kind}}, headers)

    def _body(self) -> Any:
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        if not raw:
            return None
        return json.loads(raw)

    def _authorized(self) -> bool:
        auth = self.headers.get("Authorization") or ""
        if auth.startswith("Bearer ") and auth[len("Bearer "):].strip():
            return True
        return bool((self.headers.get("x-api-key") or "").strip())

    def _route(self) -> str:
        path = self.path.split("?", 1)[0]
        if path.startswith("/v1/fine_tuning/jobs/"):
            return "/v1/fine_tuning/jobs/{id}"
        return path

    def _dispatch(self, method: str) -> None:
        route = self._route()
        self.mock._count(route)
        if route == "/healthz" and method == "GET":
            self._send(200, {"status": "ok"})
            return
        if not self._authorized():
            self.mock._bump("unauthorized")
            self._error(401, "missing API key", "authentication_error")
            return
        try:
            body = self._body() if method == "POST" else None
        except ValueError:
            self._error(400, "request body is not valid JSON", "invalid_request_error")
            return
        handlers = {
            ("POST", "/v1/chat/completions"): self._chat,
            ("POST", "/v1/messages"): self._messages,
            ("POST", "/v1/files"): self._files,
            ("POST", "/v1/fine_tuning/jobs"): self._create_job,
            ("GET", "/v1/fine_tuning/jobs"): self._list_jobs,
            ("GET", "/v1/fine_tuning/jobs/{id}"): self._get_job,
            ("GET", "/v1/models"): self._models,
        }
        fn = handlers.get((method, route))
        if fn is None:
            self._error(404, f"no route {method} {route}", "not_found_error")
            return
        fn(body)

    def do_GET(self) -> None:  # noqa: N802 - stdlib naming
        self._dispatch("GET")

    def do_POST(self) -> None:  # noqa: N802 - stdlib naming
        if (self.headers.get("Content-Type") or "").startswith("multipart/form-data"):
            self.mock._count(self._route())
            n = int(self.headers.get("Content-Length") or 0)
            if n:
                self.rfile.read(n)
            self._error(
                400, "the mock accepts file uploads as JSON {purpose, filename, content}, not multipart",
                "invalid_request_error",
            )
            return
        self._dispatch("POST")

    # ── completions ──────────────────────────────────────────────────────────
    def _throttle(self) -> bool:
        """Apply fail_first / rate_limit_every. True if a refusal was sent."""
        n = self.mock._next_call()
        headers = {"Retry-After": _fmt(self.mock.retry_after_s)}
        if n <= self.mock.fail_first:
            self.mock._bump("unavailable")
            self._error(503, "the mock is overloaded (fail_first)", "overloaded_error", headers)
            return True
        every = self.mock.rate_limit_every
        if every > 0 and n % every == 0:
            self.mock._bump("throttled")
            self._error(429, "rate limit reached (rate_limit_every)", "rate_limit_error", headers)
            return True
        return False

    def _parse_chat(self, body: Any, *, anthropic: bool) -> tuple[str, str, str, int | None] | str:
        if not isinstance(body, Mapping):
            return "request body must be a JSON object"
        model = body.get("model")
        if not isinstance(model, str) or not model:
            return "model is required"
        messages = body.get("messages")
        if not isinstance(messages, list) or not messages:
            return "messages must be a non-empty list"
        max_tokens = body.get("max_tokens")
        if anthropic and not isinstance(max_tokens, int):
            return "max_tokens is required"
        if max_tokens is not None and (not isinstance(max_tokens, int) or max_tokens <= 0):
            return "max_tokens must be a positive integer"
        system_parts: list[str] = []
        if anthropic and body.get("system"):
            system_parts.append(_content_text(body.get("system")))
        prompt_parts: list[str] = []
        for m in messages:
            if not isinstance(m, Mapping):
                return "each message must be an object"
            text = _content_text(m.get("content"))
            if m.get("role") == "system":
                system_parts.append(text)
            elif m.get("role") == "user":
                prompt_parts.append(text)
        return model, "\n".join(system_parts), "\n".join(prompt_parts), max_tokens

    def _answer(self, model: str, system: str, prompt: str, max_tokens: int | None) -> tuple[str, int, int, bool]:
        text = respond(prompt, system=system, model=model)
        words = text.split()
        truncated = max_tokens is not None and len(words) > max_tokens
        if truncated:
            assert max_tokens is not None
            text = " ".join(words[:max_tokens])
        input_tokens = count_tokens(f"{system} {prompt}")
        output_tokens = count_tokens(text)
        self.mock._tokens(input_tokens, output_tokens)
        return text, input_tokens, output_tokens, truncated

    def _chat(self, body: Any) -> None:
        parsed = self._parse_chat(body, anthropic=False)
        if isinstance(parsed, str):
            self._error(400, parsed, "invalid_request_error")
            return
        if self._throttle():
            return
        model, system, prompt, max_tokens = parsed
        text, tin, tout, truncated = self._answer(model, system, prompt, max_tokens)
        self._send(200, {
            "id": f"chatcmpl-mock-{self.mock._next_id()}",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": model,
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": text},
                "finish_reason": "length" if truncated else "stop",
            }],
            "usage": {"prompt_tokens": tin, "completion_tokens": tout, "total_tokens": tin + tout},
        })

    def _messages(self, body: Any) -> None:
        parsed = self._parse_chat(body, anthropic=True)
        if isinstance(parsed, str):
            self._error(400, parsed, "invalid_request_error")
            return
        if self._throttle():
            return
        model, system, prompt, max_tokens = parsed
        text, tin, tout, truncated = self._answer(model, system, prompt, max_tokens)
        self._send(200, {
            "id": f"msg_mock_{self.mock._next_id()}",
            "type": "message",
            "role": "assistant",
            "model": model,
            "content": [{"type": "text", "text": text}],
            "stop_reason": "max_tokens" if truncated else "end_turn",
            "stop_sequence": None,
            "usage": {"input_tokens": tin, "output_tokens": tout},
        })

    # ── fine-tuning ──────────────────────────────────────────────────────────
    def _files(self, body: Any) -> None:
        if not isinstance(body, Mapping) or not isinstance(body.get("content"), str):
            self._error(400, "expected JSON {purpose, filename, content}", "invalid_request_error")
            return
        self._send(200, self.mock._add_file(
            str(body.get("purpose") or "fine-tune"), str(body.get("filename") or "data.jsonl"), body["content"]
        ))

    def _create_job(self, body: Any) -> None:
        if not isinstance(body, Mapping):
            self._error(400, "request body must be a JSON object", "invalid_request_error")
            return
        model, training_file = body.get("model"), body.get("training_file")
        if not isinstance(model, str) or not model:
            self._error(400, "model is required", "invalid_request_error")
            return
        if not isinstance(training_file, str) or not self.mock._has_file(training_file):
            self._error(400, f"training_file {training_file!r} not found", "invalid_request_error")
            return
        hp = body.get("hyperparameters")
        self._send(200, self.mock._create_job(model, training_file, dict(hp) if isinstance(hp, Mapping) else {}))

    def _get_job(self, _body: Any) -> None:
        job_id = self.path.split("?", 1)[0].rsplit("/", 1)[-1]
        job = self.mock._poll_job(job_id)
        if job is None:
            self._error(404, f"fine-tuning job {job_id!r} not found", "not_found_error")
            return
        self._send(200, job)

    def _list_jobs(self, _body: Any) -> None:
        self._send(200, {"object": "list", "data": self.mock._jobs_snapshot(), "has_more": False})

    def _models(self, _body: Any) -> None:
        self._send(200, {
            "object": "list",
            "data": [{"id": m, "object": "model", "created": 0, "owned_by": "nexus-mock"}
                     for m in self.mock.models()],
        })


class MockLLMServer:
    """A threaded mock model server on ``host:port`` (port 0 = any free port).

    Use as a context manager, or call :meth:`start` / :meth:`stop`. All state
    is guarded by one lock, so concurrent clients see consistent counters.
    """

    def __init__(
        self,
        host: str = "127.0.0.1",
        port: int = 0,
        *,
        rate_limit_every: int = 0,
        retry_after_s: float = 1.0,
        fail_first: int = 0,
        verbose: bool = False,
    ) -> None:
        if rate_limit_every < 0 or fail_first < 0 or retry_after_s < 0:
            raise ValueError("rate_limit_every, fail_first and retry_after_s must be >= 0")
        self.rate_limit_every = rate_limit_every
        self.retry_after_s = retry_after_s
        self.fail_first = fail_first
        self.verbose = verbose
        self._lock = threading.Lock()
        self._calls = 0
        self._ids = 0
        self._requests: dict[str, int] = {}
        self._counters = {"completions": 0, "input_tokens": 0, "output_tokens": 0,
                          "throttled": 0, "unavailable": 0, "unauthorized": 0}
        self._files: dict[str, dict[str, Any]] = {}
        self._file_content: dict[str, str] = {}
        self._jobs: dict[str, dict[str, Any]] = {}
        handler = type("_BoundHandler", (_Handler,), {"mock": self})
        self._server = ThreadingHTTPServer((host, port), handler)
        self._server.daemon_threads = True
        self._thread: threading.Thread | None = None

    # ── lifecycle ────────────────────────────────────────────────────────────
    @property
    def host(self) -> str:
        return str(self._server.server_address[0])

    @property
    def port(self) -> int:
        return int(self._server.server_address[1])

    @property
    def base_url(self) -> str:
        """``http://host:port`` -- what ``LLMClient(base_url=...)`` takes."""
        host = self.host
        if host in ("0.0.0.0", ""):
            host = "127.0.0.1"
        return f"http://{host}:{self.port}"

    def start(self) -> "MockLLMServer":
        if self._thread is None:
            self._thread = threading.Thread(
                target=self._server.serve_forever, kwargs={"poll_interval": 0.05},
                name="mock-llm", daemon=True,
            )
            self._thread.start()
        return self

    def stop(self) -> None:
        if self._thread is not None:
            self._server.shutdown()
            self._thread.join(timeout=5)
            self._thread = None
        self._server.server_close()

    def serve_forever(self) -> None:
        """Serve on the calling thread (the CLI)."""
        self._server.serve_forever()

    def __enter__(self) -> "MockLLMServer":
        return self.start()

    def __exit__(self, *exc: object) -> None:
        self.stop()

    # ── observation ──────────────────────────────────────────────────────────
    @property
    def stats(self) -> MockStats:
        with self._lock:
            c = dict(self._counters)
            return MockStats(
                requests=dict(self._requests),
                total_requests=sum(self._requests.values()),
                completions=c["completions"],
                input_tokens=c["input_tokens"],
                output_tokens=c["output_tokens"],
                throttled=c["throttled"],
                unavailable=c["unavailable"],
                unauthorized=c["unauthorized"],
            )

    def reset_stats(self) -> None:
        """Zero the counters, including the throttling call counter."""
        with self._lock:
            self._requests.clear()
            self._calls = 0
            for k in self._counters:
                self._counters[k] = 0

    def models(self) -> list[str]:
        with self._lock:
            tuned = [str(j["fine_tuned_model"]) for j in self._jobs.values() if j.get("fine_tuned_model")]
        return list(BASE_MODELS) + sorted(tuned)

    # ── state, for the handler ───────────────────────────────────────────────
    def _count(self, route: str) -> None:
        with self._lock:
            self._requests[route] = self._requests.get(route, 0) + 1

    def _bump(self, counter: str) -> None:
        with self._lock:
            self._counters[counter] += 1

    def _tokens(self, tin: int, tout: int) -> None:
        with self._lock:
            self._counters["completions"] += 1
            self._counters["input_tokens"] += tin
            self._counters["output_tokens"] += tout

    def _next_call(self) -> int:
        with self._lock:
            self._calls += 1
            return self._calls

    def _next_id(self) -> int:
        with self._lock:
            self._ids += 1
            return self._ids

    def _add_file(self, purpose: str, filename: str, content: str) -> dict[str, Any]:
        with self._lock:
            file_id = f"file-mock-{len(self._files) + 1}"
            meta = {
                "id": file_id, "object": "file", "bytes": len(content.encode("utf-8")),
                "created_at": int(time.time()), "filename": filename, "purpose": purpose,
                "status": "processed",
            }
            self._files[file_id] = meta
            self._file_content[file_id] = content
            return dict(meta)

    def _has_file(self, file_id: str) -> bool:
        with self._lock:
            return file_id in self._files

    def _create_job(self, model: str, training_file: str, hp: dict[str, Any]) -> dict[str, Any]:
        with self._lock:
            job_id = f"ftjob-mock{len(self._jobs) + 1}"
            epochs = hp.get("n_epochs")
            hp = {**hp, "n_epochs": epochs if isinstance(epochs, int) and epochs > 0 else 3}
            job: dict[str, Any] = {
                "id": job_id, "object": "fine_tuning.job", "model": model,
                "training_file": training_file, "validation_file": None,
                "hyperparameters": hp, "status": _FT_STATUSES[0],
                "created_at": int(time.time()), "finished_at": None,
                "fine_tuned_model": None, "trained_tokens": None, "error": None,
                "_polls": 0,
            }
            self._jobs[job_id] = job
            return _public(job)

    def _poll_job(self, job_id: str) -> dict[str, Any] | None:
        with self._lock:
            job = self._jobs.get(job_id)
            if job is None:
                return None
            if job["status"] != "succeeded":
                job["_polls"] += 1
                job["status"] = _FT_STATUSES[min(job["_polls"], len(_FT_STATUSES) - 1)]
                if job["status"] == "succeeded":
                    content = self._file_content.get(job["training_file"], "")
                    job["fine_tuned_model"] = f"ft:{job['model']}:mock:{job_id}"
                    job["trained_tokens"] = count_tokens(content) * int(job["hyperparameters"]["n_epochs"])
                    job["finished_at"] = int(time.time())
            return _public(job)

    def _jobs_snapshot(self) -> list[dict[str, Any]]:
        with self._lock:
            return [_public(j) for j in self._jobs.values()]


def _public(job: Mapping[str, Any]) -> dict[str, Any]:
    return {k: v for k, v in job.items() if not k.startswith("_")}


def main(argv: Sequence[str] | None = None) -> int:
    """``python -m nexus.testing.mock_llm``: serve until SIGTERM/SIGINT."""
    p = argparse.ArgumentParser(
        prog="python -m nexus.testing.mock_llm",
        description="OpenAI/Anthropic-compatible mock model server (deterministic; never a real model).",
    )
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", type=int, default=8089)
    p.add_argument("--rate-limit-every", type=int, default=0, help="Every Nth completion gets 429.")
    p.add_argument("--retry-after", type=float, default=1.0, help="Retry-After seconds on 429/503.")
    p.add_argument("--fail-first", type=int, default=0, help="The first N completions get 503.")
    p.add_argument("--verbose", action="store_true", help="Log request lines (never headers).")
    args = p.parse_args(argv)
    server = MockLLMServer(
        args.host, args.port, rate_limit_every=args.rate_limit_every,
        retry_after_s=args.retry_after, fail_first=args.fail_first, verbose=args.verbose,
    )

    def _term(_signum: int, _frame: object) -> None:
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, _term)
    print(json.dumps({"msg": "mock llm listening", "base_url": server.base_url,
                      "host": args.host, "port": server.port}), flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.stop()
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
