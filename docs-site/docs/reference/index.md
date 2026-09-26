# Reference

All four references are generated at build time from the source of truth,
so none of them can drift from the code:

| Reference | Generated from | Kept honest by |
| --- | --- | --- |
| [REST API](api/index.md) | `api/openapi.yaml` (OpenAPI 3.1), also served at `GET /v1/openapi.yaml` | a Go test that fails when a registered route is missing from the document, or the document lists one that does not exist, or methods, path parameters, permissions or examples disagree |
| [CLI (`nexusctl`)](cli.md) | `nexusctl help` of a binary built from this checkout | the binary itself |
| [Python SDK](sdk.md) | the SDK's docstrings and type annotations (mkdocstrings, static analysis) | `mypy --strict` on the package |
| [Workflow spec](workflow-spec.md) | `schemas/workflow-spec-v1.json` | the gateway enforces this schema, and a test keeps its embedded copy identical |

## Exit codes (`nexusctl`)

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | any other failure: a server fault, a failed `--wait`, an invalid spec, unsupported constructs in `import` without `--allow-partial` |
| 2 | usage error; nothing was sent |
| 3 | a destructive command was not confirmed |
| 4 | not found (HTTP 404) |
| 5 | conflict (HTTP 409, or a refused state transition) |
| 6 | authentication or authorization (HTTP 401/403) |
| 7 | control plane unreachable, or 502/503/504 |
| 130 | interrupted |

Codes are only ever added, never renumbered. Scripts should use `-o json`,
because table output is formatted for people and its columns may change.
