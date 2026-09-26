# Secrets

## The rule

**A credential never goes into a spec, a parameter, a URL or a log.** Specs
are stored, versioned, shown in the console, and returned by the API to
anyone with `workflows.view`. Run parameters are visible in the same way.

The spec grammar supports this: `runtime.secrets` accepts **references
only**, as provider URIs, and the schema rejects a bare value.

```yaml
runtime:
  secrets:
    - {name: DB_PASSWORD, ref: "vault://kv/etl/db"}
```

## What works today

!!! danger "`runtime.secrets` has no effect yet"
    No worker resolves secret references. A reference is validated and stored,
    and the task runs **without** the secret. The agent's log-redaction hook
    also has nothing to redact until a resolver feeds it
    (`agent/internal/worker/runner.go`,
    [logs](../concepts/logs.md#secrets-in-logs)). `nexusctl import` reports source
    secrets as not translated for this reason.

Until a resolver exists, give tasks credentials through the **execution
environment**, which the control plane never sees:

| Executor | Mechanism |
| --- | --- |
| `kubernetes` | Task pods run as `NEXUS_K8S_SERVICE_ACCOUNT` with no service-account token mounted. Cloud workload identity (IRSA, GKE Workload Identity, Azure Workload Identity) injects its own credentials, so let the task fetch from your secret manager with that identity. Whether your provider's injection works with the executor's pod spec has not been verified in this repository, so test it on your cluster. |
| `docker` | Bake nothing into the image. Have the task read from a secret manager using the host's identity, for example an instance profile. |
| `subprocess` | The task inherits the agent's **entire** environment, including the agent's own credentials ([security](../concepts/security-tenancy.md#credentials)). Only use subprocess pools for trusted workloads. |

In the handler, fetch the secret, use it, and never log it:

```python
@task
def load(ctx: TaskContext, rows: list) -> dict:
    password = secret_manager.get("etl/db")      # your client, your identity
    ctx.log.info("connecting", host=HOST)       # never the password
    ...
```

## Checklist

- Pool separation: a queue whose workers hold production credentials should
  serve only the workloads that need them. `node_selector` and queues are the
  routing tools ([queues](../concepts/queues-fair-share.md)).
- Never pass a secret as `--param`. Parameters are stored with the run and
  shown in `run get`.
- Do not print environment variables in tasks. Logs are durable and
  searchable.
- For API clients, prefer short-lived scoped tokens
  (`POST /v1/namespaces/{ns}/tokens` with an expiry) over long-lived keys, and
  keep them in `NEXUS_API_KEY`, never in a command-line flag.
