# Certificates on AKS (cert-manager)

These files are the `cert-*` part of `deploy/k8s/`. Other files in this directory
belong to other workstreams. Why things are set up this way is in
[docs/TLS.md §5.2](../../docs/TLS.md). How to operate them is in
[docs/OPERATIONS.md §9](../../docs/OPERATIONS.md).

| File | What it is |
|---|---|
| `cert-issuer.yaml` | Bootstrap self-signed Issuer, the `nexus-internal-ca` CA Certificate, the `nexus-internal-ca` ClusterIssuer, and the `nexus-ca-bundle` trust-manager Bundle |
| `cert-certificates.yaml` | One `Certificate` per service, plus one client-only certificate for in-cluster agents |
| `cert-alerts.yaml` | PrometheusRule: expiry, stuck renewals, rejected reloads |

**Status:** written and checked for YAML syntax only. None of it has been
applied to a cluster. The AKS release template still stops at "chart does not
exist", so no pipeline applies these files.

## 1. Install the controllers (pinned)

```sh
# cert-manager. Pinned so an upgrade is a decision someone makes, not a side
# effect of re-running the install. Check that this release is still supported
# before using it (https://cert-manager.io/docs/releases/).
helm repo add jetstack https://charts.jetstack.io --force-update
helm upgrade --install cert-manager jetstack/cert-manager \
  --namespace cert-manager --create-namespace \
  --version v1.18.2 --set crds.enabled=true

# trust-manager distributes the CA bundle (see cert-issuer.yaml for why the
# per-leaf ca.crt is not enough).
helm upgrade --install trust-manager jetstack/trust-manager \
  --namespace cert-manager --version v0.18.0 --wait
```

## 2. Apply

```sh
kubectl label namespace kgf-nexus kgf-nexus.khuriwalgroup.com/trust-bundle=enabled
kubectl apply -f deploy/k8s/cert-issuer.yaml
kubectl apply -f deploy/k8s/cert-certificates.yaml
kubectl apply -f deploy/k8s/cert-alerts.yaml     # needs the Prometheus Operator CRDs
kubectl -n kgf-nexus get certificate               # all READY=True
```

## 3. Mount into a Deployment

This goes in the chart's Deployment template. Shown for the scheduler:

```yaml
spec:
  template:
    spec:
      containers:
        - name: scheduler
          env:
            - { name: NEXUS_TLS_CERT_FILE,        value: /etc/nexus/tls/tls.crt }
            - { name: NEXUS_TLS_KEY_FILE,         value: /etc/nexus/tls/tls.key }
            - { name: NEXUS_TLS_CLIENT_CERT_FILE, value: /etc/nexus/tls/tls.crt }
            - { name: NEXUS_TLS_CLIENT_KEY_FILE,  value: /etc/nexus/tls/tls.key }
            - { name: NEXUS_TLS_CA_FILE,          value: /etc/nexus/trust/ca.crt }
            - { name: NEXUS_TLS_REQUIRE_CLIENT_CERT, value: "1" }  # only after TLS.md §4.1 is done everywhere
            - { name: NEXUS_REQUIRE_TLS,          value: "1" }
          volumeMounts:
            # Mount the DIRECTORY. Never use subPath: a subPath mount is not
            # updated when the secret changes, so the process would keep the
            # old certificate until it expired.
            - { name: tls,   mountPath: /etc/nexus/tls,   readOnly: true }
            - { name: trust, mountPath: /etc/nexus/trust, readOnly: true }
      volumes:
        - name: tls
          secret:
            secretName: nexus-scheduler-tls
            # 0440 plus fsGroup: the key is readable by the service user and nobody else.
            defaultMode: 0440
        - name: trust
          configMap:
            name: nexus-ca-bundle
      securityContext:
        runAsUser: 10001
        fsGroup: 10001
```

No reloader sidecar and no restart annotation are needed. The process re-reads
the mounted files itself (docs/TLS.md §5).
