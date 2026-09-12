# ADR-0011: Microservice decomposition with a shared database schema

- **Status:** Accepted
- **Date:** 2026-09-12
- **Amends:** [ADR-0001](0001-monorepo-and-language-split.md) (deployment topology only; the monorepo and language split stand)
- **Deciders:** platform team

## Context

ADR-0001 chose a monorepo and, by implication, deployed the control-plane
modules (workflow manager, DAG engine, scheduler, resource manager) from a
shared binary — a modular monolith with logical boundaries, split later "when
measured scaling or ownership needs justify it."

A microservice deployment is now a requirement: each control-plane component
must be independently deployable and scalable.

The binding constraint is ADR-0002 and ADR-0010. Dispatch commits five facts in
one transaction — the capacity reservation, the lease, the attempt state CAS,
the run event, and the deadline timers. That single transaction is the entire
reason the platform can claim no-double-dispatch and no-capacity-drift with N
scheduler replicas and no leader election. Splitting scheduler and resource
manager into services with *separate databases* would require replacing it with
a saga plus an outbox, introducing compensating transactions on a path where a
compensation bug means either a leaked GPU reservation or a double-dispatched
task.

Independent deployability and a shared transactional boundary are not actually
in conflict, provided the sharing is disciplined.

## Decision

**Six independently deployable control-plane services, one shared Postgres
schema with strict per-service table ownership.**

| Service | Owns (writes) | Reads |
|---|---|---|
| `api-gateway` | `api_keys`, `idempotency_keys` | all (read-only projections) |
| `workflow-manager` | `workflow_specs`, `workflow_runs`, `timers` | nodes, attempts |
| `dag-engine` | `task_nodes`, `task_edges` | attempts, runs |
| `scheduler` | `task_attempts`, `leases`, `concurrency_slots` | nodes, workers, reservations |
| `resource-manager` | `workers`, `reservations` | attempts, leases |
| `event-service` | `run_events` | — |

Rules that make this a real boundary rather than a shared-database free-for-all:

1. **A service writes only the tables it owns.** Cross-service writes go
   through the owner's gRPC API. Enforced by per-service Postgres roles with
   table-level grants, so a violation fails at the database, not at code review.
2. **The dispatch transaction is the one deliberate exception.** The scheduler
   writes `reservations` and `workers.committed` (resource-manager tables)
   inside the claim transaction, because atomicity there is worth more than
   ownership purity. This is documented, tested, and the *only* exception.
   Everywhere else the scheduler calls the resource manager's API.
3. **Each service is its own container, gRPC API, health/readiness endpoint,
   and scaling unit.** No shared process.
4. **Schema migrations are a versioned, ordered artifact applied by a
   dedicated job**, never by a service at startup — concurrent services must
   not race to migrate.
5. **Services are backward/forward compatible across one schema version**, so
   a rolling deploy can run mixed versions against one schema. Additive
   migrations only; a column is dropped a release *after* the last reader is
   gone.

## Consequences

**Positive**
- Each service scales independently: schedulers scale with dispatch rate,
  gateways with client traffic, resource manager with fleet size.
- Independent deploy and blast radius. A gateway crash-loop does not stop
  in-flight scheduling.
- The critical dispatch invariant is **preserved exactly** — no sagas, no
  compensating transactions, no eventual consistency on capacity.
- Clear ownership boundaries make the codebase navigable and give each service
  a real API contract.
- Per-service DB roles turn the ownership rule into an enforced constraint.

**Negative**
- **A shared schema is real coupling.** A migration can affect several
  services, so migrations must be additive and compatibility-tested. This is
  the central cost, and it is accepted knowingly.
- More operational surface: six deployments, six health checks, service
  discovery, and inter-service auth (mTLS).
- Local development needs Compose rather than one binary. Mitigated by the
  MVP's `docker compose up` being a deliverable anyway.
- Cross-service calls add latency and new failure modes (timeouts, retries,
  partial failure) on paths that were previously function calls. Every
  inter-service call needs a timeout and a retry budget.
- Debugging spans processes; distributed tracing moves from nice-to-have to
  necessary.

**Neutral**
- Nothing forecloses database-per-service later. Table ownership is already
  explicit, so extracting a service's tables is a mechanical change — at which
  point the saga cost would have to be paid deliberately.

## Alternatives considered

**Database per service (full isolation).** The textbook microservice answer.
Rejected: it forces the dispatch transaction into a saga with compensating
transactions, directly weakening the no-double-dispatch and no-capacity-drift
guarantees that ADR-0002 and ADR-0010 exist to provide. Trading a *correctness*
guarantee for *deployment* purity is the wrong trade, particularly when the
shared-schema approach delivers the deployment benefit without it.

**Modular monolith (ADR-0001 as written).** Simplest to operate and deploy, and
the right default for an MVP. Superseded by the explicit requirement for
independent deployability.

**Services communicating only via the event bus (event-driven choreography).**
Attractive for decoupling. Rejected for the *dispatch* path, which needs a
synchronous, transactional decision — "can this task run here, and claim it if
so" cannot be eventually consistent without risking double-dispatch. The event
bus remains the right mechanism for fan-out and notification (ADR-0003).
