# Quotas

A namespace can be capped. Quotas live in the namespace's platform policy
([ADR-0021](../design/adr/0021-namespace-quotas-and-usage-metering.md)).
**Zero or absent means unlimited**, so an upgraded deployment behaves exactly
as before.

| Field | Kind | Enforced |
| --- | --- | --- |
| `max_inflight_attempts` | concurrent | in the dispatch claim |
| `max_cpu_millis`, `max_memory_bytes`, `max_gpu_count` | concurrent (sum of requests in flight) | in the dispatch claim |
| `max_custom` (`{name: qty}`) | concurrent | in the dispatch claim |
| `max_active_runs` | volume | at run creation |
| `max_runs_per_hour` | rate (sliding 60 minutes) | at run creation |
| `storage_soft_limit_bytes` | soft | reported only |
| `locked` | governance | see below |

- A concurrent quota that is full leaves attempts `PENDING`, with the reason
  visible on the attempt. It never fails them.
- A run-creation limit rejects `POST .../runs` with HTTP 429,
  `namespace_quota_exceeded`, the dimension, used and limit, and a
  `Retry-After` header for the hourly rate. `nexusctl` exits 1 and prints the
  message.
- Read and change quotas with `GET`/`PUT /v1/namespaces/{ns}/quotas`
  (permission `quotas.manage`) or in the console under Admin → Quotas. A
  stale save gets 409, and every change writes a `quota.update` audit entry.
- **Who may loosen a quota.** An unlocked quota is self-governed by the
  namespace administrator. Once a platform operator sets `locked: true`, a
  namespace administrator may only *tighten* limits.

## Usage metering

The scheduler records a usage ledger: attempt-seconds by resource, per
namespace. `GET /v1/namespaces/{ns}/usage` returns it, and platform operators
can read every namespace through `GET /v1/platform/usage`. Export,
retention and alerting are in [runbook §21](../operations/runbook.md#21-namespace-quotas-and-usage-metering).
