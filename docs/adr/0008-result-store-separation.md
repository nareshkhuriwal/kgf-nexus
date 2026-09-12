# ADR-0008: Result store separate from the metadata store

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

Tasks produce outputs, and downstream tasks consume them. Those outputs range over many orders of magnitude: a boolean flag, a small JSON summary, a 200 GB training checkpoint.

The tempting simplification is to store outputs in the metadata store alongside run state. It fails badly:

- Postgres row and TOAST overhead make large blobs expensive to store and slow to read.
- Every large output inflates backups, replication lag, and vacuum cost for the *entire* control plane. One team's checkpoint degrades everyone's scheduling latency.
- Outputs would transit the API Gateway, so a single large task can saturate the control plane's network path and starve heartbeats — a correctness risk, since a starved heartbeat means a wrongly-declared-dead worker and duplicated work.
- It breaks the plane-separation invariant that task *data* never flows through the control plane (architecture §3.1, invariant 3).

There is a counter-pressure: forcing every trivial output through object storage adds two network round-trips to tasks that return `{"rows": 42}`, which in a 50-node DAG is real latency for no benefit.

## Decision

**Task inputs and outputs live in a dedicated result store (S3-compatible; MinIO in the MVP). The metadata store holds only references.** Workers read and write the result store **directly** — payloads never pass through the control plane.

- Objects are **content-addressed**: `s3://<bucket>/<namespace>/<sha256>`. Identical content is stored once, and a reference is self-verifying.
- **Small results are inlined** in Postgres below a threshold (proposed 64 KiB; open question §12.4) to avoid the round-trip for the common trivial case. The `output_ref` JSONB discriminates inline from blob, so consumers see one uniform interface.
- Objects are **immutable**. A retry writes a new object; it never mutates an existing one. This is what makes content addressing and memoization (§6.4) sound.
- Workers receive **scoped, short-lived credentials** (or presigned URLs) covering only their namespace prefix, so a compromised worker cannot read another tenant's results.
- Retention is independent of metadata retention: results typically expire sooner than run history.

## Consequences

**Positive**
- The metadata store stays small and fast. Control-plane performance is decoupled from payload size — the main point.
- Large data transfer scales with the worker fleet rather than through a control-plane chokepoint.
- Content addressing gives free deduplication and is the foundation for memoization.
- Object storage is cheap, durable, and operationally boring. It is also the one storage primitive available identically on every cloud and locally via MinIO.
- Inline small results keep short DAGs fast.

**Negative**
- A second storage system to operate, and a second failure domain: result-store unavailability fails tasks as `transient` (architecture §10). Mitigated by object storage's high availability and by retries.
- Credential distribution to workers is real complexity (scoping, rotation, expiry).
- Orphaned objects accumulate when runs are pruned. Needs a garbage-collection sweep keyed on reachability from live `output_ref`s — deliberately a background reconciler, never an inline delete, because a bug in an inline delete path destroys data.
- The inline/blob threshold is a tuning parameter that will be wrong for someone.

## Alternatives considered

**Outputs in Postgres.** Rejected on every ground above. Notably, this is the Celery result-backend default, and its problems at scale are well documented.

**Outputs through the control plane to object storage (proxied).** Gives the control plane a central audit and policy point for data access. Rejected: it reintroduces the network chokepoint and the heartbeat-starvation risk, for a benefit (audit) obtainable more cheaply from object-store access logs.

**Workers pass outputs directly to each other.** Lowest latency, no intermediate store. Rejected: it requires worker-to-worker reachability (breaking ADR-0005's topology freedom), and it makes outputs non-durable, so a retry of a downstream task cannot re-read its input if the upstream worker is gone. Durability of intermediate results is what makes partial DAG retry possible at all.

**A pluggable result-store interface from day one.** Deferred, not rejected. The S3 API is a de facto standard with local (MinIO) and cloud implementations, so it covers the field. A filesystem/NFS backend is a plausible Phase 2 addition behind the same interface.
