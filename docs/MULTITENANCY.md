# Customer tenancy

## Registration

`POST /v1/auth/register` accepts customer name, owner email and password. In one database transaction it creates a UUID tenant ID, a namespace equal to that UUID, a customer profile and an owner user with the protected Administrator role. The owner is recorded in `tenants.owner_user_id`. Passwords use the existing password hashing implementation. Invalid or duplicate registration does not leave partial records. Registration is rate limited by the gateway. One self-service registration is permitted per normalized email. Email ownership verification and password recovery delivery are not implemented yet.

The public `/register` page shows the generated tenant ID and links to email-only sign-in. The owner signs in with email and password; the gateway resolves the unique enabled account and its namespace. Ambiguous emails fail closed. The API still accepts an explicit namespace for legacy clients. Additional users and worker tokens are created by this Administrator inside that tenant. Worker processes must use the new tenant namespace and its own worker credential; onboarding does not reassign existing workers or provision machines.

## Isolation

Each customer currently owns exactly one namespace. Existing tenant-aware namespace checks remain authoritative for REST, gRPC, roles, users, tokens, workflow definitions, runs, logs, artifacts and infrastructure configuration. Resource identifiers do not bypass namespace authorization. The client clears cached queries when credentials change and scopes the workspace to the new session. Customer ID is not an authorization credential. Changing a URL or browser namespace does not change membership.

`GET /v1/account/tenant` resolves the customer from the authenticated credential, never from a supplied tenant ID. `/customer` shows this profile. Namespace selection in settings is read-only once authenticated. Legacy namespaces are backfilled with distinct tenant IDs, without renaming workload or credential namespaces. Legacy sign-in continues to accept those namespace strings.

The isolated database integration test registers two customers, checks owner creation and duplicate rollback, and denies cross-tenant user/run access and customer-directory access. This is application-enforced logical tenancy on shared infrastructure, not dedicated databases or physical clusters. Internal services retain their current trusted service roles; no PostgreSQL RLS or tenant-specific encryption keys are claimed.

## Platform operations

An optional, separately enrolled platform operator can use `/v1/platform/tenants` for onboarding and a bounded customer directory. Customer registration never grants this privilege. `platform_operators` is writable only by the installation database operator; the gateway has SELECT only. Enrollment requires an explicitly authorized existing user ID. Operators can list customer metadata but do not gain cross-tenant workflow access. No platform operator is automatically enrolled.

## Deployment

Apply migration 0013 before the new gateway. Backfill preserves existing data. The UI can then expose customer registration. No billing subscription, cloud scaling or resource provisioning is triggered. Before exposing self-service registration publicly, add email verification, recovery, signup abuse controls and tenant quota enforcement. Multi-namespace membership, customer suspension and ownership-transfer workflows require separate lifecycle work.

## Explicit row ownership (migration 0014)

All 24 customer-owned tables now have a non-null, indexed `tenant_id` foreign key. Namespace-based rows derive it from `tenants`; dependent rows derive it from the run, attempt, schedule or user. Database triggers support older service insert statements while rejecting supplied mismatches and ownership changes. Composite foreign keys prevent cross-tenant spec/run, worker/attempt, worker/lease, worker/reservation, schedule/backfill, token/user and owner/user relationships.

The shared registries `tenants`, `platform_operators`, and migration history are system metadata rather than customer-owned rows. `tenants.id` is the canonical tenant identity. New namespaces must first be registered; directly minting credentials for an arbitrary unregistered namespace now fails.

Both scheduler placement directions and blocked-work checks filter by tenant ID. Concurrency-key counts are tenant-scoped. This adds integrity and scheduling isolation; namespace-based API read checks remain in place. It does not claim PostgreSQL RLS for trusted internal service accounts.

The migration preserves historical cross-tenant worker-link evidence in `audit_log` before clearing only terminal invalid worker associations. An active mismatch or unresolvable timer aborts migration for operator review. Rolling back the schema does not recreate repaired associations; audit evidence is retained. Deploy updated schedulers with the migration.
