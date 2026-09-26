# Customer tenancy

## Registration

`POST /v1/auth/register` accepts customer name, owner email and password. In one database transaction it creates a UUID tenant ID, a namespace equal to that UUID, a customer profile and an owner user with the protected Administrator role. The owner is recorded in `tenants.owner_user_id`. Passwords use the existing password hashing implementation. Invalid or duplicate registration does not leave partial records. Registration is rate limited by the gateway. One self-service registration is permitted per normalized email. The same transaction mints a single-use verification link for the owner, which is emailed after commit (see Email verification and password recovery below). The response reports `email_verification.delivery` as `queued`, `mail_disabled` or `not_queued`, so the console never claims an email it did not send.

The public `/register` page shows the generated tenant ID and links to email-only sign-in. The owner signs in with email and password; the gateway resolves the unique enabled account and its namespace. Ambiguous emails fail closed. The API still accepts an explicit namespace for legacy clients. Additional users and worker tokens are created by this Administrator inside that tenant. Worker processes must use the new tenant namespace and its own worker credential; onboarding does not reassign existing workers or provision machines.

## Isolation

Each customer currently owns exactly one namespace. Existing tenant-aware namespace checks remain authoritative for REST, gRPC, roles, users, tokens, workflow definitions, runs, logs, artifacts and infrastructure configuration. Resource identifiers do not bypass namespace authorization. The client clears cached queries when credentials change and scopes the workspace to the new session. Customer ID is not an authorization credential. Changing a URL or browser namespace does not change membership.

`GET /v1/account/tenant` resolves the customer from the authenticated credential, never from a supplied tenant ID. `/customer` shows this profile. Namespace selection in settings is read-only once authenticated. Legacy namespaces are backfilled with distinct tenant IDs, without renaming workload or credential namespaces. Legacy sign-in continues to accept those namespace strings.

The isolated database integration test registers two customers, checks owner creation and duplicate rollback, and denies cross-tenant user/run access and customer-directory access. This is application-enforced logical tenancy on shared infrastructure, not dedicated databases or physical clusters. Internal services retain their current trusted service roles; no PostgreSQL RLS or tenant-specific encryption keys are claimed.

## Platform operations

An optional, separately enrolled platform operator can use `/v1/platform/tenants` for onboarding and a bounded customer directory. Customer registration never grants this privilege. `platform_operators` is writable only by the installation database operator; the gateway has SELECT only. Enrollment requires an explicitly authorized existing user ID. Operators can list customer metadata but do not gain cross-tenant workflow access. No platform operator is automatically enrolled.

## Deployment

Apply migration 0013 before the new gateway. Backfill preserves existing data. The UI can then expose customer registration. No billing subscription, cloud scaling or resource provisioning is triggered. Before exposing self-service registration publicly, add signup abuse controls and configure tenant quotas (namespace quotas are enforced since migration 0027, ADR-0021, but every namespace starts unlimited; see OPERATIONS §21); email verification and password recovery exist since migration 0017 but require a configured SMTP server (docs/OPERATIONS.md §10). Multi-namespace membership, customer suspension and ownership-transfer workflows require separate lifecycle work.

## Explicit row ownership (migration 0014)

All 24 customer-owned tables now have a non-null, indexed `tenant_id` foreign key. Namespace-based rows derive it from `tenants`; dependent rows derive it from the run, attempt, schedule or user. Database triggers support older service insert statements while rejecting supplied mismatches and ownership changes. Composite foreign keys prevent cross-tenant spec/run, worker/attempt, worker/lease, worker/reservation, schedule/backfill, token/user and owner/user relationships.

The shared registries `tenants`, `platform_operators`, and migration history are system metadata rather than customer-owned rows. `tenants.id` is the canonical tenant identity. New namespaces must first be registered; directly minting credentials for an arbitrary unregistered namespace now fails.

Both scheduler placement directions and blocked-work checks filter by tenant ID. Concurrency-key counts are tenant-scoped. This adds integrity and scheduling isolation; namespace-based API read checks remain in place. It does not claim PostgreSQL RLS for trusted internal service accounts.

The migration preserves historical cross-tenant worker-link evidence in `audit_log` before clearing only terminal invalid worker associations. An active mismatch or unresolvable timer aborts migration for operator review. Rolling back the schema does not recreate repaired associations; audit evidence is retained. Deploy updated schedulers with the migration.

## Email verification and password recovery (migration 0017)

**Policy for unverified accounts: they cannot sign in.** `users.email_verified_at` records when an account proved it controls its address. When verification is required (the default, `NEXUS_EMAIL_VERIFICATION_REQUIRED=true`), login checks the password first and only then refuses an unverified account with `403 email_not_verified`. The refusal is therefore visible only to someone who already knows the password, and is not an account oracle. A restricted session was considered and rejected: it would add a second authorization dimension to every guarded route, and a registration made with someone else's address would still get a working session in a tenant it claims to own.

**Existing accounts keep working.** Migration 0017 adds the column with a default of `now()` (a catalog-only change that stores the value for every existing row) and then drops the default. Every account that existed when 0017 ran is verified; every account created afterwards, including one created by an older gateway replica during a rolling deploy, starts unverified.

**What makes an account unverified.** Self-registration, platform-operator onboarding and admin-created users all start unverified. An admin-created user sees the resend prompt at first sign-in. Changing an account's address clears verification, enforced by a trigger on `users`, so every code path that edits an email is covered. A case-only change does not clear it.

**Links.** `email_tokens` stores the sha256 of a 32-byte random token (never the token), its purpose (`verify_email` or `reset_password`), the address it was sent to, an expiry, and consumed or revoked times. A link works once, for its own purpose, before its expiry by the database clock, and only while the account is enabled and still has the address the link was sent to. Requesting a new link revokes older ones of the same purpose. Issuance is capped per account and purpose per hour in the database, so the cap holds across gateway replicas. Tokens travel in the URL fragment (`/verify-email#token=…`, `/reset-password#token=…`). Browsers do not send fragments to servers, so tokens stay out of nginx access logs and Referer headers. The console erases the fragment on load and POSTs the token in a request body. Verification requires a click so that mail scanners that open links do not use them up.

**Routes (unguarded, like login; rate limited per IP inside the handler):**

| Route | Answer |
|---|---|
| `POST /v1/auth/email/verification` `{email, namespace?}` | 202 with the same body whether or not the address has an account |
| `POST /v1/auth/email/verify` `{token}` | 200 `{verified, email}` or 400 `invalid_token` |
| `POST /v1/auth/password/forgot` `{email, namespace?}` | 202 with the same body whether or not the address has an account |
| `POST /v1/auth/password/reset` `{token, password}` | 200 `{password_reset, sessions_revoked}`, 400 `invalid_token` or 422 `invalid_password` |

**No account oracle.** The two link-request routes answer before any lookup. Lookup, token issuance and SMTP delivery run on a bounded background queue, so response bodies, headers and timing do not depend on whether the account exists. The request is refused the same way for every address when the IP limit is hit (`429`), when mail is not configured (`503 email_unavailable`) or when the queue is full (`503`). Past the per-address limit (3 per hour per purpose, per replica), requests still get the same 202 and are dropped silently.

**Reset.** In one transaction, a reset stores the new argon2id verifier, revokes every session of the account, revokes the account's other outstanding reset links, marks the address verified (following a link sent to it is proof of ownership) and writes a `user.password_reset` audit row. Scoped machine tokens created by the user and API keys are not revoked, because they belong to the namespace, not the person. Disabled accounts and accounts without a password (reserved for a future identity provider) get no links.

**Audit actions:** `user.email_verification_sent`, `user.email_verified`, `user.password_reset_requested` (outcome `error` when delivery failed), `user.password_reset`, and `session.login` denied with reason `email not verified`. Audit rows record token ids, never tokens.

**Known limits.** A registration made with someone else's address still reserves that address's single self-service registration until an operator removes it. Nothing expires unverified registrations. The per-IP and per-address limits are in memory per replica; only the hourly issuance cap is shared. Per-IP limiting trusts `X-Real-IP` only from a private or loopback peer (the console's nginx). A client that can reach the gateway directly from a private network can forge that header, which leaves the per-address and database caps as the bound.

## Single sign-on and cross-namespace owners (migration 0032)

Each tenant configures its own OIDC or SAML identity providers (ADR-0024, OPERATIONS §24). A provider belongs to exactly one tenant: every account it provisions or resolves lives in that tenant's namespace, whatever the IdP asserts, and a trigger rejects any identity whose user, provider and namespace disagree. One IdP application (OIDC issuer and client ID, or SAML entity ID) can be registered by only one tenant, and client-secret references resolve only inside the tenant's own secret directory. The login page takes the tenant from a tenant-specific link (`/login?tenant=<namespace>`); there is deliberately no discovery by email domain.

The first multi-namespace capability is the explicit owner grant: an administrator of namespace B may make a named user of another namespace an Owner of B. That user acts in B with administrator authority, attributed to them in B's audit trail, until B revokes the grant. There is no wildcard or platform-wide owner.
