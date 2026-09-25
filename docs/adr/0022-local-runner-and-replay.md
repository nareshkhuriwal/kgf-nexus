# ADR-0022: A local runner and attempt replay that reuse the platform's DAG semantics

- **Status:** Proposed
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Relates to:** ADR-0004 (declarative spec), ADR-0006 (at-least-once), ADR-0008 and ADR-0016 (result store, inline threshold), ADR-0010 (leases)

## Context

Before this change, the only way to find out what a workflow does was to submit
it to a cluster. That is a slow loop for the questions an author asks most:
does my fan-out expand the way I think, does the fan-in see nulls, does the
retry policy actually retry this error, is my shell glue right? An operator with
a failed attempt had the same problem in reverse. They could read its logs, but
they could not re-run it with the inputs it was given without rebuilding the
upstream state by hand.

The risk in building a local runner is building a second orchestrator whose
semantics drift from the real one, so that "it passed locally" means nothing.
The platform's decisions live in two pure Go packages: `internal/dag` (the
frontier, trigger rules, propagation, expansion, fan-in, input plans) and
`internal/domain` (retry policy, backoff, idempotency and memo keys). Execution
lives in the agent's executors, whose contract is the NEXUS_* environment, the
`inputs/` and `outputs/` directories and the output collection rules.

## Decision

1. **One local engine, in the SDK.** `nexus.local.run(workflow, params)` (and
   `python -m nexus.local`) executes a compiled workflow in-process. It depends
   on nothing outside the standard library, which fits the SDK's
   no-runtime-dependencies rule. `nexusctl run --local <file>` delegates to it
   through the user's interpreter, exactly as `workflow compile` does for `.py`
   files. nexusctl parses YAML itself and hands the SDK canonical JSON, so the
   SDK needs no YAML library either. There is one engine, not a Go one and a
   Python one.

2. **Decisions are a port of `internal/dag` and `internal/domain`, held there by
   shared fixtures.** `nexus/_dag.py` mirrors each Go function in the same
   order of reasoning: `Reconcile`, including its cancel and `fail_fast`
   settlement; `Propagate`; `AggregateChildren` and the template fold;
   `PlanFromCollection`, including the rule that the collection must be inline;
   `FanInBuilder`, bounded before each append; `InputPlan`; `ShouldRetry`;
   `Backoff`; `IdempotencyKey`; `MemoKey` and `InputsDigest`.
   `sdk/python/tests/parity/scenarios.json` covers twelve scenarios: retries,
   terminal classes, `no_retry_on`, LOST with `idempotency: none`, fan-out and
   fan-in, a failed child, `max_fanout` exceeded, an empty collection, and
   `fail_fast`. Both engines run it: `internal/dag/localparity_test.go` runs it
   against the Go code, and `tests/test_local_parity.py` runs it against the
   port and through the real runner loop. If either side changes an outcome,
   that side's test fails. Golden idempotency-key digests are asserted in both
   languages.

3. **Execution follows the agent's contract.** A task with `runtime.command`
   runs as a subprocess in its own process group. It gets the agent's
   environment (`NEXUS_INPUT_DIR`, `NEXUS_OUTPUT_DIR`, `NEXUS_OUTPUT_FILE`,
   `NEXUS_OUTPUT_NAMES`, `NEXUS_IDEMPOTENCY_KEY`, `NEXUS_DEADLINE_UNIX_MS` and
   `NEXUS_INLINE_THRESHOLD_BYTES`) and the same output collection:
   - declared names under `outputs/`;
   - `stdout_output`, capped at 32 MiB;
   - the legacy `output.json`, reported as `result`.

   A task without a command runs its registered Python handler through
   `python -m nexus.worker` in a subprocess, or in-process for debugging.
   `start_to_close` sends SIGTERM to the process group, then SIGKILL after a
   10 s grace period. Error classes match the agent's: a non-zero exit is
   APPLICATION, a missing executable is NOT_FOUND, a timeout is TIMEOUT.
   Results go to a `FileResultStore` under the run directory. Values at or
   below the inline threshold are inlined; larger ones are stored as blobs.

4. **Replay is an API plus the same executor.** The gateway serves
   `GET /v1/namespaces/{ns}/runs/{run}/attempts/{id}/assignment`. `{run}` may
   be `-`, meaning any run in the namespace, as in AIP-159. The response
   contains the attempt's TaskSpec (`task_nodes.spec_json`), its resolved
   `input_refs`, its idempotency key and the configured inline threshold. It
   never contains the lease token or deadlines. SecretRefs are re-emitted with
   only `name` and `ref`, and blob inputs stay as references. The route
   requires `runs.view`, `tasks.view` and `tasks.results.view`. The inputs are
   upstream results, so the result-view grant that already gates attempt
   errors and event payloads applies here too. The namespace is checked
   before the read (F101): a concrete `{run}` must belong to the path's
   namespace, and the SQL filters on the namespace for the `-` form.
   `nexusctl replay <attempt> [--local] [--out DIR]` saves the assignment
   (mode 0600) and, with `--local`, runs it through `python -m nexus.replay`.
   That is the runner's executor with the original inputs and the ORIGINAL
   idempotency key. With the original key, a handler that passes it downstream
   gets deduplicated. A fresh key would turn the replay into a second, real
   effect (ADR-0006).

## Parity limits, stated rather than hidden

| Platform | Local runner |
|---|---|
| Leases, heartbeats, LOST, the WAL (ADR-0010) | None: an attempt is a child process this runner owns. `timeouts.heartbeat` is not enforced. |
| Queues, priority, weighted fair share, `max_active_runs`, `concurrency_key`, `node_selector` | None: ready work runs when one of `max_parallel` slots is free. Completion order therefore differs, which changes outcomes only under `fail_fast`. The fixtures that depend on order are run against the port, not the threaded loop. |
| Resource reservation and enforcement, GPU placement | None. Requests are neither reserved nor enforced, and `CUDA_VISIBLE_DEVICES` is not set. |
| docker and kubernetes executors | Refused. `--host-executor` runs the task's command on the host instead, which is a different environment from the task's image. |
| Memoization across runs, invalidations | Only within one run. |
| Pause, resume, operator retry, schedules, events API, UI | None. The runner writes `events.jsonl` and `run.json` instead. |
| Object store, scoped credentials, secret resolution | A local `FileResultStore`. SecretRefs are not resolved. A replay's blob inputs must be supplied with `--input`. |
| Submit-time validation | The runner refuses tasks it cannot execute and inputs that have no source. It does not run the gateway's JSON Schema. `nexusctl` still runs its local checks first, and `workflow submit --dry-run` remains the full check. |

## Consequences

**Positive.** A workflow can be run end to end on a laptop in seconds. The
committed examples run unchanged: `hello_dag` (both the .py and the YAML),
`fan_out_map`, and the 49-node `csv_sales_pipeline`, whose report task fails
the run if a row is lost. Conformance tests hold them to the outcomes those
examples already assert. Failed attempts can be reproduced with their real
inputs. The shared fixtures catch semantic drift in either language, and they
already caught one real divergence (F116).

**Negative.** There is a second implementation of the decision logic. It
stays aligned only because of the fixtures, so a new engine rule needs a
fixture added in the same change. The local runner cannot reproduce
distributed failures: lease expiry, worker loss and concurrent completions
still need the chaos and integration suites. Replay needs the SDK installed
and, for blob inputs, bytes the user fetches separately.

**Neutral.** No migration and no proto change. The endpoint and the
`line`/`column` fields on validation problems are additive.

## Alternatives considered

- **A Go runner inside nexusctl that imports `internal/dag` directly.** This
  gives the strongest parity on decisions, but nexusctl would pull in protobuf
  and gRPC and give up its standard-library-plus-yaml.v3 build. The SDK would
  still need a runner of its own for `nexus.local.run`, so there would be two
  engines anyway. Rejected in favour of one engine plus fixtures that both
  languages run.
- **A single-node "cluster in a box" (compose with every service).** This
  gives full parity, and it already exists as `make up`, but it takes minutes
  and needs Docker. It answers a different question.
- **Replay by re-submitting the node to the cluster.** That is operator retry
  (F100). It re-runs on the platform, which cannot be debugged interactively.
