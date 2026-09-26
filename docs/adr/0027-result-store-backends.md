# ADR-0027: One result-store interface with native S3, Azure Blob and GCS backends

- **Status:** Proposed — pending owner sign-off
- **Date:** 2026-09-26
- **Deciders:** platform team
- **Refines:** ADR-0008 (result store; its deferred "pluggable interface"),
  ADR-0020 (task logs), F115 (cloud platforms)

## Context

ADR-0008 made the result store "S3-compatible" and deferred a pluggable
interface because S3 "covers the field". F115 showed where it does not:

- **Azure Blob has no S3 endpoint** (MinIO removed its gateway in 2022), so
  AKS ran an in-cluster MinIO on a PVC while the provisioned Blob container,
  private endpoint and workload identity sat unused.
- **Every client built static keys** (`credentials.NewStaticV4`), so EKS
  needed an IAM user's access key and GKE an HMAC key, while the keyless IRSA
  / Pod Identity role and Workload Identity binding sat unused.
- Three independent minio-go clients (agent, gateway log reader, GC) each
  re-implemented not-found classification, which the GC's safety depends on.

## Decision

**`internal/objstore`** is the one interface the agent
(`agent/internal/resultstore`), the gateway log reader
(`services/api-gateway/logs`) and the sweep (`internal/artifactgc`) use:

```go
Put(ctx, key, body, size, PutOptions{ContentType, IfAbsent}) error
Stat(ctx, key) (ObjectInfo, error)            // ErrNotFound only on a positive answer
Get(ctx, key, offset, length) (io.ReadCloser, error)   // ranged; ErrNotFound before any read
List(ctx, prefix, fn func(ObjectInfo) error) error     // lazy pages; ErrStop ends it
Delete(ctx, key) error                        // idempotent
BucketExists(ctx) (bool, error); CreateBucket(ctx) error
```

Three backends, selected by `NEXUS_RESULT_STORE_BACKEND` (and advertised by
the gateway to agents in the additive `ResultStoreConfig.backend`; an agent's
own setting wins):

| Backend | Library (pinned) | Credentials |
|---|---|---|
| `s3` | minio-go v7.3.0 (existing) | `NEXUS_S3_CREDENTIALS`: `static` keys; `web-identity` (IRSA); `pod-identity` (EKS Pod Identity); `iam` (adds ECS and EC2 IMDSv2); `env`; `anonymous`. `auto` = static if set, else any AWS workload identity visible in the environment, else a clear error. GCS HMAC interop is `s3` + static keys. |
| `azblob` | `github.com/Azure/azure-sdk-for-go/sdk/storage/azblob` v1.8.1, `azidentity` v1.14.1 | `NEXUS_AZURE_STORAGE_AUTH`: `workload-identity` (AKS webhook), `managed-identity` (optionally user-assigned by client id), `shared-key`; `auto` picks in that order of evidence. |
| `gcs` | `cloud.google.com/go/storage` v1.68.0 | Application Default Credentials (GKE Workload Identity, `GOOGLE_APPLICATION_CREDENTIALS`); `none` for emulators only. |

### The semantics are the contract

`objstore/objstoretest.Run` is one conformance suite every backend (and the
in-memory fake unit tests use) must pass, bare and through the tracing/metrics
decorator:

- **Atomic visibility.** Large bodies go up in parts of
  `NEXUS_RESULT_STORE_PART_SIZE_BYTES` (default 16 MiB): S3 multipart, Azure
  staged blocks committed by a block list, GCS resumable chunks. Nothing is
  visible under the key until the commit; the suite injects a body failure
  mid-upload and asserts the key stays absent.
- **Create-if-absent.** Atomic on Azure (`If-None-Match: *`) and GCS
  (`ifGenerationMatch=0`); stat-then-put on S3, where conditional writes are
  not universal (GCS interop, older MinIO). Content addressing makes the S3
  race benign.
- **Not found is not "could not tell".** Only BlobNotFound / NoSuchKey /
  ErrObjectNotExist become `ErrNotFound`; a missing bucket, a 403 or a timeout
  are errors. The GC's dangling-reference check and its refusal to sweep an
  unverifiable bucket depend on this.
- **Range reads** serve a log page as one request into the compacted log.
- **Listing** is lexical, lazy and stoppable, so a sweep's cost is bounded by
  its own limit, not the bucket; directory markers are skipped.
- **Idempotent deletes.**
- **`BucketExists` answers false only on a positive answer**, and on Azure and
  GCS works with object-scoped roles (it falls back to, or uses, a one-object
  listing: `roles/storage.objectAdmin` has no `buckets.get`).

The GC's safety argument (list, then query reachability; grace period on
last-modified time; stop on any error) needs nothing beyond this contract, so
it holds unchanged on every backend. On GCS an object-level 404 cannot
distinguish a missing bucket, which is why the GC verifies the bucket at
construction on every backend.

**F141, fixed with this change.** A deduplicated put reused an existing object
without refreshing it; an old orphan with the same bytes could then be deleted
by the GC between the put and the report that references it. The agent now
re-uploads an identical object older than an hour
(`resultstore.DedupeRefreshAge`), keeping 23 hours of margin under the default
grace period.

### Retention and lifecycle

Nexus never relies on backend lifecycle rules for correctness: reachability is
decided by the GC. Buckets should still carry, and the Terraform modules
provision: abort incomplete multipart uploads after 7 days (Azure discards
uncommitted blocks after 7 days by itself), expire noncurrent versions after
30 days, soft delete, and **no** age-based expiry on current objects (it
would delete outputs a live run still references). OPERATIONS §26.4.

## Consequences

**Positive.** AKS, EKS and GKE run keyless against their native object store;
the Terraform environments default to that. One place classifies errors. Every
result-store call is a span and a metric (`nexus_objstore_*`, labelled by
backend and operation only). The conformance suite runs locally against Silo,
Azurite and fake-gcs-server (`make objstore-conformance`).

**Negative.** Two SDK dependency trees (Azure, Google) with their transitive
modules. Emulators are not the clouds: throttling, eventual consistency of
listings on some S3-compatibles, Azure's 50 000-block limit (≈ 800 GiB at the
default block size) and IAM propagation delays are covered by design review,
not by the suite, until each cloud is applied (F115). A result store now binds
a client to one bucket; a reference into another bucket is refused with an
explicit error rather than read with the wrong credentials.

**Upgrade order.** Agents first: an old agent ignores `backend` and speaks S3
to whatever the gateway advertises. Switching a deployment's backend does not
migrate objects.

## Alternatives considered

- **S3 gateway in front of Azure (s3proxy).** Another component to run and
  secure, uncertain multipart and conditional-request fidelity. Rejected.
- **GCS only through interop.** Keeps HMAC keys, which are long-lived secrets;
  Workload Identity needs the native client. Kept as a fallback, not the
  default.
- **gocloud.dev/blob.** A portable blob API, but it hides the error codes this
  platform's safety depends on and adds its own abstraction over each SDK's
  credential model. Rejected in favour of thin, directly tested adapters.
