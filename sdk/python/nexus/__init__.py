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
from .task import TaskDefinition, registry, task
from .types import (
    Cache,
    Cancellation,
    Concurrency,
    Gpu,
    Resources,
    Retry,
    Runtime,
    SecretRef,
    SpecError,
    Timeouts,
    ValueSpec,
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
from .workflow import (
    API_VERSION,
    CompiledWorkflow,
    OutputRef,
    Param,
    TaskInvocation,
    WorkflowDefinition,
    workflow,
)

__version__ = "0.1.0"

__all__ = [
    "__version__",
    "API_VERSION",
    # authoring
    "task",
    "workflow",
    "TaskDefinition",
    "WorkflowDefinition",
    "CompiledWorkflow",
    "OutputRef",
    "TaskInvocation",
    "Param",
    "registry",
    # declaration types
    "Resources",
    "Gpu",
    "Timeouts",
    "Retry",
    "Cache",
    "Cancellation",
    "Runtime",
    "SecretRef",
    "Concurrency",
    "ValueSpec",
    "SpecError",
    # execution
    "TaskContext",
    "ResultStore",
    "InMemoryResultStore",
    "FileResultStore",
    "StructuredLogger",
    "derive_idempotency_key",
    "EffectNotRecorded",
    # client
    "NexusClient",
    "NexusError",
    "NexusAPIError",
    "AuthError",
    "NotFound",
    "Conflict",
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
