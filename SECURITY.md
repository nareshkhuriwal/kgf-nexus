# Security policy

## Supported versions

Nexus is pre-1.0 and under active development. Security fixes are made on
`main` and included in the next release; there are no long-term support
branches yet.

| Version | Supported |
|---|---|
| `main` | Yes |
| Latest release | Yes |
| Older releases | No |

Nexus has not passed its production acceptance gates. The current list of
known gaps is in [docs/README.md](docs/README.md#current-state).

## Reporting a vulnerability

**Please do not open a public issue, discussion or pull request for a
security problem.**

Report it privately through GitHub: open the repository's **Security** tab
and choose **Report a vulnerability**. That creates a private advisory that
only you and the maintainers can see.

Please include:

- the affected component (api-gateway, scheduler, agent, console, Helm chart,
  Terraform, SDK, …) and the version or commit;
- what an attacker can do, and what access they need first;
- steps to reproduce, or a proof of concept;
- any mitigation you know of.

## What happens next

The maintainers will acknowledge the report, confirm whether it is a
vulnerability, and agree a fix and a disclosure date with you. Fixes are
released through the normal release process, and the advisory is published
when the fix is available. Credit is given to reporters who want it.

## Scope notes

- The `deploy/docker-compose.yml` stack and `deploy/.env.example` use
  deliberately obvious **development-only** credentials. Running them
  unchanged on a reachable host is a misconfiguration, not a vulnerability.
- Hardening guidance for real installations is in
  [docs/OPERATIONS.md](docs/OPERATIONS.md) (TLS, row-level security, runtime
  database logins, Pod Security, network policies) and
  [docs/INSTALL-AKS.md](docs/INSTALL-AKS.md).
