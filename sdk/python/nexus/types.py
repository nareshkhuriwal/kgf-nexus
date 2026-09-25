# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Author-facing spec types and their compilation to WorkflowSpec JSON.

Every type here has one job: accept human-friendly author input, validate it in
the USER's process, and emit the canonical wire form defined by
proto/nexus/v1/{common,task,workflow}.proto. Nothing here runs on the control
plane -- it parses the emitted document and never imports this module
(ADR-0004).

Enum values are emitted as the proto enum NAMES (e.g. "IDEMPOTENCY_MODE_REQUIRED")
rather than integers, because proto3 JSON accepts names and a spec that a human
can read in a code review is worth more than four saved bytes. The names here
are the canonical ones from CONTRACT.md; they are not free to change.
"""

from __future__ import annotations

import dataclasses
import json
from typing import Any, Iterable, Literal, Mapping, Sequence

from .units import parse_bytes, parse_cpu_millis, parse_duration_ms

__all__ = [
    "Gpu",
    "Resources",
    "Timeouts",
    "Retry",
    "Cache",
    "Runtime",
    "SecretRef",
    "Concurrency",
    "ValueSpec",
    "ValueSpecLike",
    "ErrorClass",
    "BackoffStrategy",
    "IdempotencyMode",
    "TriggerRule",
    "Jitter",
]

# ── Enum surfaces ─────────────────────────────────────────────────────────────
# Authors write the short, obvious word; we map it to the contract's canonical
# enum name. Accepting the canonical name too means a spec that was decompiled
# from the wire form recompiles unchanged.

ErrorClass = Literal[
    "transient",
    "timeout",
    "worker_lost",
    "validation",
    "validation_error",
    "permission_denied",
    "application",
    "oom",
    "not_found",
    "infrastructure",
]

_ERROR_CLASS_NAMES: dict[str, str] = {
    "transient": "ERROR_CLASS_TRANSIENT",
    "timeout": "ERROR_CLASS_TIMEOUT",
    "worker_lost": "ERROR_CLASS_WORKER_LOST",
    # The architecture's YAML sketch (§4.3) writes `validation_error` while the
    # proto enum is ERROR_CLASS_VALIDATION. Both author spellings are accepted
    # so neither document makes a user wrong.
    "validation": "ERROR_CLASS_VALIDATION",
    "validation_error": "ERROR_CLASS_VALIDATION",
    "permission_denied": "ERROR_CLASS_PERMISSION_DENIED",
    "application": "ERROR_CLASS_APPLICATION",
    "oom": "ERROR_CLASS_OOM",
    "not_found": "ERROR_CLASS_NOT_FOUND",
    "infrastructure": "ERROR_CLASS_INFRASTRUCTURE",
}

BackoffStrategy = Literal["fixed", "linear", "exponential"]
_BACKOFF_NAMES: dict[str, str] = {
    "fixed": "BACKOFF_STRATEGY_FIXED",
    "linear": "BACKOFF_STRATEGY_LINEAR",
    "exponential": "BACKOFF_STRATEGY_EXPONENTIAL",
}

IdempotencyMode = Literal["required", "best_effort", "none"]
_IDEMPOTENCY_NAMES: dict[str, str] = {
    "required": "IDEMPOTENCY_MODE_REQUIRED",
    "best_effort": "IDEMPOTENCY_MODE_BEST_EFFORT",
    "none": "IDEMPOTENCY_MODE_NONE",
}

TriggerRule = Literal[
    "all_success", "all_done", "one_success", "one_failed", "none_failed", "all_skipped"
]
_TRIGGER_NAMES: dict[str, str] = {
    "all_success": "TRIGGER_RULE_ALL_SUCCESS",
    "all_done": "TRIGGER_RULE_ALL_DONE",
    "one_success": "TRIGGER_RULE_ONE_SUCCESS",
    "one_failed": "TRIGGER_RULE_ONE_FAILED",
    "none_failed": "TRIGGER_RULE_NONE_FAILED",
    "all_skipped": "TRIGGER_RULE_ALL_SKIPPED",
}

Jitter = Literal["none", "full", "equal"]

# MVP supports only these two trigger rules (architecture §11 Phase 1). The
# others are in the proto so adding them later is not a breaking change, but
# emitting one now would produce a spec the Phase 1 DAG engine rejects -- better
# to say so at compile time, in the author's editor, than at submit time.
_MVP_TRIGGER_RULES = frozenset({"all_success", "all_done"})


class SpecError(ValueError):
    """An author error in a spec declaration, raised at decoration time.

    Raising at decoration time rather than submit time is the whole point of
    running the compiler in the user's process: the traceback points at the
    offending decorator.
    """


def _enum(table: Mapping[str, str], value: str, field: str) -> str:
    key = value.strip().lower()
    if key in table:
        return table[key]
    # Also accept the canonical wire name, so decompile -> recompile is stable.
    upper = value.strip().upper()
    if upper in set(table.values()):
        return upper
    raise SpecError(
        f"unknown {field} {value!r}; expected one of {', '.join(sorted(table))}"
    )


# ── Resources ─────────────────────────────────────────────────────────────────


@dataclasses.dataclass(frozen=True, slots=True)
class Gpu:
    """GPU requirement. `count` is what the resource-manager ledger decrements.

    `kind` and `memory` are optional placement constraints, not accounting
    dimensions: the scheduler filters candidate workers by them but the ledger
    only counts devices.
    """

    count: int = 1
    kind: str | None = None
    memory: str | int | None = None

    def __post_init__(self) -> None:
        if self.count < 0:
            raise SpecError(f"gpu.count must be non-negative, got {self.count}")


@dataclasses.dataclass(frozen=True, slots=True)
class Resources:
    """Scheduling request and enforcement limit, per architecture §4.3.

    `request` is subtracted from worker capacity by the scheduler; `*_limit` is
    what the executor enforces via cgroups. A limit above the request permits
    opportunistic bursting; omitting the limit means "same as request", which is
    the predictable-performance choice and the safer default for a platform that
    has to explain OOM kills to users.
    """

    cpu: str | int | float | None = None
    memory: str | int | None = None
    gpu: Gpu | int | None = None
    disk: str | int | None = None
    custom: Mapping[str, int] | None = None

    cpu_limit: str | int | float | None = None
    memory_limit: str | int | None = None
    disk_limit: str | int | None = None

    def to_wire(self) -> dict[str, Any]:
        """Emit a ResourceRequest with request and limit ResourceVectors."""
        gpu: Gpu | None
        if isinstance(self.gpu, bool):
            raise SpecError("resources.gpu must be a Gpu or a device count, not a bool")
        gpu = Gpu(count=self.gpu) if isinstance(self.gpu, int) else self.gpu

        request: dict[str, Any] = {}
        if self.cpu is not None:
            request["cpu_millis"] = parse_cpu_millis(self.cpu)
        if self.memory is not None:
            request["memory_bytes"] = parse_bytes(self.memory)
        if self.disk is not None:
            request["disk_bytes"] = parse_bytes(self.disk)
        if gpu is not None:
            if gpu.count:
                request["gpu_count"] = gpu.count
            if gpu.kind:
                request["gpu_kind"] = gpu.kind
            if gpu.memory is not None:
                request["gpu_memory_bytes"] = parse_bytes(gpu.memory)
        if self.custom:
            custom: dict[str, int] = {}
            for k, v in self.custom.items():
                if not isinstance(v, int) or isinstance(v, bool) or v < 0:
                    raise SpecError(
                        f"custom resource {k!r} must be a non-negative integer count, "
                        f"got {v!r}"
                    )
                custom[str(k)] = v
            request["custom"] = dict(sorted(custom.items()))

        limit: dict[str, Any] = {}
        if self.cpu_limit is not None:
            limit["cpu_millis"] = parse_cpu_millis(self.cpu_limit)
        if self.memory_limit is not None:
            limit["memory_bytes"] = parse_bytes(self.memory_limit)
        if self.disk_limit is not None:
            limit["disk_bytes"] = parse_bytes(self.disk_limit)
        # GPUs are not burstable -- you cannot have 1.5 of a device -- so the
        # limit mirrors the request rather than being independently settable.
        if gpu is not None and gpu.count:
            limit["gpu_count"] = gpu.count

        for dim, req_key in (("cpu", "cpu_millis"), ("memory", "memory_bytes"), ("disk", "disk_bytes")):
            if req_key in limit and req_key in request and limit[req_key] < request[req_key]:
                raise SpecError(
                    f"{dim}_limit ({limit[req_key]}) is below {dim} request "
                    f"({request[req_key]}); the executor would kill the task it was "
                    "scheduled to fit"
                )

        out: dict[str, Any] = {}
        if request:
            out["request"] = request
        if limit:
            out["limit"] = limit
        return out


# ── Timeouts ──────────────────────────────────────────────────────────────────


@dataclasses.dataclass(frozen=True, slots=True)
class Timeouts:
    """Temporal's four-timeout model. See architecture §4.3 for why four.

    Collapsing these into one loses the operational signal: schedule_to_start
    blowing up means fix the cluster, start_to_close means fix the code,
    heartbeat means fix the network or the task's progress reporting.
    """

    schedule_to_start: str | int | None = None
    start_to_close: str | int | None = None
    schedule_to_close: str | int | None = None
    heartbeat: str | int | None = None

    def to_wire(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        if self.schedule_to_start is not None:
            out["schedule_to_start_ms"] = parse_duration_ms(self.schedule_to_start)
        if self.start_to_close is not None:
            out["start_to_close_ms"] = parse_duration_ms(self.start_to_close)
        if self.schedule_to_close is not None:
            out["schedule_to_close_ms"] = parse_duration_ms(self.schedule_to_close)
        if self.heartbeat is not None:
            out["heartbeat_ms"] = parse_duration_ms(self.heartbeat)

        # schedule_to_close is the end-to-end SLA across all retries, so it must
        # leave room for at least one full attempt. A spec where it does not is
        # always an author mistake: the task would be killed by the run budget
        # before its own attempt timeout could ever fire.
        stc = out.get("schedule_to_close_ms")
        s2c = out.get("start_to_close_ms")
        if stc is not None and s2c is not None and stc < s2c:
            raise SpecError(
                f"schedule_to_close ({stc}ms) is shorter than start_to_close "
                f"({s2c}ms); a single attempt could never finish inside the budget"
            )
        if out.get("heartbeat_ms") is not None and s2c is not None and out["heartbeat_ms"] > s2c:
            raise SpecError(
                f"heartbeat ({out['heartbeat_ms']}ms) exceeds start_to_close "
                f"({s2c}ms); the heartbeat timeout could never fire"
            )
        return out

    @staticmethod
    def coerce(value: "Timeouts | Mapping[str, Any] | None") -> "Timeouts | None":
        """Accept the dict form used in the architecture's SDK sketch (§8.3)."""
        if value is None or isinstance(value, Timeouts):
            return value
        unknown = set(value) - {
            "schedule_to_start",
            "start_to_close",
            "schedule_to_close",
            "heartbeat",
        }
        if unknown:
            raise SpecError(
                f"unknown timeout(s) {', '.join(sorted(unknown))}; the four are "
                "schedule_to_start, start_to_close, schedule_to_close, heartbeat"
            )
        return Timeouts(**value)


# ── Retry ─────────────────────────────────────────────────────────────────────


@dataclasses.dataclass(frozen=True, slots=True)
class Retry:
    """Retry policy. Defaults to full jitter, which is not cosmetic.

    Synchronized backoff across a fan-out turns one downstream blip into a
    self-inflicted thundering herd; full jitter is the cheapest fix and so it is
    the default rather than an option.
    """

    max_attempts: int = 3
    backoff: BackoffStrategy = "exponential"
    initial: str | int = "10s"
    max: str | int = "10m"
    multiplier: float = 2.0
    jitter: Jitter = "full"
    retry_on: Sequence[ErrorClass] | None = None
    no_retry_on: Sequence[ErrorClass] | None = None
    retry_on_different_worker: bool = False

    def to_wire(self) -> dict[str, Any]:
        if self.max_attempts < 1:
            raise SpecError(
                f"retry.max_attempts must be >= 1 (1 means no retries), got "
                f"{self.max_attempts}"
            )
        if self.jitter not in ("none", "full", "equal"):
            raise SpecError(
                f"unknown jitter {self.jitter!r}; expected none, full, or equal"
            )
        if self.multiplier < 1.0:
            raise SpecError(
                f"retry.multiplier must be >= 1.0, got {self.multiplier}; a "
                "shrinking backoff retries harder the longer a dependency is down"
            )
        initial_ms = parse_duration_ms(self.initial)
        max_ms = parse_duration_ms(self.max)
        if max_ms < initial_ms:
            raise SpecError(
                f"retry.max ({max_ms}ms) is below retry.initial ({initial_ms}ms)"
            )

        out: dict[str, Any] = {
            "max_attempts": self.max_attempts,
            "strategy": _enum(_BACKOFF_NAMES, self.backoff, "retry.backoff"),
            "initial_ms": initial_ms,
            "max_ms": max_ms,
            "multiplier": self.multiplier,
            "jitter": self.jitter,
        }
        retry_on = _classes(self.retry_on, "retry.retry_on")
        no_retry_on = _classes(self.no_retry_on, "retry.no_retry_on")
        overlap = set(retry_on) & set(no_retry_on)
        if overlap:
            # Ambiguity here is not resolvable by the platform without guessing,
            # and guessing about whether to re-run work is exactly what we refuse
            # to do elsewhere (ADR-0006).
            raise SpecError(
                f"error class(es) {', '.join(sorted(overlap))} appear in both "
                "retry_on and no_retry_on"
            )
        if retry_on:
            out["retry_on"] = retry_on
        if no_retry_on:
            out["no_retry_on"] = no_retry_on
        if self.retry_on_different_worker:
            out["retry_on_different_worker"] = True
        return out

    @staticmethod
    def coerce(value: "Retry | Mapping[str, Any] | int | None") -> "Retry | None":
        if value is None or isinstance(value, Retry):
            return value
        if isinstance(value, bool):
            raise SpecError("retry must be a Retry, a max-attempts count or a mapping, not a bool")
        if isinstance(value, int):
            return Retry(max_attempts=value)
        return Retry(**value)


def _classes(values: Iterable[str] | None, field: str) -> list[str]:
    """Normalize and sort error classes; sorting keeps the spec canonical."""
    if not values:
        return []
    names = {_enum(_ERROR_CLASS_NAMES, v, field) for v in values}
    return sorted(names)


# ── Cache, runtime, concurrency ───────────────────────────────────────────────


@dataclasses.dataclass(frozen=True, slots=True)
class Cache:
    """Memoization policy (§6.4). Distinct from idempotency.

    Idempotency makes re-execution safe; memoization makes it unnecessary. Only
    valid for genuinely pure tasks, which is why it is opt-in.
    """

    enabled: bool = True
    ttl: str | int | None = None

    def to_wire(self) -> dict[str, Any]:
        out: dict[str, Any] = {"enabled": bool(self.enabled)}
        if self.ttl is not None:
            out["ttl_ms"] = parse_duration_ms(self.ttl)
        return out

    @staticmethod
    def coerce(value: "Cache | Mapping[str, Any] | bool | None") -> "Cache | None":
        if value is None or isinstance(value, Cache):
            return value
        if isinstance(value, bool):
            return Cache(enabled=value)
        if not isinstance(value, Mapping):
            raise SpecError(f"cache must be a Cache, a bool or a mapping, got {type(value).__name__}")
        return Cache(**value)


# The schema's absolute maximum for a declared bound (ADR-0015). Checked here too
# so an over-long bound fails at the decorator rather than at submit.
MAX_UNINTERRUPTIBLE_MS = 3_600_000


@dataclasses.dataclass(frozen=True, slots=True)
class Cancellation:
    """How EXTERNAL cancellation treats a running attempt (ADR-0015).

    ``uninterruptible=True`` declares a critical section that must not be torn
    down part way -- a non-idempotent charge, a schema step. Cancelling it
    (operator or run cancel, a revoked lease, an agent drain) sends NO SIGTERM:
    the worker waits for the task to finish on its own for up to ``max_wait``,
    clamped to the worker's platform ceiling, then SIGKILLs it. ``max_wait``
    omitted means "the ceiling".

    It is bounded on purpose, twice: by ``max_wait`` (at most one hour) and by the
    ceiling the operator configures on the worker. An unbounded "do not stop me"
    would be a worker the platform could never reclaim. It does NOT extend
    ``timeouts.start_to_close``, which remains the task's own hard limit.
    """

    uninterruptible: bool = True
    max_wait: str | int | None = None

    def to_wire(self) -> dict[str, Any]:
        if not self.uninterruptible:
            if self.max_wait is not None:
                raise SpecError(
                    "cancellation.max_wait bounds a deferred cancellation, so it "
                    "requires uninterruptible=True"
                )
            return {}
        out: dict[str, Any] = {"uninterruptible": True}
        if self.max_wait is not None:
            ms = parse_duration_ms(self.max_wait)
            if ms <= 0 or ms > MAX_UNINTERRUPTIBLE_MS:
                raise SpecError(
                    f"cancellation.max_wait must be between 1ms and 1h, got {self.max_wait!r}"
                )
            out["max_uninterruptible_ms"] = ms
        return out

    @staticmethod
    def coerce(
        value: "Cancellation | Mapping[str, Any] | bool | None",
    ) -> "Cancellation | None":
        if value is None or isinstance(value, Cancellation):
            return value
        if isinstance(value, bool):
            return Cancellation(uninterruptible=value)
        return Cancellation(**value)


@dataclasses.dataclass(frozen=True, slots=True)
class SecretRef:
    """A secret REFERENCE, never a value.

    The control plane stores this document; secret values resolved by the worker
    at execution time never enter the metadata store, so a metadata-store
    compromise leaks no credentials (architecture §9).
    """

    name: str
    ref: str

    def to_wire(self) -> dict[str, str]:
        if not self.name or not self.ref:
            raise SpecError("secret requires both name and ref")
        if "://" not in self.ref:
            raise SpecError(
                f"secret {self.name!r} ref {self.ref!r} is not a provider URI "
                "(e.g. 'vault://kv/ml/s3'); a literal secret value must never be "
                "written into a spec"
            )
        return {"name": self.name, "ref": self.ref}


@dataclasses.dataclass(frozen=True, slots=True)
class Runtime:
    """How the task is executed (ADR-0007). Executor is a placement constraint.

    A worker advertises the executors it supports, so declaring `docker` here
    restricts the task to Docker-capable workers rather than failing at dispatch.
    """

    executor: Literal["subprocess", "docker", "k8s", "kubernetes"] = "subprocess"
    image: str | None = None
    command: Sequence[str] | None = None
    args: Sequence[str] | None = None
    env: Mapping[str, str] | None = None
    secrets: Sequence[SecretRef] | None = None
    working_dir: str | None = None
    #: Name of a DECLARED output that receives the task's stdout (subprocess and
    #: docker executors). A file of the same name in $NEXUS_OUTPUT_DIR wins over
    #: stdout, because writing the file is deliberate while stdout also carries
    #: incidental chatter. The task decorator checks the name is declared.
    stdout_output: str | None = None

    def to_wire(self) -> dict[str, Any]:
        if self.executor not in ("subprocess", "docker", "k8s", "kubernetes"):
            raise SpecError(
                f"unknown executor {self.executor!r}; expected subprocess, docker, kubernetes (or its alias k8s)"
            )
        if self.executor in ("docker", "k8s", "kubernetes") and not self.image:
            raise SpecError(f"runtime.executor={self.executor} requires an image")
        out: dict[str, Any] = {"executor": self.executor}
        if self.image:
            out["image"] = self.image
        if self.command:
            out["command"] = list(self.command)
        if self.args:
            out["args"] = list(self.args)
        if self.env:
            out["env"] = {str(k): str(v) for k, v in sorted(self.env.items())}
        if self.secrets:
            out["secrets"] = [s.to_wire() for s in sorted(self.secrets, key=lambda s: s.name)]
        if self.working_dir:
            out["working_dir"] = self.working_dir
        if self.stdout_output is not None:
            if not isinstance(self.stdout_output, str) or not self.stdout_output.strip():
                raise SpecError("runtime.stdout_output must be a non-empty output name")
            if self.executor in ("k8s", "kubernetes"):
                # The kubernetes executor refuses it at run time
                # (agent/internal/executor/kubernetes.go); say so at the decorator.
                raise SpecError(
                    "runtime.stdout_output is not supported by the kubernetes executor; "
                    "write the output to $NEXUS_OUTPUT_DIR/<name> instead"
                )
            out["stdout_output"] = self.stdout_output
        return out

    @staticmethod
    def coerce(value: "Runtime | Mapping[str, Any] | None") -> "Runtime | None":
        if value is None or isinstance(value, Runtime):
            return value
        data = dict(value)
        secrets = data.get("secrets")
        if secrets:
            data["secrets"] = [
                s if isinstance(s, SecretRef) else SecretRef(**s) for s in secrets
            ]
        return Runtime(**data)


@dataclasses.dataclass(frozen=True, slots=True)
class Concurrency:
    """Named global semaphore: at most `limit` attempts sharing `key` run at once."""

    key: str
    limit: int

    def __post_init__(self) -> None:
        if not self.key:
            raise SpecError("concurrency.key must be non-empty")
        if self.limit < 1:
            raise SpecError(
                f"concurrency.limit must be >= 1, got {self.limit}; use a node_selector "
                "or a separate queue to stop a task running entirely"
            )

    @staticmethod
    def coerce(value: "Concurrency | Mapping[str, Any] | None") -> "Concurrency | None":
        if value is None or isinstance(value, Concurrency):
            return value
        return Concurrency(**value)


# ── Typed interface ───────────────────────────────────────────────────────────

_VALUE_TYPES = frozenset({"int", "float", "string", "bool", "json", "file_ref"})


@dataclasses.dataclass(frozen=True, slots=True)
class ValueSpec:
    """A typed input or output. Enables edge type-checking at submit time.

    Declaring types is what lets an incompatible edge be rejected at submit
    rather than at 3am (Flyte's typed-interface idea, architecture §4.3).

    ``max_bytes`` is an optional author-declared bound on the value's encoded
    size. On a map template's output it lets validation reject a workflow whose
    fan-in (``max_fanout x max_bytes``) could exceed the platform's fan-in
    limit, instead of failing after every child has run.
    """

    type: str = "json"
    required: bool = True
    default: Any = dataclasses.field(default=None)
    has_default: bool = False
    # Author-declared upper bound on the value's encoded size. On a map
    # template's output it lets submit-time validation prove that
    # max_fanout x max_bytes fits the platform's fan-in limit (ARCH 5.4), so an
    # oversized fan-in is rejected at submit instead of failing the run later.
    max_bytes: int | None = None

    def to_wire(self) -> dict[str, Any]:
        if self.type not in _VALUE_TYPES:
            raise SpecError(
                f"unknown value type {self.type!r}; expected one of "
                f"{', '.join(sorted(_VALUE_TYPES))}"
            )
        out: dict[str, Any] = {"type": self.type}
        if self.required and not self.has_default:
            out["required"] = True
        if self.has_default:
            out["default_json"] = json.dumps(self.default, sort_keys=True, separators=(",", ":"))
        if self.max_bytes is not None:
            if (
                isinstance(self.max_bytes, bool)
                or not isinstance(self.max_bytes, int)
                or self.max_bytes < 1
            ):
                raise SpecError(
                    f"max_bytes must be a positive integer byte count, got {self.max_bytes!r}"
                )
            out["max_bytes"] = self.max_bytes
        return out

    @staticmethod
    def coerce(value: "ValueSpecLike") -> "ValueSpec":
        if isinstance(value, ValueSpec):
            return value
        if isinstance(value, str):
            return ValueSpec(type=value)
        data = dict(value)
        unknown = set(data) - {"type", "required", "default", "max_bytes"}
        if unknown:
            raise SpecError(
                f"unknown value option(s) {', '.join(sorted(unknown))}; expected type, "
                "required, default, max_bytes"
            )
        raw_max = data.get("max_bytes")
        # Human units ("4Ki") are accepted like every other size in the SDK.
        max_bytes = parse_bytes(raw_max) if isinstance(raw_max, str) else raw_max
        if "default" in data:
            return ValueSpec(
                type=str(data.get("type", "json")),
                required=bool(data.get("required", False)),
                default=data["default"],
                has_default=True,
                max_bytes=max_bytes,
            )
        return ValueSpec(
            type=str(data.get("type", "json")),
            required=bool(data.get("required", True)),
            max_bytes=max_bytes,
        )


#: What an author may write for a value declaration: a ValueSpec, a type name,
#: or a mapping such as ``{"type": "json", "required": True, "max_bytes": "4Ki"}``.
ValueSpecLike = ValueSpec | Mapping[str, Any] | str


def coerce_trigger_rule(value: str) -> str:
    name = _enum(_TRIGGER_NAMES, value, "trigger_rule")
    short = value.strip().lower()
    if short in _TRIGGER_NAMES and short not in _MVP_TRIGGER_RULES:
        raise SpecError(
            f"trigger_rule {short!r} is defined in the proto but not implemented by "
            "the Phase 1 DAG engine (architecture §11 supports all_success and "
            "all_done only); emitting it now would produce a spec the server rejects"
        )
    return name


def coerce_idempotency(value: str) -> str:
    return _enum(_IDEMPOTENCY_NAMES, value, "idempotency")
