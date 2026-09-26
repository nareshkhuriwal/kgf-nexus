# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

"""Nexus Python SDK -- a COMPILER for workflow specs, not a runtime hook.

    from nexus import task, workflow, TaskContext, Resources, Retry

    @task(resources=Resources(cpu="2", memory="8Gi"), cache=True)
    def extract(ctx: TaskContext, source: str) -> dict:
        ctx.heartbeat("connecting")
        return {"rows": fetch(source)}

    @workflow(name="daily-etl")
    def daily_etl(sources: str):
        return load(transform(extract(sources)))

    compiled = daily_etl.compile()      # WorkflowSpec JSON, in YOUR process
    print(compiled.hash)                # the server's content address

WHAT THIS SDK IS, precisely (ADR-0004): the decorators run in YOUR process and
emit a WorkflowSpec JSON document that is submitted over HTTP. The control plane
parses that document and never imports, executes, or evals your code. It needs
none of your dependencies installed, and a slow import or an infinite loop in
your module cannot degrade scheduling for anyone else. That is the single largest
reliability divergence from Airflow, and it is structural rather than a promise.

What follows from it, and is worth internalising:
  * Your workflow BODY runs once, at compile time, on your machine. Side effects
    in a body happen then and there.
  * Runtime values do not exist at compile time. `OutputRef` is a reference, and
    using one as a value raises rather than quietly compiling a graph you did not
    write.
  * `nexusctl workflow compile --show` prints exactly the document the server
    will see. Use it whenever the graph is not what you expected.

Execution is AT-LEAST-ONCE (ADR-0006): a handler can run more than once for one
logical unit of work. See `nexus.task.task` and `TaskContext.record_effect` for
the three idempotency mechanisms in descending order of strength.
"""

from __future__ import annotations

import importlib
from types import ModuleType
from typing import TYPE_CHECKING

from .canonical import CanonicalError, canonical_bytes, canonical_json, spec_hash
from .client import (
    AuthError,
    Conflict,
    NexusAPIError,
    NexusClient,
    NexusError,
    NotFound,
)
from .context import (
    EffectNotRecorded,
    FileResultStore,
    InMemoryResultStore,
    ResultStore,
    StructuredLogger,
    TaskContext,
    derive_idempotency_key,
)
from ._taskapi import CheckpointInfo, StaleCheckpoint, StaleLease
from .ratelimit import PermitGrant, RateLimited
from .scheduling import Backfill, BackfillState, OverlapPolicy, Schedule, SchedulePreview
from .task import TaskDefinition, registry, task
from .tracing import TraceContext
from .types import (
    Asset,
    Cache,
    Artifact,
    Cancellation,
    Concurrency,
    Gpu,
    Placement,
    Resources,
    Retry,
    Runtime,
    SecretInject,
    SecretRef,
    SpecError,
    Timeouts,
    ValueSpec,
    ValueSpecLike,
)
from .units import (
    UnitParseError,
    format_bytes,
    format_cpu_millis,
    format_duration_ms,
    parse_bytes,
    parse_cpu_millis,
    parse_duration_ms,
)
from .sourcemap import NodeSource, SourceLocation
from .workflow import (
    API_VERSION,
    CompiledWorkflow,
    OutputRef,
    Param,
    Ref,
    TaskInvocation,
    WorkflowDefinition,
    workflow,
)

# Local execution without a cluster (ADR-0022): nexus.local.run(workflow, params)
# and nexus.replay.replay(client.get_assignment(attempt_id)). Imported lazily
# (PEP 562), so `python -m nexus.local` does not find itself half-imported.
if TYPE_CHECKING:
    from . import local, replay


def __getattr__(name: str) -> ModuleType:
    if name in ("local", "replay"):
        return importlib.import_module(f".{name}", __name__)
    raise AttributeError(f"module {__name__!r} has no attribute {name!r}")


__version__ = "0.1.0"

__all__ = [
    "__version__",
    "API_VERSION",
    "local",
    "replay",
    # authoring
    "task",
    "workflow",
    "TaskDefinition",
    "WorkflowDefinition",
    "CompiledWorkflow",
    "OutputRef",
    "Ref",
    "TaskInvocation",
    "SourceLocation",
    "NodeSource",
    "Param",
    "registry",
    # declaration types
    "Asset",
    "Resources",
    "Gpu",
    "Placement",
    "Artifact",
    "Timeouts",
    "Retry",
    "Cache",
    "Cancellation",
    "Runtime",
    "SecretRef",
    "SecretInject",
    "Concurrency",
    "ValueSpec",
    "ValueSpecLike",
    "SpecError",
    # execution
    "TaskContext",
    "ResultStore",
    "InMemoryResultStore",
    "FileResultStore",
    "StructuredLogger",
    "derive_idempotency_key",
    "EffectNotRecorded",
    "TraceContext",
    # AI workloads (ADR-0032): nexus.usage, nexus.llm, nexus.testing
    "RateLimited",
    "PermitGrant",
    "CheckpointInfo",
    "StaleLease",
    "StaleCheckpoint",
    # client
    "NexusClient",
    "NexusError",
    "NexusAPIError",
    "AuthError",
    "NotFound",
    "Conflict",
    # schedule / backfill response types
    "Schedule",
    "SchedulePreview",
    "Backfill",
    "BackfillState",
    "OverlapPolicy",
    # canonicalization
    "canonical_json",
    "canonical_bytes",
    "spec_hash",
    "CanonicalError",
    # units
    "parse_cpu_millis",
    "parse_bytes",
    "parse_duration_ms",
    "format_cpu_millis",
    "format_bytes",
    "format_duration_ms",
    "UnitParseError",
]
