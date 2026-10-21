# Contributing to KGF Nexus

Thanks for helping. Nexus is under active development and not yet
production-ready (see the status note in the [README](README.md)), so the bar
for a change is that it is correct, tested and documented, not that it is
large.

## Before you start

- **Bugs and features:** open an issue first for anything bigger than a small
  fix, so the approach can be agreed before you write it.
- **Security issues:** do not open a public issue. Follow [SECURITY.md](SECURITY.md).
- **Design decisions** are recorded as ADRs in [docs/adr](docs/adr). A change
  that alters one (a new trust boundary, a schema contract, a scheduling rule)
  needs a new ADR or an addendum in the same pull request.
- **Known defects** are tracked in [CONTRACT-FINDINGS.md](CONTRACT-FINDINGS.md).
  If you fix one, update its entry; if you find one you are not fixing, add it.

## Development setup

You need Go (the version in `go.mod`), Node 24, Python 3.11+, Docker, and a
PostgreSQL server for the integration tests. The docs for running the whole
stack locally are in [deploy/README.md](deploy/README.md); `make quickstart`
brings it up on a fresh checkout.

```sh
make build        # every Go package
make test         # Go, console and Python SDK unit tests
make lint         # go vet
make up           # the full stack with your own keys
```

`make help` lists every target.

## Checks a pull request must pass

CI runs these on every pull request, and the single required check is the
`quality gate` job. Run the ones your change touches before you push:

| Change | Run |
|---|---|
| Go code | `make build lint test`, and the integration tests for the packages you touched (`go test -tags integration ./<pkg>/...` against a disposable database) |
| Database migrations | the migrations integration tests, and an up → down → up round trip |
| Protobuf | `make proto` (commit the regenerated `gen/`), `make proto-lint`, `make proto-breaking` |
| Console (`ui/`) | `make ui-test`, `npm --prefix ui run typecheck`, `make ui-build` |
| Python SDK | `make sdk-test` |
| Helm chart | `make helm-validate` |
| Terraform | `make tf-validate` and `make tf-lint` |
| Docs | `make check-doc-refs` and `make docs` (strict build and link check) |
| Workflows and scripts | `make actionlint`, `make shellcheck`, `make preflight-test` |
| Any source file | `make license-check` |

## Conventions

- **License headers.** Every Go, TypeScript, Python, shell, SQL and proto file
  starts with:

  ```
  // Copyright 2026 Khuriwal Group
  // SPDX-License-Identifier: Apache-2.0
  ```

  (with the comment syntax of the language). `make license-check` enforces it.
- **Migrations** live in `services/migrations` as numbered `up`/`down` pairs.
  They are additive (a running older release must keep working), every `up`
  has a `down`, and from 0038 on they grant through `nexus_role()` rather than
  naming `nexus_<service>` roles. See
  [services/migrations/README.md](services/migrations/README.md).
- **The runbook.** Operational behaviour is documented in
  [docs/OPERATIONS.md](docs/OPERATIONS.md). Cross-references use
  `OPERATIONS §N`; `make check-doc-refs` fails on a section that does not exist.
- **Honest output.** A path that is simulated, sampled or not yet implemented
  says so in its output, its docs and its tests. Never describe it as live.
- **Tests come with the change.** A bug fix includes a test that fails without
  it.
- **Commits** have a sentence-case subject that says what changed, and a body
  that says why. Keep unrelated changes in separate commits.

## Licensing of contributions

Nexus is licensed under the [Apache License 2.0](LICENSE). Unless you state
otherwise, any contribution you submit is licensed under the same terms
(section 5 of the license), with no additional terms or conditions.

## Code of conduct

This project follows the [Code of Conduct](CODE_OF_CONDUCT.md). By taking
part you agree to uphold it.
