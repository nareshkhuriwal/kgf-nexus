# Python SDK reference

Generated from the SDK's docstrings and type annotations by
[mkdocstrings](https://mkdocstrings.github.io/). The analysis is static: the
package is parsed, never imported. The source is [sdk/python/nexus](repo:sdk/python/nexus/),
and the narrative guide is the [SDK README](repo:sdk/python/README.md).

Install from a checkout (`pip install -e sdk/python`). The PyPI name
`nexus-sdk` belongs to an unrelated project.

## Authoring

::: nexus.task.task

::: nexus.task.TaskDefinition

::: nexus.workflow.workflow

::: nexus.workflow.WorkflowDefinition

::: nexus.workflow.OutputRef

::: nexus.workflow.TaskInvocation

::: nexus.workflow.CompiledWorkflow

## Policies and resources

::: nexus.types.Resources

::: nexus.types.Gpu

::: nexus.types.Timeouts

::: nexus.types.Retry

::: nexus.types.Cache

::: nexus.types.Cancellation

::: nexus.types.Concurrency

::: nexus.types.Runtime

::: nexus.types.SecretRef

::: nexus.types.ValueSpec

::: nexus.types.SpecError

## Inside a task

::: nexus.context.TaskContext

::: nexus.context.derive_idempotency_key

## Client

::: nexus.client.NexusClient

::: nexus.client.NexusError

::: nexus.client.NexusAPIError

::: nexus.scheduling.Schedule

::: nexus.scheduling.SchedulePreview

::: nexus.scheduling.Backfill

## Local runs and replay

::: nexus.local.run

::: nexus.local.LocalRunResult

::: nexus.local.NodeResult

::: nexus.replay.replay

::: nexus.replay.ReplayResult

## Canonical form

::: nexus.canonical.canonical_json

::: nexus.canonical.spec_hash
