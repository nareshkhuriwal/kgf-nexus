# Nexus benchmarks

**Every number on this page is a laptop number.** Each was measured on the
single machine described in §1. The Go client, PostgreSQL and the object store
all ran on that one machine, with no network hop, and other workloads were
running at the same time. Treat the numbers as the **shape** of each curve:
where it bends, and which resource saturates first. **They are not
production capacity figures**, and no production figure exists yet
([ROADMAP.md](ROADMAP.md)).

Results were re-run on 2026-09-26 on `docs/benchmarks-cloud-design-gtm`. Its
code is `main` at `82b9ec8`; that branch changes only documentation. Raw
output is in [`docs/benchmarks/raw/`](benchmarks/raw/).

| § | What | Re-run for this page? |
|---|---|---|
| 2 | Dispatch claim: throughput and latency, by replica count and backlog, for the pull and push paths | yes, two full runs |
| 2.4 | [Quota-bound cells](#quota-bound-cells) (ADR-0021) | yes, two rounds |
| 3 | Inline result vs object-store blob (ADR-0016) | yes |
| 4 | [Chaos suite](#4-chaos-suite) (worker SIGKILL, scheduler replicas) and the example sweep | yes, in an isolated compose project |
| 5 | Restore and point-in-time recovery drills | no; linked from OPERATIONS §4.6 (2026-09-25) |

---

## 1. Hardware and environment

| | |
|---|---|
| Machine | Apple MacBook Pro, model `Mac16,7` (`sysctl hw.model`) |
| CPU | Apple M4 Pro, 14 cores: 10 performance + 4 efficiency (`machdep.cpu.brand_string`, `hw.perflevel0/1.physicalcpu`) |
| Memory | 24 GiB (`hw.memsize` = 25,769,803,776 bytes) |
| Disk | internal Apple SSD, 494 GB, APFS (`diskutil info /`: Solid State, Apple Fabric) |
| OS | macOS 26.6.2 (25G83) |
| Docker | Docker Desktop, engine 29.6.2. **The Linux VM is limited to 10 vCPU and 15.8 GiB** (`docker info`: NCPU 10, MemTotal 17,011,552,256). Kernel 6.12.76-linuxkit, overlayfs. |
| PostgreSQL | 16.15 (`postgres:16-alpine`, image `sha256:721873c3…`), `shared_buffers=512MB`, `max_connections=300`, `fsync=on`, `synchronous_commit=on`, `work_mem=4MB`. It runs in the Docker VM from `make bench-pg`, container `nexus-w3l-bench-pg`, and listens on 127.0.0.1:55881. |
| Object store | Silo `RELEASE.2026-09-16T00-00-00Z@sha256:635197cb…` (the pinned S3 image, OPERATIONS §22), container `nexus-w3l-bench-minio`, 127.0.0.1:55882 |
| Client | Go 1.27.1 darwin/arm64, running natively on the host (not in Docker), `GOMAXPROCS=14` |

**The host was not idle.** Other agents' container stacks were running
throughout: 16–48 running containers. During run 2 the 1-minute load average
was sampled every minute and ranged from **9.1 to 38.4**
(`raw/run2-load.log`). The client and the database also compete for the same
CPUs. Single cells moved **2–5×** between runs of identical code (§2.3). Read
any one cell with that noise in mind, and compare cells within one table, not
across tables.

## 2. Dispatch claim (ADR-0018)

### 2.1 Method

`make bench-pg BENCH_NAME=nexus-w3l-bench BENCH_PG_PORT=55881
BENCH_MINIO_PORT=55882`, then `make bench-dispatch`. (That was the container
`make bench-pg` of the time. It now creates a database on the host's
PostgreSQL instead, `NEXUS_TEST_PG_SERVER`, because nothing in the repository
starts a Postgres container by default; to reproduce these cells, point it at a
server configured as in §1 -- `max_connections=300`, `shared_buffers=512MB` --
since 16 replicas need far more connections than a shared workstation server
has free.) For run 2, the same test
binary was built once with `go test -c -tags bench` and run directly
(`tests/bench/dispatch_test.go`, `TestDispatchCeiling`, default knobs).

- The benchmark drives the **real** claim code:
  - `store.ClaimForWorker`, the **pull** path. Every worker poll uses it, and it
    is the only path the shipped scheduler runs.
  - `store.Claim`, the **push** loop (see §2.3).
- **R replicas.** Each replica is a goroutine with its **own** 2-connection
  pool, so replicas contend on the database the way separate processes do.
  Each pull replica polls on behalf of a disjoint slice of the workers.
- **B PENDING attempts**, spread over B/100 runs, with 64 workers of
  effectively unbounded capacity. This measures the queue, not bin packing.
- **Per cell:** 3,000 dispatches, or B/2 if that is smaller, capped at 90 s.
  Latency is one claim transaction, counted for successful claims only.
- **Columns:**
  - `empty` counts claims that found nothing they could take (push-path
    contention);
  - `errors` counts failed transactions. **No run on this page had a single
    error.**
- The windowed pull claim (migration 0023) is on. The test fails if it is not.

### 2.2 Results — run 2 (2026-09-26 06:58–07:21 UTC, complete)

`raw/run2-dispatch.md`:

**Pull path (`ClaimForWorker`)**

| backlog | replicas | claims/s | p50 | p99 | max |
|---:|---:|---:|---:|---:|---:|
| 1,000 | 1 | 72 | 11.1 ms | 59.1 ms | 170 ms |
| 1,000 | 2 | 158 | 11.3 ms | 35.3 ms | 53 ms |
| 1,000 | 4 | 243 | 14.4 ms | 48.3 ms | 62 ms |
| 1,000 | 8 | 347 | 21.2 ms | 56.2 ms | 85 ms |
| 1,000 | 16 | 575 | 25.3 ms | 99.6 ms | 126 ms |
| 10,000 | 1 | 145 | 5.7 ms | 22.5 ms | 58 ms |
| 10,000 | 2 | 303 | 5.9 ms | 15.1 ms | 128 ms |
| 10,000 | 4 | 460 | 7.1 ms | 34.1 ms | 110 ms |
| 10,000 | 8 | 617 | 10.8 ms | 49.4 ms | 135 ms |
| 10,000 | 16 | 764 | 18.7 ms | 58.0 ms | 114 ms |
| 100,000 | 1 | 113 | 5.7 ms | 86.0 ms | 331 ms |
| 100,000 | 2 | 258 | 6.3 ms | 32.8 ms | 95 ms |
| 100,000 | 4 | 442 | 7.6 ms | 25.6 ms | 57 ms |
| 100,000 | 8 | 455 | 14.8 ms | 76.6 ms | 182 ms |
| 100,000 | 16 | 143 | 49.5 ms | **597 ms** | 983 ms |

(The 1,000-backlog cells run only 500 dispatches, B/2, so their
throughput includes more fixed start-up per claim.)

**Push path (`Claim`)**

| backlog | replicas | dispatched | claims/s | p50 | p99 | empty |
|---:|---:|---:|---:|---:|---:|---:|
| 1,000 | 1 | 500 | 44 | 19.1 ms | 84.4 ms | 0 |
| 1,000 | 4 | 503 | 38 | 72.4 ms | 297 ms | 31 |
| 1,000 | 16 | 513 | 39 | 167 ms | 549 ms | 324 |
| 10,000 | 1 | 1,775 | 20 | 45.7 ms | 108 ms | 0 |
| 10,000 | 4 | 2,250 | 25 | 118 ms | 638 ms | 4 |
| 10,000 | 16 | 1,377 | 15 | 537 ms | 2.77 s | 291 |
| 100,000 | 1 | 198 | 2 | 377 ms | 1.33 s | 0 |
| 100,000 | 4 | 183 | 2 | 980 ms | 12.5 s | 3 |
| 100,000 | 16 | 392 | 4 | 2.57 s | 16.1 s | 0 |

The 2- and 8-replica push rows are in the raw file. Push cells that
dispatched fewer than the target hit the 90 s cap.

### 2.3 Reading it

- **The pull claim no longer slows as the backlog grows.** At 1 replica, p50 is
  5.7 ms at both 10k and 100k. Before ADR-0018 Stage 1 it was 27 ms and
  262 ms. Up to 8 replicas, every pull cell's p99 is under ADR-0018's 250 ms
  trigger. Throughput grows with replicas up to roughly 8, and then the database's share of the
  10 vCPUs saturates.
- **16 replicas on this machine is past the knee.** That means 32 backend
  connections plus 16 client goroutines on a 10-vCPU VM that the rest of the
  host was also using.
  - Run 2: 100k/16 dropped to 143/s with p99 597 ms.
  - Run 1 ([`2026-09-26-dispatch-run1-interrupted.md`](benchmarks/2026-09-26-dispatch-run1-interrupted.md)):
    10k/16 dropped to 153/s with p99 1.03 s, while 100k/16 held at 418/s with
    p99 241 ms.

  Which 16-replica cell collapses changes from run to run, so it is host
  contention, not a property of that backlog. It shows that p99 **can** exceed
  250 ms on an oversubscribed database host. That is the condition ADR-0018's
  trigger and `nexus_scheduler_claim_duration_seconds` are meant to catch.
- **The push path is the slow path, and it is not the one the shipped scheduler
  runs.** The scheduler starts with no notifier, so its push loop is off
  (`services/scheduler/main.go`, F81). Workers are served only by the
  pull claim.
  - The push claim is still the unwindowed, fair-share-ordered scan (ADR-0018
    "What this does not change"), so its cost grows with the backlog:
    ~20–45 ms at 10k and ~0.4 s at 100k for 1 replica.
  - Replicas mostly contend for the same head of the queue. `empty` grows with
    replica count, and more replicas add latency, not throughput.
- **Push-path A/B (did ADR-0021 slow it down?).** The push path was benchmarked
  with the code from before quotas (`b7bd493`, built from a clean `git archive`)
  and with `82b9ec8`, alternated old, new, old, new, against one database
  (`raw/ab-push-*.md`, 1,500 dispatches per cell). Mean of the two rounds, p50:

  | backlog / replicas | before quotas (`b7bd493`) | `82b9ec8` |
  |---|---:|---:|
  | 10,000 / 1 | 46.8 ms | 41.9 ms |
  | 10,000 / 4 | 78.7 ms | 68.0 ms |
  | 100,000 / 1 | 367 ms | 351 ms |
  | 100,000 / 4 | 423 ms | 474 ms |

  The two builds are within the run-to-run noise. The slow push numbers are
  the unwindowed scan on this loaded host, not a regression. For reference,
  ADR-0018 (2026-09-24, same machine, host load not recorded) measured the
  push path at 27.7 ms and 271 ms for 10k/1 and 100k/1.
- **Comparison with ADR-0018's Stage 1 table** (same harness, same machine,
  2026-09-24). ADR-0018 recorded 10k/16 at 861/s with p99 63 ms, and 100k/16
  at 866/s with p99 63 ms. Run 2's 10k/16 (764/s, p99 58 ms) is close. Its
  100k/16 is not, for the contention reason above. **Neither run is a capacity
  figure.**

### Quota-bound cells

ADR-0021 quotas, `NEXUS_BENCH_QUOTA=true`. The namespace gets concurrent quotas
far above what the cell reaches, so every claim takes the full quota path
(advisory lock, usage read, check) without being refused. The unlimited
namespace pays only the quota lookup.

- **Grid:** 1,500 dispatches per cell; 500 at the 1,000 backlog.
- **Rounds:** two, alternated unlimited, quota, unlimited, quota
  (`raw/run2-quota-*.md`).
- **Format:** each cell shows round 1 · round 2.

| mode | backlog | replicas | unlimited: claims/s | unlimited: p50 | quota-bound: claims/s | quota-bound: p50 | quota-bound: p99 |
|---|---:|---:|---|---|---|---|---|
| pull | 1,000 | 1 | 121 · 143 | 7.3 · 6.2 ms | 130 · 138 | 6.9 · 6.6 ms | 21.7 · 16.2 ms |
| pull | 1,000 | 4 | 462 · 516 | 8.0 · 7.3 ms | **126 · 127** | **27.8 · 28.7 ms** | 69.0 · 65.5 ms |
| pull | 10,000 | 1 | 143 · 49 | 6.0 · 8.5 ms | 110 · 119 | 7.4 · 7.7 ms | 38.3 · 18.3 ms |
| pull | 10,000 | 4 | 339 · 291 | 8.5 · 11.5 ms | **115 · 128** | **30.0 · 30.2 ms** | 100 · 53.9 ms |
| push | 1,000 | 1 | 74 · 73 | 10.0 · 11.0 ms | 18 · 105 | 33.2 · 9.1 ms | 225 · 17.4 ms |
| push | 1,000 | 4 | 137 · 181 | 24.1 · 18.3 ms | 20 · 129 | 40.8 · 22.9 ms | 974 · 52.7 ms |
| push | 10,000 | 1 | 22 · 28 | 43.4 · 33.2 ms | 17 · 18 | 45.6 · 39.6 ms | 272 · 228 ms |
| push | 10,000 | 4 | 38 · 69 | 77.0 · 42.8 ms | 48 · 51 | 62.9 · 58.8 ms | 345 · 268 ms |

What this shows:

- **This reproduces ADR-0021's finding.** A quota-bound namespace on the pull
  path with several replicas is capped at about **115–130 dispatches/s** for
  that namespace. p50 is about 28–30 ms because it includes the wait for the
  namespace's advisory lock. OPERATIONS §21.8 recorded about 140/s and 24–29 ms
  on 2026-09-25. With 1 replica, there is no one to wait for, and the quota
  path costs little.
- The quota cap applies per namespace. Other namespaces are not serialized
  behind it.
- The 10k/1 unlimited round 2 (49/s, with a 2.6 s maximum) and the
  push quota round 1 (18–20/s) are outliers. Their paired rounds do not repeat
  them, so they show how noisy this host was.

## 3. Inline result vs object-store blob (ADR-0016)

`make bench-inline` (`TestInlineVersusBlob`): 200 round trips per size with
incompressible payloads. Inline = a PostgreSQL INSERT + SELECT of a row whose
`output_ref` carries the payload base64-encoded in JSONB. Blob = the same with
a ~200-byte reference, plus S3 PUT + GET against the local Silo. Run at
2026-09-26 07:32 UTC (`raw/run2-inline.md`):

| payload | inline p50 | inline p99 | blob p50 | blob p99 | inline row bytes |
|---:|---:|---:|---:|---:|---:|
| 64 B | 0.68 ms | 2.85 ms | 3.06 ms | 9.72 ms | 170 |
| 1 KiB | 0.59 ms | 1.22 ms | 2.34 ms | 5.47 ms | 1,452 |
| 4 KiB | 0.92 ms | 2.93 ms | 2.88 ms | 9.00 ms | 5,548 |
| 16 KiB | 1.44 ms | 5.23 ms | 3.63 ms | 16.1 ms | 21,933 |
| **64 KiB** | **2.67 ms** | **7.36 ms** | **3.45 ms** | **11.0 ms** | **87,469** |
| 256 KiB | 8.67 ms | 15.7 ms | 5.79 ms | 12.3 ms | 349,614 |
| 1 MiB | 34.5 ms | 96.3 ms | 16.1 ms | 44.4 ms | 1,398,191 |

This matches ADR-0016 (2026-09-24). The crossover is between 64 KiB and
256 KiB. At the 64 KiB default, inline is still faster by about 0.8 ms at p50,
and blobs carry a fixed floor of about 2–3 ms on this machine. The ADR-0016
figures (for example 64 KiB: 2.44 vs 3.31 ms) are within noise of these. The
decision to keep a 64 KiB default is unaffected.

## 4. Chaos suite

**Re-run 2026-09-26, 07:34–07:53 UTC, in an isolated compose project.**
Raw log: [`raw/chaos-2026-09-26.txt`](benchmarks/raw/chaos-2026-09-26.txt).

**Setup**

- The stack is compose project `nexus-w3l-chaos` on host ports 55881–55889,
  started with `E2E_PROJECT=nexus-w3l-chaos E2E_PORT_BASE=55881
  deploy/e2e/e2e-stack.sh up`. It built every image from this branch and
  ran three agents: CPU, simulated A100 ×2 and simulated A10.
- The suite (`go test -tags chaos ./tests/chaos/`) ran with
  `COMPOSE_PROJECT_NAME=nexus-w3l-chaos` and `NEXUS_COMPOSE_FILE` pointing at
  a `docker compose config` render of the base and e2e compose files (see
  F150 item 4).
- Hardware is as §1. The 1-minute load average was 8.0 at the start and 8.5 at
  the end.

**The three chaos criteria (Phase 1 exit criteria 2 and 3): all passed.**

| test | result | evidence from the log |
|---|---|---|
| `TestKillingAWorkerMidTaskYieldsExactlyOneLogicalEffect` | **PASS** (61.5 s) | The worker holding `slow` was SIGKILLed mid-task. The attempt became LOST and a retry SUCCEEDED. `seed effects=1 executions=1`, `slow effects=1 executions=2`, `settle effects=1 executions=1`. The task ran twice and produced exactly one logical effect. |
| `TestKillingAWorkerLeavesNoCapacityLeaked` | **PASS** | No DEAD worker holds committed CPU, and no reservation belongs to a terminal attempt. |
| `TestSchedulerReplicasNeverDoubleDispatchUnderLoad` | **PASS** (17.6 s) | The test scaled to 3 schedulers (plus the GPU scheduler) and ran 8 concurrent dispatch storms. It sampled the ledger 21 times under load. The peak was 18 overlapping dispatches, with no attempt holding two leases and no worker over-committed. Claims per replica: 291, 73, 67, 25. |

**The example sweep (`TestEveryCommittedExampleActuallyRuns`): FAILED.**
Six of 14 examples reached SUCCEEDED within the test's 120 s per-example
budget:

- batch_inference, chaos_effect, fanout_scale (503 nodes), hello_dag,
  idempotent_effect, ml_training_pipeline (28 nodes).

For the other eight:

- **Slow, then succeeded.** credit_risk_ml, csv_sales_pipeline,
  dispatch_storm, fan_out_map, ten_node_mixed and image_processing_gpu were
  still RUNNING at 120 s and had **SUCCEEDED** when checked after the suite.
  The test runs examples one after another on three workers, and the unfinished
  earlier runs kept occupying those workers. On this loaded host, 120 s was too
  short. This is not a correctness failure.
- **gpu_scheduling: its audit task FAILED.** The audit maps worker hostnames to
  GPU models by name pattern (`agent-gpu-a10`, `agent-gpu-`,
  `examples/gpu_scheduling/dag.py`). Under compose the agent hostname is the
  container ID, so every job was reported as "ran on … which has no
  nvidia-a100". The scheduler placed the jobs on the right GPU kinds
  (workers `f00936a85ad2`: nvidia-a100 ×2 and `22840d34bfce`: nvidia-a10). The
  example assumes Kubernetes pod names. F150 item 5.
- **ml_capacity_test: `generate_data` FAILED.** The error was `python3:
  executable file not found`. The compose agents are Alpine images without
  Python, and the example runs `python3` in the subprocess executor. F150
  item 5.

**History.** Criteria 2 and 3 were first met in CONTRACT-FINDINGS F80 and F82,
and CI runs the suite in the `e2e` job (`.github/workflows/ci.yml`). This local run is the most recent
recorded evidence.

## 5. Restore and point-in-time recovery

**Not re-run for this page.** The drills are destructive and take a full
stack. They were run on 2026-09-25 on the same machine (same model, CPU and
Docker VM limits as §1), in an isolated compose project, and are recorded with
their datasets and phase timings in
[OPERATIONS.md §4.6](OPERATIONS.md#46-measured-results--laptop-measurements-not-production-claims).
The JSON reports are in [`docs/drills/`](drills/). In summary, all as laptop
figures. *Medium* is 54,400 attempts and a 127 MiB database; *large* is 420,800
attempts and a 914 MiB database; both carry about 2 GiB of artifacts.

| drill | dataset | RPO | RTO |
|---|---|---:|---:|
| PITR to a timestamp | medium / large | n/a (exact to target) | 20.6 s / 24.5 s |
| PITR, primary lost (SIGKILL + data directory deleted) | medium / large | 33.8 s / 44.7 s | 29.4 s / 32.3 s |
| Logical restore (`restore-drill.sh --rto`, database and bucket lost) | medium / large, 2 GiB artifacts | the dump interval | 39.0 s / 45.0 s |

OPERATIONS §4.6 lists what these do **not** prove. Examples: a real remote
object store, a production-size database, and restore under concurrent load.

## 6. What would make these production figures

- The same harness on production-class hardware: a database on its own host
  with real storage, clients over a real network, and nothing else running.
- End-to-end measurements (submit to completion, with real workers). §2
  measures one claim transaction only.
- The push path windowed, or removed, since the shipped scheduler does not run it.

Findings from this re-run are recorded as F150 in
[CONTRACT-FINDINGS.md](../CONTRACT-FINDINGS.md).
