# TLS for inter-service gRPC

**Status:** available, opt-in, and wired into every gRPC server and client (§6).
Certificates hot-reload from disk without a restart, and there are tools to issue
and rotate them for compose, AKS and the Portainer host (§5).

**Audience:** whoever deploys this.

---

## 1. What the situation is

Every control-plane hop is plaintext HTTP/2. Six services talk over gRPC, and
eight dial sites each spelled out `insecure.NewCredentials()` for themselves.
`docs/adr/0011-microservice-decomposition.md` §"costs" names mTLS between services
as a known, unpaid cost of the decomposition; this document is the first half of
paying it.

Two things follow from the duplication, and both are worse than the plaintext:

- Turning TLS on meant editing eight files across five services, so it was a
  thing a developer could ship rather than a thing an operator could do.
- There was no way to *ask* whether the posture was right. A deployment that
  intended mTLS and got plaintext looked identical to one that intended
  plaintext: healthy services, successful RPCs, and worker lease tokens and
  session tokens crossing the network in the clear. Nothing fails and nothing
  logs.

`internal/transport` moves the decision to one place, keeps the unconfigured
behaviour byte-for-byte identical to today, and lets a deployment declare that
plaintext is fatal.

## 2. What it protects, and what it does not

**It protects:** confidentiality and integrity of inter-service traffic, plus
*peer identity* — under mTLS, both ends prove they hold a certificate the
configured CA signed. That matters here because the worker gRPC surface carries
bearer credentials (API keys, session tokens, lease tokens), and a bearer
credential on a plaintext wire is readable by anything on the path.

**It does not do authorization.** TLS answers "is this peer who it claims to be,
and is the channel private". It does not answer "may this caller do this". That
lives in `services/api-gateway/authn` — static API keys, hashed at rest, three
roles — and is entirely separate. A valid client certificate does not grant any
API permission, and revoking an API key does not invalidate a certificate. Do not
read "mTLS is on" as "the control plane is authenticated"; the two layers answer
different questions and both are needed.

**It does not cover:** the REST ingress (terminate that at your load balancer or
reverse proxy, as usual), Postgres (`sslmode` in the DSN), or NATS (its own TLS
config). This is inter-service gRPC only.

**Issuance and rotation** are covered in §5: hot reload in the process, cert-manager
on AKS, and `deploy/nexus-certs.sh` elsewhere. No CA key is ever in this
repository.

## 3. Environment variables

All read by `internal/transport`. Set none of them and the behaviour is exactly
what it is today: plaintext, no error, no warning.

| Variable | Role |
|---|---|
| `NEXUS_TLS_CERT_FILE` | PEM certificate (chain) this service **serves** with. |
| `NEXUS_TLS_KEY_FILE` | Its private key. Both or neither — half a pair is an error, never a downgrade. |
| `NEXUS_TLS_CA_FILE` | PEM bundle used to verify **the other end**, in both directions: a client verifies the server's chain against it, and a server verifies a presented client certificate against it. |
| `NEXUS_TLS_CLIENT_CERT_FILE` | PEM certificate this service **dials** peers with. Setting it is what makes a connection mutually authenticated. |
| `NEXUS_TLS_CLIENT_KEY_FILE` | Its private key. Both or neither. |
| `NEXUS_TLS_REQUIRE_CLIENT_CERT` | When truthy, the server **requires and verifies** a client certificate. Needs `NEXUS_TLS_CA_FILE`. |
| `NEXUS_TLS_SERVER_NAME` | Overrides the hostname a client verifies the server certificate against. See §4.3. |
| `NEXUS_REQUIRE_TLS` | Makes plaintext **fatal**. `NEXUS_ENV` does *not* arm this guard. See §4.4. |
| `NEXUS_TLS_RELOAD_INTERVAL` | How often the files above are re-read (Go duration, default `10s`; `0` disables reloading). See §5.0. |

The CA is one variable rather than two because in a single-issuer deployment —
which is every deployment this platform is small enough to have — the serving and
verifying bundles are the same file, and two knobs invite setting one and
forgetting the other. If you genuinely need separate issuers per direction, that
is a change to `internal/transport`, not a config trick.

Truthy values follow the same word list `NEXUS_REQUIRE_DSN` accepts: anything
other than empty, `0`, `false`, `no`, `off` is on.

## 4. Rolling it out

Because a server that requires client certificates rejects every client that does
not yet present one, the order matters. Do it in this order and no step has an
outage.

### 4.1 Encrypt the hops (server-authenticated TLS)

Give every service a serving key pair and every service the CA bundle:

```
NEXUS_TLS_CERT_FILE=/etc/nexus/tls/scheduler.pem
NEXUS_TLS_KEY_FILE=/etc/nexus/tls/scheduler-key.pem
NEXUS_TLS_CA_FILE=/etc/nexus/tls/ca.pem
```

Roll the **servers** first. A TLS server rejects a plaintext client, so during the
window between rolling a server and rolling its clients, calls fail — deploy the
whole control plane together, or accept a brief window of failed RPCs which the
clients retry. At this point traffic is encrypted and clients verify servers;
servers do not yet know who is calling.

### 4.2 Require client certificates (mTLS)

Once every service is dialing with TLS, issue client leaves, set them everywhere,
and only **then** turn on enforcement:

```
NEXUS_TLS_CLIENT_CERT_FILE=/etc/nexus/tls/scheduler-client.pem
NEXUS_TLS_CLIENT_KEY_FILE=/etc/nexus/tls/scheduler-client-key.pem
NEXUS_TLS_REQUIRE_CLIENT_CERT=1
```

A service is both a server and a client of its peers, so it usually holds both
pairs. One leaf may serve both roles if it carries both the `serverAuth` and
`clientAuth` extended key usages; two leaves is cleaner and is what the tests
model.

Enforcement without a CA bundle is refused at startup: requiring a certificate
with nothing to verify it against accepts *any* certificate, including a
self-signed one an attacker minted, which is strictly worse than the plaintext it
replaces while reading as mTLS in a config review.

### 4.3 The name mismatch you will hit

`config.PeerTarget` rewrites a bare `host:port` to `dns:///host:port` so each
reconnect re-resolves the name (see `internal/config/grpcdial.go` for why that is
load-bearing). gRPC then verifies the server certificate against `host` — which in
a compose file is a service name like `scheduler`, not a DNS name a real
certificate authority will put in a SAN. The handshake fails with

```
x509: certificate is valid for nexus.internal, not scheduler
```

which reads like a broken certificate and is a naming mismatch. Two fixes:

1. **Preferred:** issue certificates whose SANs include the names services
   actually dial (`scheduler`, `dag-engine`, `workflow-manager`, …). In compose or
   Kubernetes those names are stable and are the real peer identity.
2. **Escape hatch:** set `NEXUS_TLS_SERVER_NAME` to the name the certificate does
   carry. Note this is *per process*, so it only works when every peer a service
   dials presents the same name — fine for a single wildcard or shared-identity
   cert, wrong for per-service identities.

### 4.4 Make plaintext fatal

Set **`NEXUS_REQUIRE_TLS=1`**. With it set and no certificates configured,
`transport.ServerOptions()` and `transport.DialOption()` return an error naming
the variables they checked, and the service exits at startup instead of serving.

**`NEXUS_ENV` does not arm this guard.** An earlier version of this document said
it did, and that was wrong: `transport.RequireTLS()` reads only
`NEXUS_REQUIRE_TLS`, and `TestNexusEnvDoesNotArmTheTLSGuard` pins that. The
reason is in the code. Unlike a missing DSN, missing TLS has a working outcome,
since every deployment ran plaintext until now. If `NEXUS_ENV=production` armed
the guard, wiring TLS support into the services would have broken every
deployment that already set it for the DSN guard. Requiring TLS is a separate
decision. Make it once certificates are actually distributed.

This is the step that turns "we forgot to configure certificates" from an
invisible condition into a startup failure. Without it, the mistake is found by a
packet capture or not at all.

## 5. Certificate issuance and rotation

Before this section was written, issuance and rotation were out of scope. The key
pair was read once at startup, so every renewal needed a restart, and nothing
reported expiry. Both have changed:

- **Rotation needs no restart.** The process re-reads its certificate, key and CA
  bundle from disk every `NEXUS_TLS_RELOAD_INTERVAL` (10s by default).
- **Issuance has a tool for every environment.** That is cert-manager on AKS, and
  `deploy/nexus-certs.sh` for the laptop and the Portainer host.
- **Expiry is measured**, as served, in metrics you can alert on.

### 5.0 How hot reload behaves

These points are what an operator can rely on. All of them are enforced by tests
in `internal/transport`.

- **New handshakes use the new material; established connections keep theirs.** A
  TLS session's certificate is fixed at its handshake. Control-plane connections
  re-handshake at least every `MaxPeerConnectionAge` (30s, `config.PeerServerOptions`),
  so a rotation reaches every internal hop within about a minute. The gateway's
  **worker** listener does not set a connection age, so an agent keeps its current
  connection, on its old certificate, until that connection drops. That is
  harmless for renewal. It does mean that removing a CA from the bundle does not
  disconnect agents already connected with a leaf from that CA. See §5.6.
- **A new pair is validated before it replaces the old one.** It is rejected, and
  the previous material keeps serving, if: the key does not match the certificate;
  the leaf is expired; the leaf is not valid yet (more than 5 minutes of clock
  skew); a file is missing or unreadable; or the CA bundle contains no currently
  valid certificate. A rejection is logged once, at ERROR, with the serial still
  being served. It increments `nexus_tls_reload_total{result="failure"}` and holds
  `nexus_tls_reload_stale` at 1 until the files are fixed. A not-yet-valid
  certificate is re-tried on every poll and applied once it becomes valid.
- **Writers do not need to be atomic across files.** A poll that sees a change
  waits a debounce (a quarter of the interval, at most 2s) and proceeds only if the
  content has stopped changing. `cp cert; cp key` therefore does not produce a
  spurious failure. Each individual file should still be replaced by rename, which
  is what every tool here does.
- **Kubernetes secret volumes work as is.** Kubelet swaps a `..data` symlink
  atomically. Polling reads through the configured path and follows the swap.
  (That is why this uses polling rather than fsnotify: a file watch misses that
  swap.) **Never mount the secret with `subPath`.** subPath mounts are not updated,
  which silently brings back "serve the old certificate until it expires".
- **The CA bundle may hold several CAs at once.** That is how a CA rotates without
  an outage (§5.3). Expired CAs in the bundle are ignored as long as at least one
  is still valid.
- **Startup is still strict.** An unloadable or expired certificate at startup is
  fatal, as before. There is no previous material to fall back to, and a service
  whose every handshake would fail should refuse to start.
- **TLS session tickets are off** on the server. A resumed session skips
  certificate presentation and would carry a pre-rotation identity past a
  post-rotation CA bundle. Clients never resumed sessions anyway (grpc-go sets no
  session cache), so this costs nothing.
- `NEXUS_TLS_RELOAD_INTERVAL=0` turns reloading off and restores load-once.

### 5.1 Local compose

```sh
make dev-certs    # local CA + leaves for every service and agent, into .dev-certs/ (gitignored)
make up-tls       # the normal stack plus deploy/docker-compose.tls.yml: mTLS on every gRPC hop
make certs-check  # expiry of everything in .dev-certs
```

Leaves last 30 days (`NEXUS_CERT_DAYS`) and the CA 365 days (`NEXUS_CA_DAYS`).
Keys are 0600 from creation. Each service leaf has its compose name, `localhost`
and `127.0.0.1` as SANs, and carries both server and client auth. Agents get
client-only leaves. The override's one-shot `tls-init` container copies the leaves
and bundle into a volume owned by the service uid (10001). It never copies the CA
key. This step is needed because your 0600 keys are unreadable to uid 10001
through a bind mount.

To rotate while the stack is running:

```sh
deploy/nexus-certs.sh issue scheduler --dns scheduler,localhost --ip 127.0.0.1
docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.tls.yml run --rm tls-init
docker compose -f deploy/docker-compose.yml logs scheduler | grep 'TLS material reloaded'
```

Dev only: every service can read every leaf in the shared volume.

### 5.2 AKS: cert-manager with a private CA

Manifests are in `deploy/k8s/cert-*.yaml`. Install steps and a Deployment mount
example are in [deploy/k8s/cert-README.md](../deploy/k8s/cert-README.md).
cert-manager is pinned to v1.18.2 and trust-manager to v0.18.0.

**The choice: a private CA held by cert-manager's CA issuer.** An Azure Key Vault
issuer was considered and not chosen.

| | Private CA (cert-manager `ca:` issuer) — **default** | Azure Key Vault |
|---|---|---|
| Where the CA key lives | A Kubernetes secret in the `cert-manager` namespace (or an intermediate from an offline root) | HSM-backed in Key Vault; never extractable |
| Issuance path | In-cluster, no network dependency; renewals keep working if Azure's control plane is degraded | Every issuance is an Azure API call through a third-party issuer or CSI driver; Key Vault's own CA integration is for public CAs (DigiCert/GlobalSign), so a *private* CA needs extra machinery |
| Cost / moving parts | cert-manager + trust-manager | cert-manager + a Key Vault issuer or the Secrets Store CSI driver + workload identity + Key Vault access policy |
| Blast radius of a cluster compromise | Whoever can read the CA secret can mint identities until the CA is rotated | The key cannot be read, only used through its access policy, which is still a compromise but a revocable one |
| Fit for 30-day leaves renewed automatically | Native | Workable, with more latency and more failure modes per renewal |

The private CA wins for this platform. It has the fewest moving parts on the
renewal path. A certificate system that fails during an Azure incident is a
second outage stacked on the first. Its weakness is the CA key sitting in a
secret. The mitigations are: keep it in the `cert-manager` namespace, not
`kgf-nexus`; and **in production, load an intermediate signed by an offline root
instead of the self-signed bootstrap** (instructions in the header of
`cert-issuer.yaml`). Choose Key Vault instead if your compliance regime requires
HSM-resident CA keys. The services do not care which issuer is used, because they
read files either way.

Design points and the reasons for them:

- **One leaf per service, both EKUs, Service names as SANs.** It lasts 30 days,
  renews at 10 days left, and gets a new key on every renewal.
- **The CA renews with its key kept** (`rotationPolicy: Never`). Leaves issued
  under the previous CA certificate still chain to the renewed one. Rotating the
  *key* is a planned procedure (§5.3, OPERATIONS §9.4), not a side effect of
  renewal.
- **Peers verify against a trust-manager `Bundle`, not the leaf secret's
  `ca.crt`.** A leaf's `ca.crt` changes only when that leaf renews, so during a CA
  key rotation, services would trust different CAs depending on when each last
  renewed. One bundle, updated for everyone at once, is what makes an overlap
  window possible.

### 5.3 CA rotation (overlap)

This applies in every environment; only the tools differ. There are three states,
and the move from one to the next waits until every process has reloaded:

1. Bundle = **old**. All leaves are from old.
2. Bundle = **old + new**. Re-issue every leaf from new. Both kinds verify.
3. Bundle = **new**. Every leaf is from new.

`TestCABundleOverlapRotation` checks each state with real handshakes. The step
from 2 to 3 is the dangerous one. `nexus-certs.sh drop-retired-cas` refuses while
any leaf it knows about is still from the old CA. On AKS, you check that yourself
(OPERATIONS §9.4).

### 5.4 Metrics and alerting

`transport.RegisterMetrics(reg)` exposes, per process:

| Metric | Meaning |
|---|---|
| `nexus_tls_cert_not_after_timestamp_seconds{role}` | Expiry of what is **served**, for `role` = `server`, `client`, `ca` (latest valid CA in the bundle). **Alert on this.** |
| `nexus_tls_cert_expiry_seconds{role}` | Seconds remaining, refreshed each poll. Use it for dashboards only: it freezes if the poller stalls. |
| `nexus_tls_reload_total{role,result}` | On-disk changes applied (`success`) or rejected (`failure`, once per distinct content) |
| `nexus_tls_reload_stale{role}` | 1 while rejected material is on disk and the previous material is still served |

**Readiness does not change.** A certificate near expiry still works. A `/readyz`
that failed on it would take every replica out of rotation at the same moment and
turn a warning into an outage. Alerting handles expiry. The rules are in
`deploy/k8s/cert-alerts.yaml` and OPERATIONS §9.3.

**Gap:** only workflow-manager serves `/metrics` today, so it is the only service
exporting these. Every service logs a WARN, at most hourly, when the leaf it is
serving is in the last fifth of its lifetime without a renewal. On AKS,
cert-manager's own `certmanager_certificate_expiration_timestamp_seconds` covers
every secret. What it cannot see is a process still serving an old certificate
after a renewal. For the services without `/metrics`, that is only visible in the
logs until they expose `/metrics` (each one needs a one-line
`transport.RegisterMetrics(reg)` once it has a registry).

### 5.5 Agents (execution plane)

The agent dials through `transport.DialOption()`, so it hot-reloads its client
certificate and CA bundle exactly as the services do. No restart is needed.

**Enrolment of an agent outside the cluster.** The private key is created on the
agent host and never leaves it:

```sh
# agent host
deploy/nexus-certs.sh csr edge-agent-7 /etc/nexus/tls          # writes tls.key.new (0600) + tls.csr
# send ONLY tls.csr to the CA holder
# CA host (for AKS: the machine holding the offline root/intermediate, not the cluster)
NEXUS_CERT_DIR=/secure/nexus-ca deploy/nexus-certs.sh sign-csr edge-agent-7 tls.csr
# send back enrolled/edge-agent-7/tls.crt and ca-bundle.pem
# agent host
deploy/nexus-certs.sh install-signed /etc/nexus/tls tls.crt ca-bundle.pem
```

`sign-csr` signs client-only certificates, and only when the CSR's subject is
exactly `CN=<name>`. The CA holder chooses the identity, not the requester.
`install-signed` refuses a certificate that does not match the pending key. Agent
environment:

```
NEXUS_TLS_CLIENT_CERT_FILE=/etc/nexus/tls/tls.crt
NEXUS_TLS_CLIENT_KEY_FILE=/etc/nexus/tls/tls.key
NEXUS_TLS_CA_FILE=/etc/nexus/tls/ca.crt
NEXUS_TLS_SERVER_NAME=<a SAN on the gateway's leaf>   # only if the dialled name is not one
NEXUS_REQUIRE_TLS=1
```

**Rotation** repeats the same three commands before expiry, from a cron job or a
timer on the agent host (e.g. monthly for 30-day leaves). The running agent picks
the new certificate up by itself. `TestAgentEnrollmentAndLiveRotation` runs this
exact procedure twice against a live gateway.

In-cluster agents mount the `nexus-agent-tls` secret instead
(`cert-certificates.yaml`).

### 5.6 What this still does not do

- **No revocation.** There is no CRL and no OCSP. A leaked leaf stays valid until
  it expires, which is why leaves are short-lived. To cut off a compromised
  identity immediately, rotate the CA (§5.3) and re-issue everyone else. Even
  then, an agent connection **already established** on the worker listener stays
  up until it drops, because that listener has no `MaxConnectionAge`. Adding one
  is a gateway change and outside this work.
- **No authorization by certificate identity.** A valid client certificate still
  grants nothing (§2). Nothing checks that the CN of a client leaf matches the
  service or agent that presents it.
- The Portainer host's rendered compose file (`deploy/render_compose.py`) does not
  include the TLS override yet. OPERATIONS §9.5 describes the host-side setup that
  replaces it.

A deliberately unsupported option: do not put a CA private key in this
repository, and do not reuse the test CA from `internal/transport/testca_test.go`
for anything. Test CAs are generated per test run and thrown away, and
`.dev-certs/` is gitignored.

## 6. Wiring

Every gRPC server and client is wired. Check with
`grep -rn 'transport\.\(ServerOptions\|DialOption\)' --include='*.go' services agent`.
The options compose with the existing peer options rather than replacing them. The
connection-age rotation in `config.PeerServerOptions()` matters regardless of the
transport, and it is also what carries a rotated certificate onto existing
internal connections (§5.0). So append, never substitute:

```go
sopts, err := transport.ServerOptions()
if err != nil {
    return fmt.Errorf("resolve TLS server options: %w", err)
}
grpcSrv := grpc.NewServer(append(config.PeerServerOptions(), sopts...)...)
```

```go
creds, err := transport.DialOption()
if err != nil {
    return fmt.Errorf("resolve TLS dial options: %w", err)
}
conn, err := grpc.NewClient(config.PeerTarget(addr), append(config.PeerDialOptions(), creds)...)
```

Client half: `services/api-gateway/upstream/{dagengine,resourcemanager,scheduler,workflowmanager}.go`,
`services/workflow-manager/grpcapi/{dagengine_grpc,scheduler_grpc}.go`,
`services/dag-engine/runstate_client.go`, `agent/internal/worker/grpcclient.go`.
Server half: `services/{scheduler,dag-engine}/main.go`,
`services/{resource-manager,workflow-manager}/cmd/*/main.go`, and the worker listener in
`services/api-gateway/cmd/api-gateway/main.go`. The worker listener deliberately
does **not** use `PeerServerOptions`, because long polls need their own keepalive
policy. That is why §5.0 and §5.6 treat agent connections separately.

event-service speaks HTTP, not gRPC, and is not covered.

Calls to `ServerOptions`/`DialOption` with the same files share one reloader per
process, and therefore one poller and one set of metrics.
