# DAG semantics

## Edges carry data

An edge `{from_node_id, to_node_id, output_to_input}` does two things. It
orders the two nodes, and it delivers upstream outputs to downstream inputs.
`output_to_input` is keyed by the **downstream input name**, and each value
is an upstream output name (CONTRACT-FINDINGS F1 records that the field name
reads backwards). An edge with an empty map is a pure ordering dependency.

The gateway rejects a graph at submit time when:

- it has a cycle;
- an edge names an undeclared output or input, or types disagree;
- a required input has no source (edge, parameter, literal or default);
- a map template is unbounded or its fan-in could exceed the platform limit.

## When a node runs: trigger rules

A node's `trigger_rule` decides when it may run. The rules come from
Airflow's taxonomy.

!!! warning "Only `all_success` and `all_done` are accepted today"
    The schema and the DAG engine know all six rules, but gateway validation
    rejects the last four (`trigger_rule ... is implemented but deferred to
    Phase 2`, `internal/dag/validate.go`). The table documents the engine's
    behaviour so that you know what they will mean once they are enabled.

| Rule | Fires when | Can no longer fire when |
| --- | --- | --- |
| `all_success` (default) | every upstream `SUCCEEDED` | any upstream failed, was skipped or was cancelled (decided at once, without waiting for the rest) |
| `all_done` | every upstream is terminal, whatever the outcome | never |
| `one_success` | the first upstream `SUCCEEDED` (it does not wait for the others) | all upstreams are terminal and none succeeded |
| `one_failed` | the first upstream failed | all upstreams are terminal and none failed |
| `none_failed` | all upstreams are terminal and none failed (skips are fine) | any upstream failed |
| `all_skipped` | every upstream was `SKIPPED` | any upstream ended other than skipped |

When a rule can no longer fire, the node settles without running. It becomes
`UPSTREAM_FAILED` if any upstream failed, because failure wins so the operator
sees it, and `SKIPPED` otherwise. That decision is made in the same reconcile,
so a large fan-out collapses promptly.

`all_done` is the rule for clean-up and notification tasks that must run
after a failure. The DAG engine (`internal/dag`) and the
[local runner](../recipes/local-development.md) implement the same rules, and
shared fixtures keep the two engines in step
([ADR-0022](../design/adr/0022-local-runner-and-replay.md)).

## Failure propagation

- `FAILED`: this node ran and did not succeed after its retries.
- `UPSTREAM_FAILED`: this node never ran because an ancestor failed.
- `SKIPPED`: this node's trigger rule could not be satisfied.

Keeping them apart answers "what actually broke" in a fifty-node pipeline:
the `FAILED` nodes are the causes, and the `UPSTREAM_FAILED` nodes are the
consequences.

A retry-exhausted `TIMED_OUT` or `LOST` attempt folds into a `FAILED` node
(F2). A run whose nodes are all terminal, with at least one `CANCELLED` and no
failures, ends `CANCELLED` (F3).

## `fail_fast`

With `fail_fast: true`, the first terminal failure stops dispatch of **new**
work. Running attempts finish, because killing them is what cancel does. The
remaining nodes settle as cancelled.

## No runtime branching

Nexus has no branch operator that picks a path from a runtime value. What it
has instead:

- `all_done` for clean-up and notification paths that must run after a
  failure (the other rules are deferred, see above);
- a task that decides internally and does nothing on the path not taken;
- a [map](fan-out-fan-in.md) over a computed collection, which may be empty.
  An empty collection gives zero children and an empty fan-in.

The [Airflow migration guide](../migrate/airflow.md) covers what this means
for `BranchPythonOperator`.
