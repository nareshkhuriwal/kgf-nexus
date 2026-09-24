# ADR-0013: Go remains the control-plane and agent language

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-24
- **Deciders:** platform team
- **Closes:** ARCHITECTURE.md §12 question 1
- **Refines:** ADR-0001

## Context

§12 asked Go or Rust "before Phase 1". Phase 1 happened in Go: six services, the
worker agent, the CLI and the artifact GC are Go in one module, the plane
contract is generated Go (`gen/`), and every correctness guarantee that has been
proven so far -- the atomic claim (F82/F86), lease fencing, the chaos suite
(F80), fair share (ADR-0017) -- was proven against the Go implementation. The
question is therefore no longer "which language" but "is there evidence that
justifies a rewrite of any part", and the only part the original question
singled out was the **agent footprint**.

## Evidence (measured on the proposing branch)

Go 1.27.1, `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' ./agent/cmd/nexus-agent`,
Apple M4 Pro laptop:

| Build | Size |
|---|---:|
| linux/amd64, static, stripped (what ships in the image) | 16.6 MB |
| darwin/arm64, static, stripped | 15.7 MB |
| darwin/arm64, default flags (symbols + DWARF) | 23.1 MB |

| Startup (darwin/arm64 stripped) | Value |
|---|---:|
| exec → exit of `nexus-agent -version`, median of 30 (includes the Python harness's fork/exec) | 19 ms (p90 20 ms) |
| max RSS of that process (`/usr/bin/time -l`) | 16.9 MB |

The agent is a single static binary with no runtime prerequisite on the host,
which was the requirement ADR-0001 cited Go for. Steady-state RSS of an agent
supervising tasks was **not** measured here (it needs a live control plane);
the 17 MB figure is a floor, not a working-set number.

What the agent binary is made of explains the size and why a rewrite would not
remove most of it: gRPC + protobuf (the plane contract), the MinIO client (the
result store, ADR-0008), TLS, and the executors. A Rust agent would carry tonic
+ prost + an S3 client + rustls for the same functions.

## Options

1. **Keep Go for control plane and agent.** *Chosen.*
2. **Rust agent, Go control plane.** Rejected: the whole stripped binary is 16.6 MB, so
   a Rust agent could save at most around 10 MB of disk and a similar amount of
   baseline RSS per host (an estimate, not measured) -- negligible next to the task workloads the agent
   supervises (a single task image is usually hundreds of MB) -- while splitting
   the plane contract across two codegen stacks, duplicating the executor
   conformance suite, and re-proving lease fencing, the WAL and the result
   store in a second implementation. The per-host cost it removes is not a
   bottleneck anyone has reported; the cost it adds is permanent.
3. **Rust everywhere.** Rejected: a rewrite of proven code with no failing
   requirement behind it.

## Decision

Go stays the language of the control-plane services and the worker agent
(ADR-0001's split is confirmed). Python stays the first SDK. Revisit only on a
measured requirement the Go agent cannot meet -- e.g. a target host class
(edge devices) with a hard memory budget below the measured RSS -- and then for
the agent alone, behind the unchanged proto contract.

## Consequences

**Positive** -- one toolchain for every server-side component; the proven
implementations stay the only implementations; codegen from one `proto/`.

**Negative** -- the agent is larger and has a GC; a memory-constrained edge
target would need its own agent. The measurements above are from one Apple
Silicon laptop (darwin/arm64 build); a linux/amd64 static build will differ by
a few MB and was not measured here.

**Neutral** -- nothing in the code changes.
