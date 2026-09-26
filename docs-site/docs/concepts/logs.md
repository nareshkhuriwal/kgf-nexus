# Logs

Every attempt's stdout and stderr is durable, searchable and bounded
([ADR-0020](../design/adr/0020-durable-task-logs.md)).

```bash
nexusctl logs <attempt-id>                  # the whole log
nexusctl logs <attempt-id> --follow         # live tail; ends when the attempt does
nexusctl logs <attempt-id> --stream stderr --tail 100
```

`nexusctl run get <run-id>` shows each node's latest attempt id. In the
console, the attempt panel has the same log with search, and it follows
live over server-sent events.

## How it works

- The agent splits output into records: one line each, with a stream
  (`stdout`, `stderr`, `combined` where the substrate cannot separate them,
  or `nexus` for platform markers), a timestamp, and a line number that is
  contiguous from 1.
- Records are sealed into chunks (256 KiB, or every 2 s while output is
  flowing) and written **directly to the result store** under the namespace
  prefix. Only chunk metadata goes to the control plane, through a
  lease-fenced RPC, so a stale worker cannot add to a log.
- **Bounded.** Each attempt keeps 64 MiB by default. Beyond that, the head
  and a 2 MiB tail are kept, and a marker states exactly what was omitted.
- **Backpressure never reaches the task.** If the store or the gateway is
  unreachable, lines are counted and dropped past 8 MiB of unshipped output,
  and a marker records the loss. A task is never slowed or killed by its
  logging.
- Kubernetes pod logs cannot separate streams, so they are tagged
  `combined`.

## Structured logging from Python

`ctx.log.info("extracted", rows=n)` writes one JSON object per line, with run,
node, attempt and task correlation fields on every line. The `nexusctl init`
templates log this way.

## Secrets in logs

The agent has a redaction hook that replaces known secret values before a
line is buffered. It has **no input yet**, because workers do not resolve
secret references (see [security](security-tenancy.md#secrets)). A task that
prints a credential ships it verbatim, so do not print credentials.

Retention and sizing are in [runbook §19](../operations/runbook.md#19-durable-task-logs-retention-and-sizing).
