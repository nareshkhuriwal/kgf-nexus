# ADR-0034: KGF.Nexus workspaces — one namespace per provisioned workspace

- **Status:** Proposed — pending owner sign-off (implemented: migration 0040)
- **Date:** 2026-09-26
- **Deciders:** owner, platform team
- **Refines:** ADR-0024 (context authentication and tenancy mapping),
  ADR-0033 decision 4 (namespace per resource group) and decision 8 (the UI)
- **Detail:** [KGF-INTEGRATION.md §15](../KGF-INTEGRATION.md#15-kgfnexus-workspaces-and-the-engine-ui),
  OPERATIONS §24.11

## Context

KGF Cloud is adding a provisioned resource type, `KGF.Nexus/workspaces`, on
the model of Azure Data Factory: a customer creates a workspace in a resource
group, KGF provisions it, and the portal opens the Nexus workflow engine UI,
mounted in the KGF portal, for that one workspace. Sign-in, users and
permissions stay KGF's.

ADR-0024 maps each KGF (organization, subscription, resource group) to one
namespace, provisioned on first use. Several workspaces may live in one
resource group, so that mapping cannot tell them apart; and a workspace has a
lifecycle KGF drives (create, delete) that "provisioned by the first request"
cannot express: a request after deletion would silently re-create it.

## Decision

1. **A new optional context claim, `res`**: the workspace's ARM-style id,
   `/subscriptions/{sub}/resourceGroups/{rg}/providers/KGF.Nexus/workspaces/{ws}`.
   It must be well-formed (fixed segments and provider case-insensitive,
   `{ws}` matching `^[a-z][a-z0-9-]{2,47}$`) and lie in the token's own `sub`
   and `rg`; otherwise `403 kgf_resource_invalid`.
2. **Only fronts assert `res`.** `NEXUS_KGF_FRONT_ISSUERS` names the KGF
   provider fronts (KGF: `kgf-nexus`). A front's token must carry `res`
   (`kgf_resource_required`) and be a service context
   (`kgf_front_context_invalid`); any other issuer's token carrying `res` is
   refused (`kgf_resource_not_allowed`). An issuer cannot be both a user
   issuer and a front. Two shapes are accepted:
   - **delegated user** (`obo`, `obo_role`): the person, with the ADR-0024 §4
     role mapping, in the workspace's namespace, which must already be
     provisioned and active (`404 workspace_not_provisioned`). Never
     provisioned lazily. Audit rows record the person and the front.
   - **provisioner** (no `obo`): may call only the provisioning routes.
3. **Namespace v2** = `"kgf-" + hex(sha256("kgf-nexus/v2" 0x00 org 0x00
   lower(res)))[:32]`, recorded in a new table `kgf_workspaces` (migration
   0040; `UNIQUE (organization, resource_id)`, state active/deleted). A new
   table rather than a column on `kgf_scopes`, whose uniqueness is per
   resource group (the v1 key); v1 rows and readers are untouched.
4. **Provisioning is explicit and service-only**: `PUT /v1/kgf/workspace`
   (idempotent, 201/200) and `DELETE /v1/kgf/workspace` (idempotent, always
   200 `{"state":"deleted"}`), the workspace taken from the signed `res`,
   never from a body. Each writes an audit row.
5. **Delete retains and revokes.** DELETE marks the workspace deleted and
   revokes the namespace's API keys, scoped tokens and service accounts in one
   transaction. The tenant and all it owns (specs, runs, logs, usage ledger,
   audit chain) are retained under ordinary Nexus retention; nothing is
   hard-deleted, because Nexus has no safe tenant purge. In-flight runs are
   not cancelled, and schedules, triggers and sensors are not disabled: those
   rows belong to other services (ADR-0011).
6. **A deleted workspace's resource id is not re-created** (`409
   workspace_deleted`). The namespace is derived, so re-creating would
   reactivate the old namespace and its history; a fresh namespace would need
   a generation the v2 formula does not carry.
7. **Keys and audiences.** `NEXUS_KGF_CONTEXT_KEYS_URL` takes a comma-separated
   list of key documents (the gateway's and each front's), merged with the
   static keys; `NEXUS_KGF_AUDIENCE` keeps its default and also accepts a list
   (the KGF.Nexus front mints `kgf-nexus-engine`).
8. **The Nexus workflow engine UI** is mounted in the KGF portal as a
   federated extension, to be specified with it; KGF passes its calls through
   at `…/KGF.Nexus/workspaces/{ws}/engine/v1/…` and the engine receives plain
   `/v1/…`. This ADR changes no UI code.

## Consequences

**Positive.** Several isolated workspaces per resource group; KGF's lifecycle
decides when a workspace exists, and a deleted one is closed to people and
machines at once, on every replica. The v1 mapping, lazy provisioning and
gateway-issued human contexts are unchanged.

**Negative.**

- A deleted workspace's name cannot be reused in the same resource group
  until a later mapping version adds a generation.
- Delete stops access but not work already queued: in-flight attempts run to
  their end (their agents' credentials are revoked, so they cannot claim
  more), and schedules keep materialising runs that nothing will execute.
- Keys are not bound to issuers: a compromised front key can sign
  gateway-issuer contexts and the reverse. The issuer rules separate token
  shapes, not signers (follow-up: per-source issuer binding).
- One indexed lookup per delegated workspace request (the active state is
  never cached, so a DELETE takes effect on every replica immediately).
- `res` is a Nexus-side extension of tenantctx's claim set; kgf-cloud's
  `pkg/tenantctx` must carry it byte-compatibly for the conformance test to
  cover it.

**Neutral.** Standalone deployments and `NEXUS_KGF_FRONT_ISSUERS` unset:
nothing changes; every context carrying `res` is refused.

## Alternatives considered

- **A `resource_id` column on `kgf_scopes`.** Needs the per-resource-group
  unique constraint replaced (a non-additive change on a live table) and
  mixes two mappings with different keys in one table. Rejected for a new
  table.
- **Lazy provisioning of workspaces, as v1.** A request after DELETE would
  resurrect the workspace; KGF could not make "deleted" final. Rejected.
- **Re-creating a deleted workspace as a fresh namespace** (a generation in
  the hash). Safe, but it changes the agreed v2 formula that kgf-cloud builds
  against; deferred to a future mapping version if the owner wants name reuse.
- **Letting the gateway assert `res`.** The gateway would need to know
  workspaces; the front is the KGF component that owns them. Rejected: only
  fronts may.
- **Hard-deleting the tenant on DELETE.** No safe tenant purge exists (every
  owned table, the audit chain and the usage ledger would need one).
  Rejected.
