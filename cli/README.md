# nexusctl

Command line interface for Nexus. Go, standard library only plus `gopkg.in/yaml.v3`.

## Build

```bash
go build -o ./nexusctl ./cli/cmd/nexusctl
```

## Configure

Resolution order, highest wins:

1. flags — `--endpoint`, `--namespace`, `--api-key`, `--context`
2. environment — `NEXUS_ENDPOINT`, `NEXUS_API_KEY`, `NEXUS_NAMESPACE`, `NEXUS_CONTEXT`, `NEXUS_CONFIG`
3. `~/.nexus/config.yaml` (or `--config`)
4. built-in defaults (`http://localhost:8080`, namespace `default`)

Flags beat the environment beats the file — otherwise a stale config file would
silently override an explicit `--endpoint`, and you would spend an afternoon
wondering why you were talking to the wrong cluster.

```yaml
# ~/.nexus/config.yaml
current-context: staging
namespace: default
contexts:
  staging:
    endpoint: http://nexus.staging.internal:8080
    api_key: nx_...
  prod:
    endpoint: https://nexus.prod.internal
    namespace: prod
```

```bash
nexusctl config view              # shows where each value came from
```

**API key material is never printed** — not by `config view`, not in `--output
json`, not in an error, not in a log. `config view` reports only whether a key is
set and its length, which is enough to diagnose an auth failure without putting a
key on a screen that may be shared or recorded.

## Commands

```
workflow submit <file> [--dry-run]   Submit a spec (JSON, YAML, or .py)
workflow compile <file> [--show]     Print exactly the spec the server will see
workflow versions <name>             List registered spec versions

run create <name> [--param K=V]      Start a run
           [--idempotency-key KEY] [--wait]
run get <run-id>                     Detail with per-node states
run list [--state S] [--spec N]      List runs
run cancel <run-id>                  Cancel
run retry <run-id> [--node ID]       Retry from failed nodes

worker ls                            Fleet with capacity and heartbeat age
capacity                             Advertised vs committed
logs <attempt-id> [--follow]         Stream an attempt's logs
events <run-id> [--follow]           Event history, or live tail over SSE
config view                          Resolved configuration
version
```

Global: `-o, --output table|json|yaml`. Table is the default and is formatted for
humans — its columns may change, so **scripts should use `--output json`**.

## `workflow compile --show`

The command worth knowing:

```bash
nexusctl workflow compile dag.py --show
```

It prints the canonical document and its content address, and contacts nothing.
ADR-0004 names this specifically as the mitigation for *"my Python compiled to a
graph I did not expect"*. When the graph surprises you, this shows you what the
control plane will actually parse.

For a `.py` file, nexusctl shells out to **your** interpreter (`--python`, default
`python3`). It does not embed a Python runtime and does not parse Python: the
platform's guarantee is that only your own process executes your code, and the CLI
keeps that true.

## Error messages

An orchestrator CLI that prints a bare `500` is useless. Every non-2xx carries the
server's own message, the request id for correlating with control-plane logs, and —
where the cause is knowable — a concrete next step:

```
$ nexusctl run get run-xyz
nexusctl: GET /v1/namespaces/prod/runs/run-xyz failed: HTTP 500: scheduler panic
  request id: req-abc123
  This is a control-plane fault. Quote the request id above when reporting it.
```

A 401 names `NEXUS_API_KEY`; a 422 suggests `--dry-run`; a 409 explains the
idempotency key; an unreachable endpoint names the endpoint it tried. Locally
detectable spec problems fail before any request is made.

## Tests

```bash
go test ./cli/...
```

Covers arg parsing (including flags before and after the operand), output
formatting, config precedence, canonicalization parity with the Python SDK, error
message content, and that key material never reaches any output stream.
