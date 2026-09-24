# ADR-0006: At-least-once execution with mandated idempotency

- **Status:** Proposed
- **Date:** 2026-09-12
- **Deciders:** platform team

## Context

A distributed orchestrator must choose a delivery guarantee. The options are real and each has a cost.

**At-most-once.** Never retry an uncertain outcome. Simple, and side effects never duplicate — but a workflow stalls on every ambiguous failure and needs human intervention. Unusable for unattended pipelines.

**Exactly-once.** What everyone wants. Consider the unavoidable scenario: a worker completes a task, commits its side effect, then dies before its report is acknowledged. The control plane sees only silence. It cannot distinguish "died after the effect" from "died before the effect." Retrying may duplicate; not retrying may stall. This is the Two Generals problem, and it is a property of networks, not of implementation quality. Systems marketed as exactly-once deliver at-least-once plus deduplication at the effect boundary.

**At-least-once.** Retry uncertain outcomes. Tasks may run more than once, so correctness requires idempotency.

## Decision

**Nexus provides at-least-once execution and treats idempotency as a platform requirement, not a user aspiration.**

Concretely:

1. **The platform derives the idempotency key** (§6.2): `sha256(run_id ‖ node_id ‖ map_index ‖ task_version)`. Users never invent keys — that is where this goes wrong in practice. `attempt_number` is deliberately **excluded**, so every retry of the same logical work shares one key. That exclusion is the mechanism.
2. **`idempotency: required` is the default** in the task spec. A task may declare `best_effort` or `none`, but does so explicitly and visibly.
3. **Lease fencing** (ADR-0010) means at most one attempt is *authorized* at a time. A zombie worker's late report is rejected on a stale token, so duplicate execution can never corrupt control-plane state — only produce a duplicate side effect, which is precisely what idempotency addresses.
4. **`LOST` is a distinct state from `FAILED`** (§5.1). `FAILED` means we know it ran and did not succeed. `LOST` means we do not know whether it ran. They get different retry policies, because they carry different information.
5. **Indeterminate outcomes are handled per declared semantics** (§6.5): `required` → auto-retry; `best_effort` → auto-retry with a `PossibleDuplicateExecution` warning event; `none` → **refuse to auto-retry**, mark `FAILED` with `reason=indeterminate_outcome`, require explicit human action.
6. **The SDK provides the primitives** in a documented preference order: pass the key to a downstream system's native dedup (strongest) → conditional write / `ON CONFLICT DO NOTHING` → platform effect journal (weakest, but narrows the crash window from the whole task to one write).
7. **The worker WAL** (§6.3) removes the common case: an agent restart replays a completed-but-unreported outcome instead of discarding finished work. Only genuinely ambiguous crashes reach the indeterminate path.
8. **Memoization** (§6.4) is a separate, complementary mechanism: idempotency makes re-execution *safe*; memoization makes it *unnecessary*.

## Consequences

**Positive**
- Workflows make progress through worker death, network partition, and control-plane restart without human intervention.
- The guarantee is stated accurately, so users can reason about it correctly. A precisely-described weaker guarantee is far more useful than a comfortably-described one that does not hold.
- Platform-derived keys eliminate the most common source of idempotency bugs.
- Point 5 means the platform never silently double-charges a customer because retrying was more convenient.

**Negative**
- **Users must write idempotent handlers.** This is a genuine burden and cannot be hidden. Mitigated by defaulting to `required`, providing SDK primitives, documenting the pattern prominently, and surfacing non-idempotent tasks in the UI.
- Tasks declaring `idempotency: none` need human intervention on indeterminate outcomes. Correct, but real operational cost — the UI must make these visible and actionable.
- The effect journal adds a write to the result store per guarded effect.

**Neutral**
- Duplicate execution is observable in metrics (`possible_duplicate_execution_total`), so its real-world frequency can be measured rather than guessed.

## Alternatives considered

**Claim exactly-once.** Rejected as dishonest. It would mislead users into skipping the idempotency work that the semantics actually require, producing exactly the silent-data-corruption bugs the claim implies are impossible.

**At-most-once.** Rejected: every ambiguous failure becomes a page. Available per-task via `idempotency: none`, which is effectively at-most-once for that task — the right place for this behaviour is an opt-in, not a platform default.

**Transactional side effects (two-phase commit with downstream systems).** Genuine exactly-once where the downstream supports it. Rejected: almost nothing in practice supports 2PC, it is operationally fragile, and coordinator failure creates worse problems than it solves.

**Deterministic replay (Temporal).** Solves a related but different problem — it makes *orchestration* resumable, not side effects exactly-once. Temporal activities are themselves at-least-once and require idempotency, exactly as here. Not an alternative to this decision.

## Addendum (2026-09): memoization implemented

Point 8 is implemented (architecture §6.4). Two consequences for this ADR's guarantees:
a memo hit is recorded as its own `SUCCEEDED` attempt with `cache_hit_of`, so "show me
every attempt" still answers truthfully (it reused, it did not run); and memoization is
never applied to `idempotency: none`, so the at-most-once opt-out cannot be bypassed by
the cache. Dynamic-expansion children derive their key from the TEMPLATE id plus
`map_index` (§6.2), so retrying child 7 re-runs exactly child 7's logical work.
