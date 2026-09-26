# ADR-0024: Tenant SSO (OIDC and SAML 2.0), IdP group mapping, service accounts and granular identity RBAC

- **Status:** Proposed — pending owner sign-off (implemented on the proposing branch)
- **Date:** 2026-09-25
- **Deciders:** platform team
- **Refines:** ADR-0012 (module permissions and role bindings), migration 0007
  (human identity), migration 0013 (tenants)
- **Closes:** the "Current boundary" list in docs/RBAC-IMPLEMENTATION.md
  (service-account bindings, per-token ceilings, delegated identity
  administration, cross-namespace owners, cluster permissions, users
  pagination) and CONTRACT-FINDINGS F136–F137

## Context

Humans sign in with an email and a password only. Migration 0007 said OIDC
"is deliberately NOT implemented here" and shaped `users` so that an external
subject could bind to it later (`password_hash` nullable, one row per
namespace and email). Every customer that runs its own identity provider
(Entra ID, Okta, Google Workspace, Keycloak, ADFS) therefore has to maintain a
second set of passwords, cannot enforce its own MFA and conditional-access
policy, and cannot offboard a person from one place.

RBAC (ADR-0012) is namespace-scoped human module RBAC with four gaps that the
SSO work depends on:

1. Identity administration is reserved to unbound administrators: the guard
   rejects any bound principal on `users.manage`, `tokens.manage` and
   `roles.manage`, so a tenant cannot delegate "manage the SSO mapping" to a
   non-administrator.
2. Machine credentials (scoped tokens) carry a coarse role plus
   `read|write|admin_write` scopes. There are no service accounts and no
   per-token permission ceilings.
3. A person belongs to exactly one namespace. There is no way to let a
   platform team act in several customer namespaces without an account per
   namespace.
4. `GET /users` has a hard cap and no cursor, so the role-assignment picker
   stops at 500 accounts.

Constraints from the code: the gateway is the only authenticator (ADR-0011);
the console is a single-page app that presents a bearer token from
localStorage and talks to the gateway on the same origin through nginx;
sessions are opaque `nxsess_` tokens stored as sha256; namespaces are the
tenancy boundary and every tenant owns exactly one namespace; migrations are
additive (ADR-0011 rule 5).

## Decision

### 1. Libraries (pinned in go.mod)

| Purpose | Library | Version | Why |
| --- | --- | --- | --- |
| OIDC discovery, JWKS, ID-token verification | `github.com/coreos/go-oidc/v3` | v3.21.0 | The de-facto Go RP library (used by Kubernetes, Dex, oauth2-proxy); verifies signature against the discovered JWKS with key rotation, `iss`, `aud`, `exp`; no known advisories |
| OAuth 2.0 code exchange, PKCE | `golang.org/x/oauth2` | v0.36.0 | Go project library; has `GenerateVerifier`/`S256ChallengeOption`; ≥ v0.27.0 fixes GHSA-6v2p-p543-phr9. v0.37.0 needs Go 1.26 and the build image is 1.25 |
| JOSE (transitive) | `github.com/go-jose/go-jose/v4` | v4.1.5 | ≥ 4.1.4 fixes GHSA-78h2-9frx-2jm8 |
| SAML 2.0 SP | `github.com/crewjam/saml` | v0.5.1 | Maintained, widely deployed; all four published advisories (GHSA-4hq8-gmxx-h6w9, -j2jp-wvqg-wc2g, -5mqj-xc49-246p, -267v-3v32-g6q5) are fixed at ≤ 0.4.14; validates XML round-trip safety (`xml-roundtrip-validator`) before signature checks |
| XML signature (transitive) | `github.com/russellhaering/goxmldsig` | **v1.6.1, forced** | crewjam/saml 0.5.1 declares v1.4.0, which is inside GHSA-479m-364c-43vc (signature bypass, all ≤ 1.5.0, published 2026-03-18). The explicit require in go.mod lifts it to the fixed release |

Rejected: writing JWT or XML-DSig verification ourselves (the history of both
is a history of signature-wrapping and algorithm-confusion bypasses);
`russellhaering/gosaml2` (smaller user base, same dsig dependency, no
advantage); a sidecar such as oauth2-proxy (does not know tenants, cannot do
per-tenant IdPs or group-to-role sync, and would make the gateway trust a
header).

### 2. Where SSO configuration lives

`sso_providers` (migration 0032), one row per tenant IdP, owned by the
api-gateway like every identity table:

- `kind` `oidc|saml`, `slug`, `display_name`, `enabled`, `revision`.
- OIDC: `issuer`, `client_id`, `client_secret_ref`, `scopes`.
- SAML: `idp_metadata_xml` (pasted by the administrator; the gateway never
  fetches metadata from a tenant-supplied URL), `idp_entity_id` (derived).
- Shared policy: `allowed_domains`, `jit_provisioning`,
  `link_by_verified_email`, `groups_claim`, `revoke_sessions_on_role_loss`.

**Secrets are references, never values.** `client_secret_ref` is
`file:<name>`, resolved to `$NEXUS_SSO_SECRET_DIR/<namespace>/<name>` (a
mounted Kubernetes/Compose secret), or `env:NEXUS_SSO_SECRET_<NAME>` only when
the operator sets `NEXUS_SSO_ALLOW_ENV_SECRETS=true`. The namespace directory
is what stops tenant A from pointing its provider at tenant B's secret, and
the env prefix is what stops a tenant administrator from naming
`NEXUS_DATABASE_URL` and having the gateway send it to an issuer they control.
The API never returns a secret; it reports whether the reference resolves.

**Outbound fetches are SSRF-bounded.** Issuer discovery and the token
endpoint use a dedicated HTTP client: https only, 10 s timeout, 1 MiB body
cap, redirects refused, and a dial-time check that refuses loopback, private,
link-local and unspecified addresses (checked on the resolved IP, so DNS
rebinding cannot bypass it). `NEXUS_SSO_ALLOW_INSECURE_ISSUERS` and
`NEXUS_SSO_ALLOW_PRIVATE_ISSUERS` exist for development and the Keycloak
end-to-end suite and log a warning at startup.

`(issuer, client_id)` is unique across all tenants, and a SAML IdP entity ID
is unique across tenants, so two tenants can never share one IdP application
and confuse whose users are whose.

### 3. The browser flow

```
SPA /login?tenant=<ns>  --GET-->  /v1/auth/sso/tenants/{ns}/providers   (public list, [] for unknown)
Button               --navigate--> /v1/auth/sso/{id}/start?return_to=/runs
  gateway: state (32 B), nonce (32 B), PKCE verifier (S256), binding cookie (32 B)
           stores sso_login_states{sha256(state), provider, nonce, verifier,
                                   saml_request_id, return_to, sha256(binding), expires +10 min}
           Set-Cookie nexus_sso_bind (HttpOnly, SameSite=Lax, Secure on https, Path=/v1/auth/sso)
  302 -> IdP authorize (response_type=code, code_challenge, state, nonce)
        or SAML AuthnRequest (HTTP-Redirect binding, ID recorded)
IdP  --302/POST--> /v1/auth/sso/oidc/{id}/callback   or   /v1/auth/sso/saml/{id}/acs
  gateway: consume state exactly once (UPDATE … consumed_at IS NULL AND expires_at > now())
           OIDC: provider in path == provider in state, binding cookie matches,
                 RFC 9207 `iss` checked when present, code exchanged with PKCE verifier
           verify, provision/link, sync groups, audit
           issue a one-time handoff (sha256 stored, 60 s, bound to the same binding hash)
  303 -> {NEXUS_PUBLIC_URL}/sso/complete#handoff=…    (fragment: never logged, never in Referer)
SPA  --POST (same origin, cookie sent)--> /v1/auth/sso/exchange {handoff}
  gateway: consume handoff once, binding cookie must match, create the session
  -> {token, expires_at, user, return_to}   (same shape as /v1/auth/login)
```

Sessions reuse `GatewayCreateSession` unchanged; `sessions.sso_provider_id`
(new, nullable) records which IdP minted one, which logout needs.

Per-provider redirect and ACS URLs (`…/oidc/{id}/callback`,
`…/saml/{id}/acs`) are the RFC 9700 mix-up defence: a response for provider A
can only be processed as provider A.

**Why a handoff and not a cookie session.** The console authenticates every
request with a bearer token (sessions, API keys and scoped tokens share one
code path, and the CSRF-free property of the REST API depends on it). Turning
the SSO result into a cookie would give the gateway a second, ambient
credential and make every mutating route CSRF-relevant. The handoff keeps the
existing credential model and costs one extra round trip.

**Why bind to a cookie.** Login CSRF: an attacker completes a flow with their
own IdP account and delivers the callback (or SAMLResponse) to a victim, who
would then work inside the attacker's account. The state is bound to the
browser that started the flow; OIDC callbacks check it immediately (a top-level
GET carries a Lax cookie), and the handoff exchange checks it again. A SAML
POST from the IdP is cross-site, so Lax cookies are absent there; the check
at the exchange still refuses a handoff produced by another browser.

**Return URLs.** `return_to` must be a relative path: it starts with a single
`/`, contains no `\`, no scheme, no control characters, no `//` prefix, at
most 512 bytes, and is validated on start and again by the SPA. The gateway
itself only ever redirects to fixed paths under `NEXUS_PUBLIC_URL`.

### 4. Token and assertion validation

**OIDC** (`services/api-gateway/sso/oidc.go`): authorization code flow with
PKCE S256, `state`, `nonce`; confidential client (secret via reference);
`go-oidc` verifies the signature with the discovered JWKS (asymmetric
algorithms only: RS/PS/ES 256–512, EdDSA — never `none` or HS*), `iss`,
`aud` and `exp`. The gateway additionally checks `nonce` equals the stored
nonce, `azp` equals the client when present or when `aud` has several
values, `iat` is no more than 10 minutes old, and applies a 2-minute clock
skew to `exp`/`iat`. The ID token is the only source of identity; the
access token is not used.

**SAML** (`services/api-gateway/sso/saml.go`): SP-initiated only
(`AllowIDPInitiated=false`). Before crewjam parses anything the gateway
checks the decoded XML (after crewjam's round-trip validation) has exactly one
`Assertion`, no `EncryptedAssertion`, and that the assertion carries its own
`ds:Signature`. A Response-level signature, when present, is removed before
the library runs so that crewjam's "signed response implies trusted assertion"
shortcut can never apply: the **assertion signature is always what is
verified**, against the certificates in the stored IdP metadata only
(KeyInfo in the message is never trusted). Crewjam then checks `Destination`,
`Issuer`, `InResponseTo` (against the request ID stored with the state),
`Recipient`, `NotBefore`/`NotOnOrAfter` with 180 s skew and the IssueInstant
age. The gateway adds what crewjam skips: at least one bearer
`SubjectConfirmation` must exist (crewjam's per-confirmation checks are
vacuous with none), an `AudienceRestriction` must exist and name this SP
(crewjam accepts an assertion with none), and a `NameID` must be present.

**Replay.** OIDC: the state (and so the nonce and code verifier) is consumed
exactly once. SAML: the state is consumed exactly once, and the assertion ID
is inserted into `sso_assertion_replay` (primary key on provider and ID, kept
until `NotOnOrAfter` + skew), so the same assertion cannot be used twice even
against a second state.

### 5. Provisioning and account linking

Identities live in `user_identities(provider_id, subject)` → `users.id`,
unique per provider (not per issuer: several tenants legitimately use
`https://accounts.google.com`). A trigger enforces that the user, the
provider and the identity are in the same namespace. On login:

1. Identity found → that user (refused if disabled).
2. Not found, and a user with the asserted email exists in the tenant → link
   **only if** the provider has `link_by_verified_email`, the IdP asserted
   `email_verified=true` (OIDC claim; SAML attribute of that name), and the
   email domain is in `allowed_domains`. Otherwise refused with
   `account_exists_unlinked` and audited. Never linked on email alone.
3. Not found, no user → created **only if** `jit_provisioning` and the domain
   is allowed. JIT users have no password, account role `reader`, and an
   explicit (possibly empty) role binding, so they never fall back to legacy
   reader permissions: no mapped group means no module access.

Everything is resolved inside the provider's namespace. The provider row, not
anything in the token, decides the tenant; an IdP configured by tenant A can
only ever produce sessions in tenant A, whatever email or tenant claim it
asserts.

### 6. IdP group mapping

`sso_group_mappings(provider_id, idp_group, role_name)`, replaced as a set
with a revision check. On every SSO login the gateway computes the mapped
roles from the asserted groups (`groups_claim`, default `groups`) and writes
them to `rbac_user_access.idp_roles` (new column), separate from
administrator-assigned `roles`. Effective roles are the union. Removing a group
at the IdP removes the role at the next login; when
`revoke_sessions_on_role_loss` is set, the user's other sessions are revoked
in the same transaction. Every change writes `user.roles.idp_sync` with the
added and removed roles. Mapping to Administrator is refused: the
Administrator role is tied to the account role and last-administrator
protection, and an IdP group change must not be able to create or destroy a
recovery administrator.

### 7. Granular identity permissions and delegation

The coarse grants are split (the old identifiers were never storable in a
custom role, so no stored role changes meaning):

| Old | New |
| --- | --- |
| `users.manage` | `users.create`, `users.update`, `users.enable`, `users.disable`, `users.sessions.revoke`, `users.password.reset`, `users.roles.assign` |
| `roles.manage` | `roles.create`, `roles.update`, `roles.delete` |
| `tokens.manage` | `tokens.create`, `tokens.revoke` |
| — | `service_accounts.view/create/update/disable/roles.assign` |
| — | `sso.view`, `sso.configure`, `sso.mappings.update` |
| — | `owners.view`, `owners.grant` |
| `infrastructure.*` for clusters | `clusters.view`, `clusters.configure` (0032 backfills them into every custom role holding the infrastructure grant, so nobody loses access) |

Budgets: the namespace budget is the quota (`quotas.view`, `quotas.manage`,
`usage.view`, ADR-0021), which is already separate. No `budgets.*` grant is
added because there is no budget API behind it (RBAC-DESIGN: no decorative
grants).

Custom roles may now hold identity permissions. The guard's blanket refusal
is replaced by three rules, each enforced in the handler inside the
namespace's advisory-lock transaction:

- **No escalation by content:** a role can only be created, edited or assigned
  if every grant in it is one the actor holds (unchanged rule, now applied to
  service accounts, tokens and group mappings too).
- **No escalation by target:** a delegated (non-Administrator) actor can only
  act on an account whose effective grants are a subset of its own, and never
  on an Administrator. Otherwise a delegate with `users.password.reset` could
  take over the recovery administrator.
- **Reserved:** `owners.grant` and creating Administrator accounts need an
  Administrator (or Owner); they cannot be delegated to a custom role.

### 8. Service accounts and token ceilings

`service_accounts` (namespace, name, description, disabled, revision) and
`service_account_roles`. `scoped_tokens` gains nullable `service_account_id`
and `permissions`. Authorization:

- Service-account token: grants = (union of the account's current roles) ∩
  (the token's `permissions`). Disabling the account or narrowing its roles
  takes effect on the next request; the ceiling never widens.
- Legacy scoped token with `permissions`: grants = (legacy role/scope
  allowances) ∩ `permissions`.
- Legacy token without `permissions`: unchanged.

At creation every ceiling entry must be held by the issuer and (for a service
account) by the account. Grant-bearing tokens never authenticate the worker
protocol.

### 9. Cross-namespace owners

`namespace_owner_grants(namespace, user_id, granted_by, created_at,
revoked_at)`. An Administrator (or existing Owner) of namespace B grants Owner
of B to a user by **user ID and email** (the pair makes probing another
tenant's users infeasible). When that user's session requests
`/v1/namespaces/B/…`, the guard resolves the grant and acts with
Administrator authority in B, recorded as role `Owner` and attributed to the
real user in B's audit log. There is no wildcard or global owner; each
namespace is an explicit grant revocable by B. `GET /v1/auth/memberships`
lists the namespaces a session may act in.

### 10. Users pagination

`GET /users?limit=&cursor=&q=` returns `{users, next_cursor}`, keyset-ordered
by `(created_at DESC, id DESC)`; `q` matches email or display name
server-side. The 500-account picker cap is gone.

### 11. Logout

`POST /v1/auth/logout` still revokes the session first. For an SSO session it
also returns `end_session_url`: the OIDC `end_session_endpoint` (RP-initiated
logout 1.0, with `client_id` and `post_logout_redirect_uri`) when discovered,
or a SAML `LogoutRequest` URL (HTTP-Redirect) when the IdP metadata has an SLO
endpoint. The console navigates there after local logout. Local logout never
depends on the IdP answering.

## Consequences

**Positive.** Tenants bring their own IdP, MFA and offboarding; SSO users need
no Nexus password. Group-driven roles are revoked at the next login. Identity
administration can be delegated without handing out Administrator. Machine
credentials get real least privilege. Platform teams can act in several
namespaces with one audited identity.

**Negative.**

- Group changes apply at the next login, not immediately: an existing session
  keeps its roles until it expires (12 h) unless the user is disabled or an
  administrator revokes sessions. SCIM and OIDC back-channel logout would close
  this and are not implemented.
- The handoff adds one round trip and a dependency on `NEXUS_PUBLIC_URL`
  being correct; SSO refuses to start without it.
- SAML encrypted assertions and signed AuthnRequests (without an SP key
  reference) are not supported in this release; confidentiality relies on TLS.
- In-memory discovery/JWKS caches are per replica; a key rotation is picked up
  on the first unknown `kid` (go-oidc refetches), discovery changes within 1 h.
- More identity tables to back up and more permissions for administrators to
  understand.

**Neutral.** API keys, worker credentials and the password login are
unchanged. Existing scoped tokens behave exactly as before.

## Alternatives considered

- **Implicit or hybrid flow / tokens in the fragment** — rejected by current
  OAuth security BCP (RFC 9700); code + PKCE is required.
- **Storing the ID token for `id_token_hint`** — rejected: it is a replayable
  identity assertion kept only for logout; `client_id` is an accepted
  alternative in RP-Initiated Logout 1.0.
- **Discovering the tenant from the email domain** — rejected for now: an
  unauthenticated "which tenant uses this domain" endpoint enumerates
  customers. The login page takes a tenant ID or a tenant-specific link.
- **Auto-linking by email whenever the domain matches** — rejected: it hands an
  existing account (possibly an administrator) to anyone who can make the IdP
  assert that address, which unverified-email IdPs allow.
- **IdP-initiated SAML** — rejected: no request to bind `InResponseTo` to,
  which removes the login-CSRF and replay defences.
