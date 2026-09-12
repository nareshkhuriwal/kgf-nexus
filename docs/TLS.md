# TLS for inter-service gRPC

**Status:** available and opt-in. Nothing is wired into the service `main.go`s yet
— `internal/transport` provides the credentials; see §6 for the call each service
needs.

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

**It does not do certificate issuance or rotation.** See §5 — that is honestly out
of scope, and pretending otherwise is how a platform ends up with a CA whose key
is in a git repository.

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
| `NEXUS_REQUIRE_TLS` / `NEXUS_ENV` | Make plaintext **fatal**. See §4.4. |

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

The guard, mirroring `internal/config.DSNRequired()`:

Set **`NEXUS_ENV=production`** (or `prod`, or `staging`) or
**`NEXUS_REQUIRE_TLS=1`**. With either set and no certificates configured,
`transport.ServerOptions()` and `transport.DialOption()` return an error naming
the variables they checked, and the service should exit at startup rather than
serve. `NEXUS_REQUIRE_TLS=0` overrides `NEXUS_ENV` for staging, canary, or a CI
job that deliberately runs without certificates.

`NEXUS_ENV` is deliberately the same variable the DSN guard reads, so one setting
arms every production-only check the platform has. `NEXUS_ENV=dev` arms nothing —
the dev path must stay unchanged, which is the whole premise.

This is the step that turns "we forgot to configure certificates" from an
invisible condition into a startup failure. Without it, the mistake is found by a
packet capture or not at all.

## 5. Certificate issuance and rotation: out of scope

`internal/transport` reads files. It does not create, sign, renew, or reload them,
and the tests mint a throwaway CA in a temp directory precisely so that nothing
about a real PKI is implied.

Two consequences an operator must plan around:

- **Rotation needs a restart.** The key pair is loaded once, at startup. Replacing
  the file on disk does nothing to a running process. A future
  `tls.Config.GetCertificate` callback could re-read on each handshake; that is
  not implemented. Until it is, treat a certificate renewal as a rolling restart —
  which the platform tolerates well, since `MaxPeerConnectionAge` already rotates
  every connection every 30 seconds.
- **Nothing checks expiry.** A certificate that lapses fails every handshake at
  once, and the symptom is a control plane where every RPC fails with a TLS error.
  Alert on days-to-expiry from wherever the certificates are issued, not from
  here.

How to actually wire issuance, roughly in order of how much machinery you already
have:

- **cert-manager**, if this runs on Kubernetes. An `Issuer` plus a `Certificate`
  per service mounts a secret with `tls.crt`, `tls.key`, `ca.crt`; point the three
  variables at the mount path and let cert-manager renew. Pair it with a reloader
  so renewal triggers a rolling restart.
- **A service mesh** (Linkerd, Istio) does mTLS in a sidecar and needs none of
  this — the services keep speaking plaintext to localhost while the mesh
  encrypts the hop. If you are going that way, do not also configure these
  variables; you would be paying for two handshakes and debugging both.
- **`step-ca` or Vault PKI** for a plain-VM or compose deployment: a short-lived
  leaf per service, renewed by a sidecar or a cron job, with the CA's root offline
  and an intermediate doing the signing.
- **A hand-rolled `openssl` CA** is adequate for a staging environment and is
  where most deployments start. Keep the key off the machines it signs for, and
  write down the expiry date somewhere a human reads.

A deliberately unsupported option: do not put the CA private key in this
repository, and do not reuse the test CA from `internal/transport/testca_test.go`
for anything. It is generated per test run and thrown away, which is the only
reason it is safe to have in the tree.

## 6. Wiring (not yet done)

`internal/transport` is intentionally not called from any `main.go` yet. The
options compose with the existing peer options rather than replacing them — the
connection-age rotation in `config.PeerServerOptions()` is load-bearing regardless
of the transport, so append, never substitute:

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

Sites that need the client half: `services/api-gateway/upstream/{dagengine,
resourcemanager,scheduler,workflowmanager}.go`,
`services/workflow-manager/grpcapi/{dagengine_grpc,scheduler_grpc}.go`,
`services/dag-engine/runstate_client.go`, and
`agent/internal/worker/grpcclient.go`. Sites that need the server half: the five
`grpc.NewServer` calls in `services/{scheduler,dag-engine}/main.go`,
`services/{resource-manager,workflow-manager,api-gateway}/cmd/*/main.go`.

Note the agent's worker connection is the one hop that leaves the control-plane
network, and is therefore the one where the plaintext matters most: its RPCs carry
the API key and session token on every call.
