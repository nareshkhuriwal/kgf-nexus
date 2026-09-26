"""Tests for the task functions. They run the real handlers against the SDK's
mock model server (nexus.testing.MockLLMServer) or the in-memory provider: no
platform, no paid API, no key."""

from __future__ import annotations

import json
import os

import pytest

import dag
from dag import (
    USE_GPU,
    EvalGateFailed,
    MockFineTuneProvider,
    builtin_examples,
    eval_gate,
    evaluate,
    fine_tune,
    prepare_data,
    register,
    submit_job,
    validate_data,
    wait_for_job,
)
from nexus import InMemoryResultStore, TaskContext, derive_idempotency_key
from nexus.testing import MockLLMServer

JOB_POLLS = "/v1/fine_tuning/jobs/{id}"


@pytest.fixture
def mock():
    with MockLLMServer() as server:
        yield server


@pytest.fixture(autouse=True)
def _fresh_in_memory_provider():
    MockFineTuneProvider.reset()


def _attempt(store, number, node="submit_job", cls=TaskContext):
    """Attempt `number` of one node: every attempt shares the store and the
    idempotency key, exactly as on the platform."""
    return cls(run_id="run-1", node_id=node, attempt_id=f"{node}-a{number}", attempt_number=number,
               task_ref=f"__WORKFLOW_NAME__.{node}", task_version=1,
               idempotency_key=derive_idempotency_key("run-1", node, None, 1), store=store)


def _data(make_ctx, max_train_tokens=2_000_000):
    prepared = prepare_data(make_ctx("prepare_data"), dataset="builtin://arithmetic", holdout_fraction=0.2)
    return validate_data(make_ctx("validate_data"), prepared=prepared, max_train_tokens=max_train_tokens,
                         n_epochs=3)


def test_validation_dedupes_and_drops_bad_rows(make_ctx):
    data = _data(make_ctx)
    report = data["report"]
    assert report["duplicates"] == 3 and sum(report["invalid"].values()) == 1
    assert report["train"] + report["holdout"] == 150 and report["holdout"] >= 15
    held = {json.dumps(e) for e in data["holdout"]}
    assert not held & {json.dumps(e) for e in data["train"]}


def test_validation_refuses_a_broken_export(make_ctx):
    rows = builtin_examples()[:20] + [{"messages": []}] * 5
    with pytest.raises(ValueError, match="invalid"):
        validate_data(make_ctx("validate_data"), prepared={"train": rows, "holdout": []},
                      max_train_tokens=10**9, n_epochs=1)


def test_validation_enforces_the_token_budget(make_ctx):
    with pytest.raises(ValueError, match="max_train_tokens=1000"):
        _data(make_ctx, max_train_tokens=1000)


def test_submission_is_idempotent_across_retries(make_ctx, mock):
    data = _data(make_ctx)
    store = InMemoryResultStore()
    args = dict(data=data, base_model="mock-small", provider_kind="mock-server", base_url=mock.base_url,
                api_key_env="", n_epochs=3)
    first = submit_job(_attempt(store, 1), **args)
    again = submit_job(_attempt(store, 2), **args)
    assert again == first and first["job_id"].startswith("ftjob-")
    assert mock.stats.requests["/v1/files"] == 1
    assert mock.stats.requests["/v1/fine_tuning/jobs"] == 1


def test_a_crash_after_the_upload_does_not_upload_again(make_ctx, monkeypatch):
    data = _data(make_ctx)
    store = InMemoryResultStore()
    args = dict(data=data, base_model="mock-small", provider_kind="in-memory", base_url="unused",
                api_key_env="", n_epochs=3)
    real = MockFineTuneProvider.create_job

    def flaky(self, *a, **kw):
        MockFineTuneProvider.calls["create_job"] += 1
        raise ConnectionError("provider hung up")

    monkeypatch.setattr(MockFineTuneProvider, "create_job", flaky)
    with pytest.raises(ConnectionError):
        submit_job(_attempt(store, 1), **args)
    monkeypatch.setattr(MockFineTuneProvider, "create_job", real)
    job = submit_job(_attempt(store, 2), **args)
    assert job["training_file"] == "file-mem-1"
    assert MockFineTuneProvider.calls["upload"] == 1 and MockFineTuneProvider.calls["create_job"] == 2
    assert len(MockFineTuneProvider._jobs) == 1


class _CrashAfterCheckpoints(TaskContext):
    def checkpoint(self, state):
        info = super().checkpoint(state)
        self.saved += 1
        if self.saved == self.crash_at:
            raise ConnectionError("simulated worker loss")
        return info


def test_polling_resumes_the_same_job_after_a_crash(make_ctx, mock):
    job = submit_job(make_ctx("submit_job"), data=_data(make_ctx), base_model="mock-small",
                     provider_kind="mock-server", base_url=mock.base_url, api_key_env="", n_epochs=3)
    store = InMemoryResultStore()
    args = dict(job=job, provider_kind="mock-server", base_url=mock.base_url, api_key_env="",
                poll_interval_s=0, max_polls=10)
    first = _attempt(store, 1, "wait_for_job", _CrashAfterCheckpoints)
    first.saved, first.crash_at = 0, 2
    with pytest.raises(ConnectionError):
        wait_for_job(first, **args)
    tuned = wait_for_job(_attempt(store, 2, "wait_for_job"), **args)
    # validating_files -> queued -> running -> succeeded: 3 polls in all, the
    # count carried across the crash by the checkpoint.
    assert tuned["polls"] == 3 and mock.stats.requests[JOB_POLLS] == 3
    assert tuned["fine_tuned_model"] == f"ft:mock-small:mock:{job['job_id']}"


def test_polling_is_bounded(make_ctx, mock):
    job = submit_job(make_ctx("submit_job"), data=_data(make_ctx), base_model="mock-small",
                     provider_kind="mock-server", base_url=mock.base_url, api_key_env="", n_epochs=3)
    with pytest.raises(TimeoutError, match="after 2 polls"):
        wait_for_job(make_ctx("wait_for_job"), job=job, provider_kind="mock-server", base_url=mock.base_url,
                     api_key_env="", poll_interval_s=0, max_polls=2)


def _pipeline(make_ctx, base_url, registry, *, base_model="mock-small", provider_kind="mock-server"):
    data = _data(make_ctx)
    job = submit_job(make_ctx("submit_job"), data=data, base_model=base_model, provider_kind=provider_kind,
                     base_url=base_url, api_key_env="", n_epochs=3)
    tuned = wait_for_job(make_ctx("wait_for_job"), job=job, provider_kind=provider_kind, base_url=base_url,
                         api_key_env="", poll_interval_s=0, max_polls=10)
    evaluation = evaluate(make_ctx("evaluate"), tuned=tuned, data=data, base_url=base_url, api_key_env="",
                          max_tokens=16)
    gate = eval_gate(make_ctx("eval_gate"), evaluation=evaluation, min_accuracy=0.9, min_improvement=0.05)
    return tuned, evaluation, register(make_ctx("register"), tuned=tuned, evaluation=evaluation, gate=gate,
                                       registry_dir=str(registry))


def test_end_to_end_against_the_mock_server(make_ctx, mock, tmp_path):
    tuned, evaluation, record = _pipeline(make_ctx, mock.base_url, tmp_path)
    # mock-small gets ~30% of arithmetic wrong; its fine-tune answers correctly.
    assert evaluation["tuned_accuracy"] == 1.0 and evaluation["base_accuracy"] < 0.9
    assert record["created"] and record["model"] == tuned["fine_tuned_model"]
    assert os.listdir(tmp_path) == [os.path.basename(record["path"])]
    # The fine-tuned id's ':' is mapped into the usage-key alphabet.
    assert any(k.startswith("tokens:openai/ft_mock-small_mock_") for k in evaluation["usage"])


def test_end_to_end_with_the_in_memory_provider(make_ctx, mock, tmp_path):
    _, evaluation, record = _pipeline(make_ctx, mock.base_url, tmp_path, provider_kind="in-memory")
    assert record["provider"] == "in-memory" and evaluation["improvement"] > 0


def test_the_gate_refuses_a_fine_tune_that_is_not_better(make_ctx, mock, tmp_path):
    with pytest.raises(EvalGateFailed, match="improvement over mock-large is \\+0.000"):
        _pipeline(make_ctx, mock.base_url, tmp_path, base_model="mock-large")
    assert not os.listdir(tmp_path)  # nothing registered


def test_registration_is_idempotent(make_ctx, tmp_path):
    tuned = {"fine_tuned_model": "ft:m:mock:j1", "base_model": "m", "job_id": "j1", "provider": "p"}
    evaluation = {"holdout": 3, "base_accuracy": 0.5, "tuned_accuracy": 1.0, "improvement": 0.5}
    first = register(make_ctx("register"), tuned=tuned, evaluation=evaluation, gate={}, registry_dir=str(tmp_path))
    second = register(make_ctx("register"), tuned=tuned, evaluation=evaluation, gate={}, registry_dir=str(tmp_path))
    assert first["created"] and not second["created"] and len(os.listdir(tmp_path)) == 1
    with pytest.raises(ValueError, match="already registers"):
        register(make_ctx("register"), tuned={**tuned, "job_id": "j2"}, evaluation=evaluation, gate={},
                 registry_dir=str(tmp_path))


def test_workflow_shape():
    spec = fine_tune.compile().spec
    ids = [t["id"] for t in spec["tasks"]]
    assert sorted(ids) == sorted(["prepare_data", "validate_data", "submit_job", "wait_for_job", "evaluate",
                                  "eval_gate", "register"])
    gpus = [t["id"] for t in spec["tasks"] if (t.get("resources") or {}).get("request", {}).get("gpu_count")]
    assert gpus == (["evaluate"] if USE_GPU else [])
    assert dag.MOCK_API_KEY not in json.dumps(spec)  # no key, not even the mock's, in the spec
