"""__WORKFLOW_NAME__ -- fine-tune a model, gate it on held-out data, register it.

    prepare_data --> validate_data --> submit_job --> wait_for_job --> evaluate --> eval_gate --> register

  * prepare_data   reads chat examples (JSONL, or the built-in arithmetic set)
                   and splits them into train and held-out by a hash of the
                   prompt, so the split is stable and duplicates never straddle it.
  * validate_data  schema check, de-duplication, train/held-out leakage removal
                   and a training-token budget. Too many bad rows, too few left,
                   or an over-budget job fails here -- before anything is paid for.
  * submit_job     uploads the training file and creates the job through a
                   provider-agnostic `FineTuneProvider`. IDEMPOTENT: each step is
                   journalled under the node's idempotency key, so a retry after
                   a crash re-uses the uploaded file and the created job instead
                   of paying for a second one.
  * wait_for_job   polls the job (bounded by `max_polls`), heartbeating each
                   poll and checkpointing the poll count, so a retry resumes the
                   same job with the remaining budget.
  * evaluate       answers the held-out prompts with the base model and the
                   fine-tuned model (the GPU step when generated with --gpu).
  * eval_gate      FAILS THE RUN unless the fine-tuned model reaches
                   `min_accuracy` and beats the base model by `min_improvement`.
  * register       writes the model record to `registry_dir` with a
                   create-if-absent rename, so a re-run never registers twice.

The defaults point at the mock model server (`python -m nexus.testing.mock_llm`)
at http://mock-llm:8089, whose OpenAI-style fine-tuning endpoints advance a job
validating_files -> queued -> running -> succeeded one step per poll, and whose
`mock-small` gets ~30% of arithmetic wrong while its fine-tunes answer correctly.

    pytest
    nexusctl workflow submit dag.py
    nexusctl run create __WORKFLOW_NAME__ --wait
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import re
import time
import urllib.error
import urllib.request
import uuid
from typing import Any, Protocol

from nexus import Gpu, RateLimited, Resources, Retry, Runtime, SecretRef, TaskContext, Timeouts, task, workflow
from nexus.llm import LLMClient

IMAGE = "__WORKFLOW_NAME__:0.1.0"

# Request a GPU for the evaluation step (where a self-hosted model would serve
# the held-out prompts). Off by default: with a hosted fine-tuning API nothing
# here needs one. Generate with `nexusctl init fine-tune --gpu` to start with it on.
USE_GPU = __USE_GPU__
GPU_KIND = "nvidia-a10"

# Sent when `api_key_env` is empty: the mock accepts any non-empty key, and a
# real provider rejects this one (401 -> a terminal error).
MOCK_API_KEY = "mock-key-not-a-secret"

# The provider key as a SECRET REFERENCE, resolved by the worker into the
# environment variable `name` (pass that name as api_key_env). Empty for the mock.
#   API_KEY_SECRETS = [SecretRef(name="OPENAI_API_KEY", ref="vault://kv/llm/openai")]
API_KEY_SECRETS: list[SecretRef] = []

# Validation: more invalid rows than this fraction means the export is broken,
# not noisy; fail instead of training on what is left.
MAX_INVALID_FRACTION = 0.05
MIN_TRAIN_EXAMPLES = 10


def _task(name: str, **kwargs: Any):
    ref = f"__WORKFLOW_NAME__.{name}"
    return task(
        task_ref=ref,
        runtime=Runtime(
            executor="docker",
            image=IMAGE,
            command=["python", "-m", "nexus.entrypoint", "--module", "dag"],
            args=[ref],
            secrets=API_KEY_SECRETS or None,
        ),
        **kwargs,
    )


def _api_key(api_key_env: str) -> str:
    if not api_key_env:
        return MOCK_API_KEY
    key = os.environ.get(api_key_env)
    if not key:
        raise PermissionError(f"environment variable {api_key_env} is not set; supply the key as a secret")
    return key


def make_client(ctx: TaskContext, model: str, base_url: str, api_key_env: str) -> LLMClient:
    return LLMClient(ctx, provider="openai", model=model, base_url=base_url, api_key=_api_key(api_key_env))


# ── fine-tuning providers ────────────────────────────────────────────────────


class FineTuneProvider(Protocol):
    """What the workflow needs from a fine-tuning backend. Implement it for a
    self-hosted trainer (and generate with --gpu) or another vendor's API."""

    name: str

    def upload(self, filename: str, jsonl: str) -> str:
        """Store a training file; return its id."""
        ...

    def create_job(self, base_model: str, training_file: str, *, n_epochs: int, suffix: str) -> str:
        """Start a job; return its id."""
        ...

    def get_job(self, job_id: str) -> dict:
        """{"status": "validating_files"|"queued"|"running"|"succeeded"|"failed"|"cancelled",
        "fine_tuned_model": str|None, "trained_tokens": int|None, "error": ...}"""
        ...


class OpenAICompatibleFineTuneProvider:
    """OpenAI's fine-tuning API (`/v1/files`, `/v1/fine_tuning/jobs`), stdlib only.

    `upload="multipart"` is what api.openai.com takes; `upload="json"` is the
    mock server's JSON form of the same upload.
    """

    name = "openai-compatible"

    def __init__(self, base_url: str, api_key: str, *, upload: str = "json", timeout_s: float = 60.0) -> None:
        if upload not in ("json", "multipart"):
            raise ValueError("upload must be 'json' or 'multipart'")
        self.base_url = base_url.rstrip("/")
        self._api_key = api_key
        self.upload_format = upload
        self.timeout_s = timeout_s

    def __repr__(self) -> str:  # never the key
        return f"OpenAICompatibleFineTuneProvider(base_url={self.base_url!r}, upload={self.upload_format!r})"

    def _call(self, method: str, path: str, body: bytes | None = None, content_type: str = "application/json") -> dict:
        headers = {"Authorization": f"Bearer {self._api_key}", "Accept": "application/json"}
        if body is not None:
            headers["Content-Type"] = content_type
        req = urllib.request.Request(self.base_url + path, data=body, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout_s) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            text = e.read()[:300].decode("utf-8", "replace")
            if e.code == 429:
                raise RateLimited(f"{method} {path}: HTTP 429: {text}",
                                  retry_after_s=float(e.headers.get("Retry-After") or 30)) from None
            if e.code in (401, 403):
                raise PermissionError(f"{method} {path}: HTTP {e.code}: {text}") from None
            if e.code >= 500:
                raise ConnectionError(f"{method} {path}: HTTP {e.code}: {text}") from None
            raise ValueError(f"{method} {path}: HTTP {e.code}: {text}") from None
        except (urllib.error.URLError, TimeoutError) as e:
            raise ConnectionError(f"{method} {path}: {e}") from None

    def upload(self, filename: str, jsonl: str) -> str:
        if self.upload_format == "json":
            body = json.dumps({"purpose": "fine-tune", "filename": filename, "content": jsonl}).encode()
            return str(self._call("POST", "/v1/files", body)["id"])
        boundary = uuid.uuid4().hex
        parts = [
            f'--{boundary}\r\nContent-Disposition: form-data; name="purpose"\r\n\r\nfine-tune\r\n'.encode(),
            (f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="{filename}"\r\n'
             "Content-Type: application/jsonl\r\n\r\n").encode() + jsonl.encode() + b"\r\n",
            f"--{boundary}--\r\n".encode(),
        ]
        return str(self._call("POST", "/v1/files", b"".join(parts),
                              f"multipart/form-data; boundary={boundary}")["id"])

    def create_job(self, base_model: str, training_file: str, *, n_epochs: int, suffix: str) -> str:
        body = json.dumps({"model": base_model, "training_file": training_file, "suffix": suffix,
                           "hyperparameters": {"n_epochs": n_epochs}}).encode()
        return str(self._call("POST", "/v1/fine_tuning/jobs", body)["id"])

    def get_job(self, job_id: str) -> dict:
        return self._call("GET", f"/v1/fine_tuning/jobs/{job_id}")


class MockFineTuneProvider:
    """An in-memory provider for unit tests. Its state lives in this PROCESS, so
    it only works when every task runs in one process (tests, or the local
    runner with isolation="inprocess"); use the mock server otherwise."""

    name = "in-memory"
    _files: dict[str, str] = {}
    _jobs: dict[str, dict] = {}
    calls: dict[str, int] = {"upload": 0, "create_job": 0, "get_job": 0}
    STATUSES = ("validating_files", "queued", "running", "succeeded")

    @classmethod
    def reset(cls) -> None:
        cls._files.clear()
        cls._jobs.clear()
        cls.calls.update({k: 0 for k in cls.calls})

    def upload(self, filename: str, jsonl: str) -> str:
        self.calls["upload"] += 1
        file_id = f"file-mem-{len(self._files) + 1}"
        self._files[file_id] = jsonl
        return file_id

    def create_job(self, base_model: str, training_file: str, *, n_epochs: int, suffix: str) -> str:
        self.calls["create_job"] += 1
        if training_file not in self._files:
            raise ValueError(f"training_file {training_file!r} not found")
        job_id = f"ftjob-mem{len(self._jobs) + 1}"
        self._jobs[job_id] = {"id": job_id, "model": base_model, "status": self.STATUSES[0], "polls": 0,
                              "fine_tuned_model": None, "trained_tokens": None, "error": None,
                              "n_epochs": n_epochs, "training_file": training_file}
        return job_id

    def get_job(self, job_id: str) -> dict:
        self.calls["get_job"] += 1
        job = self._jobs[job_id]
        if job["status"] != "succeeded":
            job["polls"] += 1
            job["status"] = self.STATUSES[min(job["polls"], len(self.STATUSES) - 1)]
            if job["status"] == "succeeded":
                job["fine_tuned_model"] = f"ft:{job['model']}:mem:{job_id}"
                job["trained_tokens"] = len(self._files[job["training_file"]].split()) * job["n_epochs"]
        return dict(job)


def make_provider(provider_kind: str, base_url: str, api_key_env: str) -> FineTuneProvider:
    if provider_kind == "mock-server":
        return OpenAICompatibleFineTuneProvider(base_url, _api_key(api_key_env), upload="json")
    if provider_kind == "openai":
        return OpenAICompatibleFineTuneProvider(base_url, _api_key(api_key_env), upload="multipart")
    if provider_kind == "in-memory":
        return MockFineTuneProvider()
    raise ValueError(f"provider_kind must be mock-server, openai or in-memory, got {provider_kind!r}")


# ── data ─────────────────────────────────────────────────────────────────────


def builtin_examples() -> list[dict]:
    """150 arithmetic chat examples, plus three duplicates and one broken row,
    so validation has something to do. Deterministic."""
    out = []
    for i in range(150):
        a, b, op = i + 2, (i * 17 + 3) % 30 + 1, "+-*"[i % 3]  # unique: a differs per row
        ans = a + b if op == "+" else a - b if op == "-" else a * b
        out.append({"messages": [{"role": "user", "content": f"What is {a} {op} {b}?"},
                                 {"role": "assistant", "content": str(ans)}]})
    return out + [out[3], out[40], out[41], {"messages": [{"role": "user", "content": "What is 1 + 1?"}]}]


def _prompt(example: dict) -> str:
    msgs = example.get("messages") if isinstance(example, dict) else None
    if not isinstance(msgs, list):
        return ""
    return " ".join(str(m.get("content", "")) for m in msgs if isinstance(m, dict) and m.get("role") == "user")


def _in_holdout(example: dict, fraction: float) -> bool:
    h = int(hashlib.sha256(_prompt(example).strip().lower().encode()).hexdigest()[:8], 16)
    return (h % 10_000) < fraction * 10_000


def _estimate_tokens(example: dict) -> int:
    chars = sum(len(str(m.get("content", ""))) for m in example["messages"])
    return math.ceil(chars / 4) + 4 * len(example["messages"])


def _valid(example: Any) -> str | None:
    """None if `example` is a well-formed chat example, else why not."""
    if not isinstance(example, dict) or not isinstance(example.get("messages"), list) or not example["messages"]:
        return "no messages list"
    for m in example["messages"]:
        if not isinstance(m, dict) or m.get("role") not in ("system", "user", "assistant"):
            return "a message has no valid role"
        if not isinstance(m.get("content"), str) or not m["content"].strip():
            return "a message has empty content"
    if example["messages"][-1]["role"] != "assistant":
        return "the last message is not the assistant's"
    if not any(m["role"] == "user" for m in example["messages"]):
        return "no user message"
    return None


# ── tasks ────────────────────────────────────────────────────────────────────

_SMALL = Resources(cpu="250m", memory="256Mi")
_NET_RETRY = Retry(max_attempts=4, initial="30s", max="10m",
                   retry_on=["rate_limited", "transient", "worker_lost", "infrastructure", "timeout"])


@_task("prepare_data", resources=_SMALL, timeouts=Timeouts(start_to_close="30m", heartbeat="5m"),
       retry=Retry(max_attempts=3, retry_on=["transient", "infrastructure"]))
def prepare_data(ctx: TaskContext, dataset: str, holdout_fraction: float) -> dict:
    """Read the examples and split them into train / held-out."""
    if not 0 < holdout_fraction < 1:
        raise ValueError("holdout_fraction must be in (0, 1)")
    if dataset == "builtin://arithmetic":
        rows: list[Any] = builtin_examples()
    else:
        path = dataset[len("file://"):] if dataset.startswith("file://") else dataset
        rows = []
        with open(path) as f:
            for line in f:
                if line.strip():
                    try:
                        row = json.loads(line)
                    except ValueError:
                        row = {"unparseable": line[:80]}
                    # Accept the prompt/completion form too.
                    if isinstance(row, dict) and "prompt" in row and "completion" in row and "messages" not in row:
                        row = {"messages": [{"role": "user", "content": str(row["prompt"])},
                                            {"role": "assistant", "content": str(row["completion"])}]}
                    rows.append(row)
    train = [r for r in rows if not _in_holdout(r, holdout_fraction)]
    holdout = [r for r in rows if _in_holdout(r, holdout_fraction)]
    ctx.log.info("prepared", rows=len(rows), train=len(train), holdout=len(holdout))
    return {"train": train, "holdout": holdout}


@_task("validate_data", resources=_SMALL, timeouts=Timeouts(start_to_close="30m", heartbeat="5m"),
       retry=Retry(max_attempts=1))
def validate_data(ctx: TaskContext, prepared: dict, max_train_tokens: int, n_epochs: int) -> dict:
    """Schema, de-duplication, leakage and the token budget. Raises on a bad export."""
    report: dict[str, Any] = {"invalid": {}, "duplicates": 0, "leaked_from_holdout": 0}
    clean: dict[str, list] = {"train": [], "holdout": []}
    seen: set[str] = set()
    total = 0
    for split in ("train", "holdout"):
        for ex in prepared[split]:
            total += 1
            why = _valid(ex)
            if why:
                report["invalid"][why] = report["invalid"].get(why, 0) + 1
                continue
            key = json.dumps([[m["role"], " ".join(m["content"].lower().split())] for m in ex["messages"]])
            if key in seen:
                report["duplicates"] += 1
                continue
            seen.add(key)
            clean[split].append({"messages": ex["messages"]})
    # The split is by prompt hash, so a prompt is never on both sides unless its
    # answers differ; drop such held-out rows rather than grade on trained data.
    train_prompts = {_prompt(e).strip().lower() for e in clean["train"]}
    kept = [e for e in clean["holdout"] if _prompt(e).strip().lower() not in train_prompts]
    report["leaked_from_holdout"] = len(clean["holdout"]) - len(kept)
    clean["holdout"] = kept

    invalid = sum(report["invalid"].values())
    if total == 0 or invalid / total > MAX_INVALID_FRACTION:
        raise ValueError(f"{invalid} of {total} rows are invalid (over {MAX_INVALID_FRACTION:.0%}): "
                         f"{report['invalid']}; fix the export")
    if len(clean["train"]) < MIN_TRAIN_EXAMPLES or not clean["holdout"]:
        raise ValueError(f"{len(clean['train'])} training and {len(clean['holdout'])} held-out examples "
                         f"left; need at least {MIN_TRAIN_EXAMPLES} and 1")
    train_tokens = sum(_estimate_tokens(e) for e in clean["train"]) * n_epochs
    if train_tokens > max_train_tokens:
        raise ValueError(f"the job would train on ~{train_tokens} tokens ({n_epochs} epochs), over "
                         f"max_train_tokens={max_train_tokens}")
    report.update(train=len(clean["train"]), holdout=len(clean["holdout"]), estimated_train_tokens=train_tokens)
    ctx.log.info("validated", **{k: v for k, v in report.items() if k != "invalid"}, invalid=invalid)
    return {**clean, "report": report}


@_task("submit_job", resources=_SMALL, timeouts=Timeouts(start_to_close="30m", heartbeat="5m"),
       retry=_NET_RETRY)
def submit_job(ctx: TaskContext, data: dict, base_model: str, provider_kind: str, base_url: str,
               api_key_env: str, n_epochs: int) -> dict:
    """Upload and create the job, each at most once per node (the effect journal)."""
    if ctx.effect_recorded("job"):
        job = ctx.previous_result("job")
        ctx.log.info("job already submitted by an earlier attempt", job_id=job["job_id"])
        return job
    provider = make_provider(provider_kind, base_url, api_key_env)
    # The idempotency key is the same for every attempt of this node, so it
    # names the upload and tags the job: a human can find a duplicate if the
    # write-then-crash window below is ever hit.
    tag = ctx.idempotency_key[:16]
    if ctx.effect_recorded("upload"):
        file_id = ctx.previous_result("upload")
    else:
        jsonl = "".join(json.dumps(e, sort_keys=True) + "\n" for e in data["train"])
        file_id = provider.upload(f"{tag}.jsonl", jsonl)
        ctx.record_effect("upload", file_id)  # immediately after the write, never before
    job_id = provider.create_job(base_model, file_id, n_epochs=n_epochs, suffix=tag)
    job = {"job_id": job_id, "training_file": file_id, "base_model": base_model, "provider": provider.name}
    ctx.record_effect("job", job)
    ctx.log.info("job submitted", **job)
    return job


class FineTuneFailed(ValueError):
    """The provider reports the job failed or was cancelled. Terminal."""


@_task("wait_for_job", resources=_SMALL, timeouts=Timeouts(start_to_close="12h", schedule_to_close="40h", heartbeat="10m"),
       retry=_NET_RETRY)
def wait_for_job(ctx: TaskContext, job: dict, provider_kind: str, base_url: str, api_key_env: str,
                 poll_interval_s: float, max_polls: int) -> dict:
    """Poll until the job ends, at most max_polls times across all attempts."""
    provider = make_provider(provider_kind, base_url, api_key_env)
    state = ctx.resume()
    if not state or state.get("job_id") != job["job_id"]:
        state = {"job_id": job["job_id"], "polls": 0, "status": None}
    elif state["polls"]:
        ctx.log.info("resuming polling", job_id=job["job_id"], polls=state["polls"], status=state["status"])
    while state["polls"] < max_polls:
        info = provider.get_job(job["job_id"])
        state["polls"] += 1
        state["status"] = info["status"]
        ctx.heartbeat(f"job {job['job_id']}: {info['status']} (poll {state['polls']}/{max_polls})")
        ctx.checkpoint(state)
        if info["status"] == "succeeded":
            return {**job, "fine_tuned_model": info["fine_tuned_model"],
                    "trained_tokens": info.get("trained_tokens"), "polls": state["polls"]}
        if info["status"] in ("failed", "cancelled"):
            raise FineTuneFailed(f"fine-tuning job {job['job_id']} {info['status']}: {info.get('error')}")
        time.sleep(poll_interval_s)
    raise TimeoutError(f"fine-tuning job {job['job_id']} still {state['status']} after {max_polls} polls")


def _accuracy(ctx: TaskContext, model: str, holdout: list, base_url: str, api_key_env: str,
              max_tokens: int) -> float:
    llm = make_client(ctx, model, base_url, api_key_env)
    correct = 0
    for ex in holdout:
        prompt_msgs = ex["messages"][:-1]
        want = ex["messages"][-1]["content"]
        got = llm.complete(prompt_msgs, max_tokens=max_tokens).text
        correct += " ".join(got.lower().split()).rstrip(".") == " ".join(want.lower().split()).rstrip(".")
    return round(correct / len(holdout), 6)


_EVAL_RESOURCES = (
    Resources(cpu="2", memory="8Gi", gpu=Gpu(count=1, kind=GPU_KIND))
    if USE_GPU
    else Resources(cpu="500m", memory="512Mi")
)


@_task("evaluate", resources=_EVAL_RESOURCES, timeouts=Timeouts(start_to_close="2h", heartbeat="10m"),
       retry=_NET_RETRY)
def evaluate(ctx: TaskContext, tuned: dict, data: dict, base_url: str, api_key_env: str, max_tokens: int) -> dict:
    """Held-out accuracy of the base model and of the fine-tuned model."""
    if USE_GPU:
        ctx.log.info("gpu assignment", cuda_visible_devices=os.environ.get("CUDA_VISIBLE_DEVICES", ""))
    holdout = data["holdout"]
    base = _accuracy(ctx, tuned["base_model"], holdout, base_url, api_key_env, max_tokens)
    ft = _accuracy(ctx, tuned["fine_tuned_model"], holdout, base_url, api_key_env, max_tokens)
    ctx.log.info("evaluated", base_accuracy=base, tuned_accuracy=ft, holdout=len(holdout))
    return {"base_model": tuned["base_model"], "fine_tuned_model": tuned["fine_tuned_model"],
            "holdout": len(holdout), "base_accuracy": base, "tuned_accuracy": ft,
            "improvement": round(ft - base, 6), "usage": ctx.usage_totals}


class EvalGateFailed(ValueError):
    """The fine-tuned model is not good enough to register. Terminal."""


@_task("eval_gate", resources=Resources(cpu="100m", memory="128Mi"), timeouts=Timeouts(start_to_close="5m"),
       retry=Retry(max_attempts=1))
def eval_gate(ctx: TaskContext, evaluation: dict, min_accuracy: float, min_improvement: float) -> dict:
    """Fail the run unless the fine-tune is good AND better than its base."""
    problems = []
    if evaluation["tuned_accuracy"] < min_accuracy:
        problems.append(f"held-out accuracy {evaluation['tuned_accuracy']:.3f} < min_accuracy={min_accuracy}")
    if evaluation["improvement"] < min_improvement:
        problems.append(f"improvement over {evaluation['base_model']} is {evaluation['improvement']:+.3f}, "
                        f"below min_improvement={min_improvement}")
    if problems:
        raise EvalGateFailed(f"{evaluation['fine_tuned_model']} not registered: " + "; ".join(problems))
    return {"passed": True, **{k: evaluation[k] for k in ("base_accuracy", "tuned_accuracy", "improvement")}}


@_task("register", resources=_SMALL, timeouts=Timeouts(start_to_close="10m"),
       retry=Retry(max_attempts=3, retry_on=["transient", "infrastructure"]))
def register(ctx: TaskContext, tuned: dict, evaluation: dict, gate: dict, registry_dir: str) -> dict:
    """Record the model in the registry directory, exactly once per model.

    A conditional write: the record is written to a temporary file and hard-
    linked to its final name, which fails if the name exists. So concurrent or
    repeated registrations of one model produce one record, never two.
    """
    os.makedirs(registry_dir, exist_ok=True)
    name = re.sub(r"[^A-Za-z0-9._-]", "_", tuned["fine_tuned_model"])
    final = os.path.join(registry_dir, f"{name}.json")
    record = {
        "model": tuned["fine_tuned_model"], "base_model": tuned["base_model"], "job_id": tuned["job_id"],
        "provider": tuned["provider"], "trained_tokens": tuned.get("trained_tokens"),
        "metrics": {k: evaluation[k] for k in ("holdout", "base_accuracy", "tuned_accuracy", "improvement")},
        "gate": gate, "workflow": "__WORKFLOW_NAME__", "run_id": ctx.run_id,
    }
    tmp = f"{final}.{uuid.uuid4().hex}.tmp"
    with open(tmp, "w") as f:
        json.dump(record, f, sort_keys=True, indent=2)
    try:
        os.link(tmp, final)
        created = True
    except FileExistsError:
        created = False
    finally:
        os.unlink(tmp)
    with open(final) as f:
        stored = json.load(f)
    if stored["job_id"] != record["job_id"]:
        raise ValueError(f"{final} already registers {stored['model']} from job {stored['job_id']}")
    ctx.log.info("registered" if created else "already registered", model=stored["model"], path=final)
    return {**stored, "path": final, "created": created}


# ── workflow ─────────────────────────────────────────────────────────────────


@workflow(
    name="__WORKFLOW_NAME__",
    namespace="__NAMESPACE__",
    description="Validate data, fine-tune idempotently, gate on held-out accuracy vs the base model, register.",
    params={
        "dataset": {"type": "string", "required": False, "default": "builtin://arithmetic"},
        "holdout_fraction": {"type": "float", "required": False, "default": 0.2},
        "base_model": {"type": "string", "required": False, "default": "mock-small"},
        # mock-server | openai | in-memory (unit tests only)
        "provider_kind": {"type": "string", "required": False, "default": "mock-server"},
        "base_url": {"type": "string", "required": False, "default": "http://mock-llm:8089"},
        "api_key_env": {"type": "string", "required": False, "default": ""},
        "n_epochs": {"type": "int", "required": False, "default": 3},
        "max_train_tokens": {"type": "int", "required": False, "default": 2000000},
        "poll_interval_s": {"type": "float", "required": False, "default": 10.0},
        "max_polls": {"type": "int", "required": False, "default": 720},
        "max_tokens": {"type": "int", "required": False, "default": 16},
        "min_accuracy": {"type": "float", "required": False, "default": 0.9},
        "min_improvement": {"type": "float", "required": False, "default": 0.05},
        "registry_dir": {"type": "string", "required": False, "default": "/data/model-registry"},
    },
    run_timeout="48h",
)
def fine_tune(dataset: str, holdout_fraction: float, base_model: str, provider_kind: str, base_url: str,
              api_key_env: str, n_epochs: int, max_train_tokens: int, poll_interval_s: float, max_polls: int,
              max_tokens: int, min_accuracy: float, min_improvement: float, registry_dir: str):
    prepared = prepare_data(dataset=dataset, holdout_fraction=holdout_fraction)
    data = validate_data(prepared=prepared, max_train_tokens=max_train_tokens, n_epochs=n_epochs)
    job = submit_job(data=data, base_model=base_model, provider_kind=provider_kind, base_url=base_url,
                     api_key_env=api_key_env, n_epochs=n_epochs)
    tuned = wait_for_job(job=job, provider_kind=provider_kind, base_url=base_url, api_key_env=api_key_env,
                         poll_interval_s=poll_interval_s, max_polls=max_polls)
    evaluation = evaluate(tuned=tuned, data=data, base_url=base_url, api_key_env=api_key_env,
                          max_tokens=max_tokens)
    gate = eval_gate(evaluation=evaluation, min_accuracy=min_accuracy, min_improvement=min_improvement)
    return register(tuned=tuned, evaluation=evaluation, gate=gate, registry_dir=registry_dir)


if __name__ == "__main__":
    compiled = fine_tune.compile()
    print(compiled.to_json(indent=2))
    print(f"\nspec hash: {compiled.hash}")
