# KGF Nexus — production platform and operator console

This is a fresh session. Do not assume access to previous chats. The working repository is `/Users/nareshkhuriwal/Khuriwalgroup/kgf-nexus`. Work in this existing repository, not the separate `KGF Nexus version one` folder. This brief contains the requested outcome; repository files and current runtime inspection establish implementation facts. Earlier agents have already built substantial functionality. Inspect the current Git state and preserve uncommitted work before making changes. Do not launch another concurrent implementation against the same files; if another writer is active, agree on ownership before overlapping edits.

Continue the existing KGF Nexus project under its revised architecture. The target is a complete, production-ready, application-independent workflow orchestration product, not a demo, mock dashboard or two-tab MVP. Extend the current implementation rather than restarting it or replacing it with an unrelated framework.

The local operator console was last observed at `http://localhost:3000/workers`, showing two active agents. Treat this as a historical observation, not proof that the stack is running now. The previous review passed Go build/vet, the non-database Go test run, 45 UI tests and the UI application type check. Database integration was not verified, the Python interpreter lacked pytest, and Docker availability changed during the conversation. Recheck the present environment and run the missing verification; do not inherit a claim of production readiness.

## Establish the current baseline

Read the repository instructions, CONTRACT.md, docs/ARCHITECTURE.md, the ADRs including ADR-0011, CONTRACT-FINDINGS.md, current code and uncommitted changes. Reconcile documentation with implementation before deciding something is missing. Preserve ongoing work. The existing architecture has six control-plane services sharing PostgreSQL, Go workers and CLI, a Python SDK, React UI and MinIO-backed artifacts. The current run-detail UI already contains a DAG view, node selection, attempts, logs, events and cancellation; improve and verify these instead of duplicating them.

The user's production requirement supersedes earlier MVP-only scope. Preserve compatibility with existing workflows, API clients and worker behavior. Where production requirements conflict with a frozen contract, document the exact conflict and proposed additive contract/schema change before implementing it; do not silently change wire semantics or rewrite frozen migrations.

## Deliver a complete operator experience

Build cohesive navigation and connected workflows across these areas:

1. Overview: active/recent runs, failures, queue age, worker availability, resource reservations and service health, with drill-down links.
2. Workflow catalog: search/filter, version history, definition inspection, dependency graph, validation feedback and parameterized run submission. Support the existing SDK/CLI authoring path; a visual authoring tool is a separate feature, not a prerequisite for viewing DAGs.
3. Run explorer: filters and pagination, live state, zoomable/pannable DAG, node selection, dependency highlighting, execution timeline, parameter/spec version, attempt history, pause/resume/cancel and controlled retry with clear semantics.
4. Task/attempt detail: required and resolved inputs, output/artifact references, bounded previews/downloads, searchable live logs, error classification, retry history, worker placement, requested/reserved/observed resources, deadlines and durations. Preserve selected run/node/attempt in deep links.
5. Scheduling: create/manage schedules, timezone and daylight-saving semantics, next-run preview, enable/disable, overlap/concurrency policy, bounded catch-up/backfill and external trigger status. These require durable backend behavior, not frontend-only configuration.
6. Workers/pools: inventory, actual capabilities, heartbeat/liveness, reservations versus measured utilization, active attempts, drain, pool/queue routing and actionable placement explanations. Label simulated resources explicitly and keep them out of production defaults.
7. Administration: authenticated sessions, namespace/project access, roles, service identities, scoped credentials, secret references, quotas and audit history. Do not expose secret values in definitions, URLs, logs or bundles.
8. Operations: metrics, alerts, dependency health, retry/recovery visibility and documented backup/restore and upgrade procedures.

Every displayed control must have a real, authorized API and tested effect. Use honest loading, empty, error, stale-data, disconnected and permission-denied states. Do not present mock data, silent no-ops or unwired controls as complete features. Maintain keyboard accessibility, readable graph labels, useful tables and responsive layouts. Use real data to validate the operator experience.

## Preserve distributed correctness

Keep task execution outside the control plane and database credentials outside workers. Preserve atomic dispatch/resource reservation, lease fencing, distinct LOST outcomes, stable idempotency keys across retries and immutable attempt history. Execution remains at least once; no exactly-once promise for arbitrary external effects. Avoid Ray as a platform dependency and use standard databases, brokers and container runtimes.

Finish NATS JetStream integration with a transactional outbox or equivalently reviewed durable publication design. PostgreSQL remains authoritative. Handle duplicate/out-of-order notifications and broker outages; periodic reconciliation must preserve progress. Adding a NATS container alone does not complete integration.

Verify large artifacts actually upload, publish and reach downstream tasks; code paths and passing unit tests alone are insufficient. Validate missing/invalid outputs, upload failures, stale reports and orphan cleanup. Distinguish capacity advertised for scheduling from resource limits actually enforced by the runtime. Qualify GPU execution on real compatible hardware.

## Production delivery standard

Inventory dependencies and move to current stable, maintained, mutually compatible releases after checking official support information. Pin reproducible builds and record migration changes; avoid floating latest tags and upgrades without compatibility tests.

Define the first production environment, trusted versus external-user workloads, expected load, availability/recovery objectives and retention requirements. Ask only for decisions that materially block deployment/security architecture; continue independent development meanwhile. Do not assume a local Docker-socket setup provides hostile multi-tenant isolation.

Deliver service authentication/encryption, enforced database permissions with documented transactional exceptions, runtime isolation, scoped secret handling, metrics/tracing/alerts, backups with a restore drill, additive migrations and rolling-upgrade compatibility. Keep local Compose useful, but clearly separate development configuration from production deployment. Do not deploy publicly or apply changes to a live production environment without explicit authorization.

## Work sequence and acceptance

First produce a brief feature-to-API matrix identifying what exists, is partial or is missing, plus a prioritized implementation plan. Then implement complete vertical slices in that order; the plan is the beginning of the work, not the final deliverable. Prefer a functioning workflow-to-run-to-task-to-artifact journey before adding broad administrative navigation.

Run Go build/vet/tests, Python SDK tests, UI type checking/component tests and browser end-to-end tests. Use isolated integration infrastructure; existing database tests must not alter a shared live schema. Test concurrent scheduling, worker loss, partitions, duplicate/stale completion, lease expiry, retries, cancellation races, resource accounting, artifact transfer, broker/database recovery, authentication and namespace isolation. Test backup restoration and record load-test hardware/results against agreed targets.

Exercise a real fan-out/fan-in workflow through the UI: register/view its version, submit parameters, inspect graph/node attempts and logs, retrieve artifacts, observe a controlled failure/retry and verify final state. Cover schedule/backfill and operator actions with integration tests as they are delivered.

Report verified capabilities, checks executed and skipped, migration instructions, remaining defects and deployment blockers. Do not describe the application as production-ready until the agreed acceptance gates pass. Do not claim full feature parity with Airflow, Prefect, Ray or Argo; use their relevant patterns to meet the explicit product requirements above. GitOps application deployment is a separate scope from workflow orchestration.
