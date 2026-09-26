# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""nexus.testing.MockLLMServer: the responder, auth, throttling, the
fine-tuning lifecycle and the CLI."""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import urllib.error
import urllib.request
from typing import Any

import pytest

from nexus.testing import MockLLMServer, respond
from nexus.testing.mock_llm import count_tokens

SDK_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def call(mock: MockLLMServer, method: str, path: str, body: Any = None, *, key: str | None = "k",
         headers: dict[str, str] | None = None, raw: bytes | None = None) -> tuple[int, Any]:
    hdrs = {"Content-Type": "application/json", **(headers or {})}
    if key is not None:
        hdrs["Authorization"] = f"Bearer {key}"
    data = raw if raw is not None else (None if body is None else json.dumps(body).encode())
    req = urllib.request.Request(mock.base_url + path, data=data, headers=hdrs, method=method)
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read())


# ── responder ─────────────────────────────────────────────────────────────────

def test_arithmetic():
    assert respond("What is 12 + 7?") == "19"
    assert respond("what is 3 x 4") == "12"
    assert respond("What is 10 - 15?") == "-5"
    assert respond("What is 7 / 2?") == "3.5"
    assert respond("What is 1 / 0?") == "undefined"
    assert respond("What is 2.5 * 2?") == "5"


def test_small_model_is_wrong_about_thirty_percent_deterministically():
    questions = [f"What is {a} + {b}?" for a in range(10) for b in range(10)]
    right = [str(a + b) for a in range(10) for b in range(10)]
    small = [respond(q, model="mock-small") for q in questions]
    wrong = sum(1 for s, r in zip(small, right) if s != r)
    assert 15 <= wrong <= 45
    assert small == [respond(q, model="mock-small") for q in questions], "deterministic"
    assert [respond(q, model="ft:mock-small:mock:ftjob-1") for q in questions] == right
    assert [respond(q, model="mock-large") for q in questions] == right


def test_sentiment_grader_and_summary():
    ask = "Classify the sentiment of this review as positive, negative or neutral: "
    assert respond(ask + "I loved it, the food was great!") == "positive"
    assert respond(ask + "Terrible service and the worst soup.") == "negative"
    assert respond(ask + "It was a restaurant.") == "neutral"
    assert respond("Classify the sentiment\nawful, just awful") == "negative"
    assert respond("Classify the sentiment great") == "positive"
    sys_prompt = "You are a strict grader. Answer PASS or FAIL."
    assert respond("Reference: 19\nCandidate: 19.", system=sys_prompt) == "PASS"
    assert respond("Reference: 19\nCandidate: 20", system=sys_prompt) == "FAIL"
    assert respond("Reference: Paris", system=sys_prompt).startswith("Summary:")
    assert respond("a b c d e f g h i j") == "Summary: a b c d e f g h"
    assert respond("   ") == "Summary:"
    assert count_tokens("") == 1 and count_tokens("a b  c") == 3


# ── HTTP ──────────────────────────────────────────────────────────────────────

def test_auth_health_models_and_errors():
    with MockLLMServer() as mock:
        assert call(mock, "GET", "/healthz", key=None) == (200, {"status": "ok"})
        status, doc = call(mock, "POST", "/v1/chat/completions", {"model": "m", "messages": []}, key=None)
        assert status == 401 and doc["error"]["type"] == "authentication_error"
        status, doc = call(mock, "POST", "/v1/messages", {"model": "m"}, key=None,
                           headers={"x-api-key": "k"})
        assert status == 400 and doc["type"] == "error", "x-api-key authenticates Anthropic"
        status, doc = call(mock, "GET", "/v1/models")
        assert status == 200 and [m["id"] for m in doc["data"]] == ["mock-small", "mock-large"]
        assert call(mock, "GET", "/v1/nothing")[0] == 404
        assert call(mock, "POST", "/v1/chat/completions", raw=b"{bad")[0] == 400
        for bad in ([], {"messages": [{"role": "user", "content": "x"}]}, {"model": "m", "messages": "x"},
                    {"model": "m", "messages": ["x"]},
                    {"model": "m", "messages": [{"role": "user", "content": "x"}], "max_tokens": 0}):
            assert call(mock, "POST", "/v1/chat/completions", bad)[0] == 400
        status, doc = call(mock, "POST", "/v1/messages",
                           {"model": "m", "messages": [{"role": "user", "content": "x"}]})
        assert status == 400 and "max_tokens" in doc["error"]["message"]
        # Content blocks are accepted, as upstream.
        status, doc = call(mock, "POST", "/v1/messages", {
            "model": "m", "max_tokens": 5, "system": [{"type": "text", "text": "hi"}],
            "messages": [{"role": "user", "content": [{"type": "text", "text": "What is 2 + 2?"}]}],
        })
        assert status == 200 and doc["content"][0]["text"] == "4"
        assert doc["usage"] == {"input_tokens": 6, "output_tokens": 1}
        stats = mock.stats
        assert stats.unauthorized == 1 and stats.requests["/healthz"] == 1
        mock.reset_stats()
        assert mock.stats.total_requests == 0


def test_throttling_knobs():
    with MockLLMServer(rate_limit_every=3, fail_first=1, retry_after_s=2) as mock:
        body = {"model": "m", "messages": [{"role": "user", "content": "hi"}]}
        statuses = []
        for _ in range(7):
            req = urllib.request.Request(mock.base_url + "/v1/chat/completions",
                                         data=json.dumps(body).encode(),
                                         headers={"Authorization": "Bearer k"}, method="POST")
            try:
                with urllib.request.urlopen(req, timeout=5) as r:
                    statuses.append(r.status)
            except urllib.error.HTTPError as e:
                statuses.append(e.code)
                assert e.headers["Retry-After"] == "2"
        assert statuses == [503, 200, 429, 200, 200, 429, 200]
        assert mock.stats.throttled == 2 and mock.stats.unavailable == 1
    with pytest.raises(ValueError):
        MockLLMServer(rate_limit_every=-1)


def test_fine_tuning_lifecycle():
    with MockLLMServer() as mock:
        content = '{"prompt": "What is 1 + 1?", "completion": "2"}\n{"prompt": "hi", "completion": "hello"}\n'
        status, f = call(mock, "POST", "/v1/files",
                         {"purpose": "fine-tune", "filename": "train.jsonl", "content": content})
        assert status == 200 and f["id"].startswith("file-") and f["bytes"] == len(content)
        assert call(mock, "POST", "/v1/files", {"purpose": "x"})[0] == 400
        status, doc = call(mock, "POST", "/v1/files", raw=b"--b\r\n",
                           headers={"Content-Type": "multipart/form-data; boundary=b"})
        assert status == 400 and "JSON" in doc["error"]["message"]

        assert call(mock, "POST", "/v1/fine_tuning/jobs", {"model": "mock-small",
                                                           "training_file": "file-nope"})[0] == 400
        assert call(mock, "POST", "/v1/fine_tuning/jobs", {"training_file": f["id"]})[0] == 400
        assert call(mock, "POST", "/v1/fine_tuning/jobs", ["x"])[0] == 400
        status, job = call(mock, "POST", "/v1/fine_tuning/jobs",
                           {"model": "mock-small", "training_file": f["id"],
                            "hyperparameters": {"n_epochs": 2}})
        assert status == 200 and job["status"] == "validating_files" and job["fine_tuned_model"] is None
        seen = [call(mock, "GET", f"/v1/fine_tuning/jobs/{job['id']}")[1] for _ in range(4)]
        assert [j["status"] for j in seen] == ["queued", "running", "succeeded", "succeeded"]
        done = seen[-1]
        assert done["fine_tuned_model"] == f"ft:mock-small:mock:{job['id']}"
        assert done["trained_tokens"] == count_tokens(content) * 2
        assert "_polls" not in done
        assert call(mock, "GET", "/v1/fine_tuning/jobs/ftjob-none")[0] == 404
        status, listing = call(mock, "GET", "/v1/fine_tuning/jobs")
        assert status == 200 and listing["data"][0]["id"] == job["id"]
        assert done["fine_tuned_model"] in mock.models()
        assert mock.stats.requests["/v1/fine_tuning/jobs/{id}"] == 5
        # The fine-tuned model answers correctly where its base does not.
        body = {"model": done["fine_tuned_model"], "max_tokens": 4,
                "messages": [{"role": "user", "content": "What is 3 + 9?"}]}
        assert call(mock, "POST", "/v1/chat/completions", body)[1]["choices"][0]["message"]["content"] == "12"


def test_cli_serves_until_sigterm():
    env = {**os.environ, "PYTHONPATH": SDK_ROOT}
    proc = subprocess.Popen(
        [sys.executable, "-m", "nexus.testing.mock_llm", "--port", "0", "--rate-limit-every", "2"],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env, text=True,
    )
    try:
        assert proc.stdout is not None
        line = json.loads(proc.stdout.readline())
        with urllib.request.urlopen(line["base_url"] + "/healthz", timeout=5) as r:
            assert json.loads(r.read()) == {"status": "ok"}
    finally:
        proc.send_signal(signal.SIGTERM)
        assert proc.wait(timeout=10) == 0
