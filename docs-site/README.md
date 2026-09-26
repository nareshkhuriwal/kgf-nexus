# docs-site

The KGF Nexus documentation site. Build and check it with:

```bash
make docs          # venv from the lock file, strict build, link check
make docs-serve    # live preview on http://127.0.0.1:8000
```

`make docs` creates `docs-site/.venv` from `requirements.txt`, installing with
`--require-hashes`. It builds `nexusctl` from this checkout for the CLI
reference, runs `mkdocs build --strict` into `docs-site/site/`, and then runs
`scripts/check_links.py` over the result. Nothing is published anywhere.

## What is generated, and from where

| Page(s) | Hook | Source |
| --- | --- | --- |
| `reference/api/*` and `reference/api/openapi.yaml` | `hooks/openapi_reference.py` | `api/openapi.yaml` |
| `reference/cli.md` | `hooks/cli_reference.py` | `nexusctl help` of a binary built from this checkout (`$NEXUS_DOCS_NEXUSCTL`, or `go build`) |
| `reference/sdk.md` (bodies) | mkdocstrings | the SDK's docstrings, by static analysis. The package is never imported. |
| `reference/workflow-spec.md` | `hooks/schema_reference.py` | `schemas/workflow-spec-v1.json` |
| `design/*`, `operations/runbook.md`, `operations/deploy.md`, `operations/tls.md`, `operations/terraform/*` | `hooks/repo_docs.py` | `docs/*.md`, `docs/adr/*.md`, `deploy/**/README.md`: rendered **in place**, not copied |

`repo_docs.py` rewrites each relative link in a rendered repository document.
A link to another rendered document becomes a site link. A link to any other
repository file becomes a GitHub URL on `main`. A link to a file that does not
exist fails the strict build. Hand-written pages link to repository files with
`[text](repo:path)`, which is checked the same way.

## Why MkDocs Material

The requirement was a maintained static generator, pinned, with the least
toolchain weight. The repository already needs Python 3.11+ for the SDK, and
Node 22 only for the operator console.

- **MkDocs Material, the choice.** It is pure Python and installs into a venv
  in seconds. mkdocstrings documents the typed SDK by static analysis
  (griffe), without importing it. Build hooks are plain Python, which is how
  the OpenAPI, CLI and schema references are generated without extra
  plugins. `--strict` together with MkDocs 1.6 link and anchor validation
  gives a real failure on a broken reference.
- **Docusaurus, rejected.** It would add a second Node toolchain with its own
  lock file of a few hundred packages to a docs build. Documenting the Python
  SDK from docstrings would need a separate generator. Its React/MDX theming
  buys nothing this site uses.
- **Sphinx, rejected.** It is also Python and is excellent for API docs, but
  the repository's docs are Markdown. Rendering them through MyST works, yet
  mixing reStructuredText conventions with GitHub-style Markdown costs more
  than it gains here.

**Maintenance status, and the plan.** The versions are pinned: MkDocs
**1.6.1** and Material **9.7.7**, with the full transitive set hash-locked in
`requirements.txt`. The Material team has said that MkDocs 2.0 will not be
backward compatible (plugins and theme overrides break). They maintain
Material 9.x on MkDocs 1.x and are building Zensical as its successor; the
build banner points to their analysis. Consequences:

- Stay on the 1.x line. The lock file prevents an accidental upgrade to
  2.0, and `NO_MKDOCS_2_WARNING=1` silences the banner in `make docs`.
- The site uses only portable pieces: Markdown with pymdown-extensions,
  mkdocstrings, and four small hooks with no theme overrides. So a move to
  Zensical or another generator is a configuration exercise, not a rewrite.
  Revisit when Zensical supports mkdocstrings and hooks.

## Updating the lock

```bash
# edit requirements.in, then:
uv pip compile docs-site/requirements.in --generate-hashes --python-version 3.11 \
    -o docs-site/requirements.txt
make docs
```

All documentation tooling is under permissive licences (MkDocs BSD-2-Clause,
Material MIT, mkdocstrings and griffe ISC, pymdown-extensions MIT). None of
it is part of any shipped artifact.
