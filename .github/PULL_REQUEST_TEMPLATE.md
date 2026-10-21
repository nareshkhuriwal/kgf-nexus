## What and why

<!-- What this changes, and the problem it solves. Link the issue: Fixes #123 -->

## How it was verified

<!-- The checks you ran (see CONTRIBUTING.md) and anything you verified by hand. -->

- [ ] Tests added or updated; a bug fix includes a test that fails without it
- [ ] The relevant `make` checks pass locally (build/lint/test, ui, sdk, helm-validate, tf-validate, docs …)

## Contracts and docs

- [ ] No change to a public contract (API, workflow spec, chart values, database schema), **or** it is documented here and backward compatible
- [ ] Migrations are additive, have a `down`, and grant through `nexus_role()`
- [ ] Docs updated (OPERATIONS runbook, ADR or addendum, CONTRACT-FINDINGS) where behaviour changed
- [ ] New source files carry the SPDX license header
