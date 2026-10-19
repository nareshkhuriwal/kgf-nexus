{{/*
Copyright 2026 Khuriwal Group
SPDX-License-Identifier: Apache-2.0

What one release runs (components.*), the runtime identity every Nexus process
reports (cluster.name and the downward API), and how the agents reach the
control plane (agentDefaults.controlPlane, agentDefaults.nats). ADR-0035,
docs/OPERATIONS.md §20.16.
*/}}

{{/*
"true" when this release runs the control plane: the six services, the
schedulers, the console, the migrate/bootstrap/db-roles Jobs, artifact GC and
the in-cluster postgres/minio/nats. components.controlPlane.enabled=false is a
WORKER-POOL release: agents only (and the cluster controller, when enabled),
dialing a control plane installed by another release. Absent (an older values
file) counts as enabled, so nothing an existing install renders changes.
*/}}
{{- define "nexus.controlPlaneEnabled" -}}
{{- $c := (.Values.components | default dict).controlPlane | default dict -}}
{{- if or (kindIs "invalid" $c.enabled) $c.enabled }}true{{ end -}}
{{- end -}}

{{/*
"true" when the chart runs MinIO itself: minio.enabled in a release that runs
the control plane. A worker-pool release never does, so its agents treat the
store as external (minio.endpoint) whatever minio.enabled says.
*/}}
{{- define "nexus.minioInRelease" -}}
{{- if and .Values.minio.enabled (include "nexus.controlPlaneEnabled" .) }}true{{ end -}}
{{- end -}}

{{/*
"true" when the agents connect to a NATS broker: the platform runs one
(nats.enabled) and agentDefaults.nats.url names it. Either off, the agents
long-poll (ADR-0003) and render no NATS client settings at all.
*/}}
{{- define "nexus.agentsUseNats" -}}
{{- $url := ((.Values.agentDefaults | default dict).nats | default dict).url -}}
{{- if kindIs "invalid" $url }}{{ $url = "nats://nats:4222" }}{{ end -}}
{{- if and .Values.nats.enabled $url }}true{{ end -}}
{{- end -}}

{{/* The broker URL the agents dial (NEXUS_NATS_URL). */}}
{{- define "nexus.agentNatsURL" -}}
{{- $url := ((.Values.agentDefaults | default dict).nats | default dict).url -}}
{{- if kindIs "invalid" $url }}{{ $url = "nats://nats:4222" }}{{ end -}}
{{- $url -}}
{{- end -}}

{{/*
Refuse a release that would render nothing useful, or agents that cannot
reach what they need. Called from nexus.required, so every render checks it.
*/}}
{{- define "nexus.componentsRequired" -}}
{{- $cp := .Values.agentDefaults.controlPlane | default dict -}}
{{- $cpTLS := $cp.tls | default dict -}}
{{- if not (include "nexus.controlPlaneEnabled" .) -}}
{{- if and (not .Values.agents) (not .Values.clusterController.enabled) -}}
{{- fail "components.controlPlane.enabled=false renders only the agents (and the cluster controller), and agents is empty: a worker-pool release needs at least one agents[] entry" -}}
{{- end -}}
{{- if and .Values.tls.enabled .Values.tls.certManager.enabled (not .Values.tls.certManager.issuerRef.name) -}}
{{- fail "tls.enabled in a worker-pool release (components.controlPlane.enabled=false) needs tls.certManager.issuerRef: the ClusterIssuer that signed the control plane's leaves. A self-signed CA bootstrapped in this namespace would not be trusted by the api-gateway (or set tls.certManager.enabled=false and provide nexus-agent-tls yourself)" -}}
{{- end -}}
{{- if include "nexus.agentsUseNats" . -}}
{{- $n := .Values.nats -}}
{{- if not (has $n.auth.mode (list "nkey" "none")) -}}
{{- fail (printf "nats.auth.mode=%q: use nkey (production) or none (development only)" $n.auth.mode) -}}
{{- end -}}
{{- if $n.tls.enabled -}}
{{- if and $n.tls.verifyClients (not $n.tls.clientSecrets.agent) -}}
{{- fail "nats.tls.verifyClients in a worker-pool release needs nats.tls.clientSecrets.agent: a Secret with tls.crt, tls.key and ca.crt from the broker's CA (with tls.certManager this is nexus-agent-tls)" -}}
{{- end -}}
{{- if not (or $n.tls.caSecret $n.tls.clientSecrets.agent) -}}
{{- fail "nats.tls.enabled in a worker-pool release needs nats.tls.caSecret (a Secret with ca.crt) so the agents can verify the broker" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and $cpTLS.enabled (not $cpTLS.caSecretName) -}}
{{- fail "agentDefaults.controlPlane.tls.enabled needs agentDefaults.controlPlane.tls.caSecretName: a Secret in this namespace with key ca.crt, the CA that issued the control-plane endpoint's certificate (with this release's own mTLS, tls.enabled, the agents already verify api-gateway against their leaf's CA: leave controlPlane.tls off)" -}}
{{- end -}}
{{- if and $cpTLS.serverName (not (or $cpTLS.enabled .Values.tls.enabled)) -}}
{{- fail "agentDefaults.controlPlane.tls.serverName has no effect without TLS: set agentDefaults.controlPlane.tls.enabled (or tls.enabled), or clear serverName" -}}
{{- end -}}
{{- end -}}

{{/*
Runtime identity (ADR-0035): the pod, namespace and node a Nexus process runs
on, from the downward API, and the registered cluster it belongs to. Every
control-plane service, scheduler, agent, artifact-gc and the cluster
controller reports them in its service-instance row, so the console can place
each instance on a pod and node. Unset (docker run, compose), the binaries
fall back to the hostname and report no cluster. args: root, cluster name
(empty = NEXUS_CLUSTER_NAME not rendered).
*/}}
{{- define "nexus.runtimeIdentityEnv" -}}
{{- $cluster := index . 1 | default "" -}}
- name: NEXUS_POD_NAME
  valueFrom: { fieldRef: { fieldPath: metadata.name } }
- name: NEXUS_POD_NAMESPACE
  valueFrom: { fieldRef: { fieldPath: metadata.namespace } }
- name: NEXUS_NODE_NAME
  valueFrom: { fieldRef: { fieldPath: spec.nodeName } }
{{- with $cluster }}
- { name: NEXUS_CLUSTER_NAME, value: {{ . | quote }} }
{{- end }}
{{- end -}}

{{/* The cluster name every component but the agents reports: cluster.name. */}}
{{- define "nexus.clusterName" -}}
{{- (.Values.cluster | default dict).name | default "" -}}
{{- end -}}

{{/*
The cluster the agents report and label their claims with (ADR-0031):
claimPlacement.clusterName when set, else cluster.name.
*/}}
{{- define "nexus.agentClusterName" -}}
{{- .Values.claimPlacement.clusterName | default (include "nexus.clusterName" .) -}}
{{- end -}}

{{/*
gRPC TLS for the agents' dial to the control plane. This release's mTLS
(tls.enabled) gives them a client leaf and its CA, exactly as nexus.tlsEnv's
client role; agentDefaults.controlPlane.tls adds server verification against
caSecretName's ca.crt for an endpoint this release's mTLS does not cover (a
load balancer that terminates TLS, another cluster's gateway), and serverName
overrides the name the certificate is verified against. controlPlane.tls alone
does not set NEXUS_REQUIRE_TLS: the configured CA already makes the dial TLS,
and NEXUS_REQUIRE_TLS would also refuse a plaintext NATS connection.
*/}}
{{- define "nexus.agentTlsEnv" -}}
{{- $t := .Values.tls -}}
{{- $cpTLS := (.Values.agentDefaults.controlPlane | default dict).tls | default dict -}}
{{- $caFile := ternary "/etc/nexus/tls-ca/ca.crt" "/etc/nexus/tls/ca.crt" (ne $t.caBundleConfigMap "") -}}
{{- if $cpTLS.enabled }}{{ $caFile = "/etc/nexus/control-plane-ca/ca.crt" }}{{ end -}}
{{- if $t.enabled }}
- { name: NEXUS_TLS_CLIENT_CERT_FILE, value: /etc/nexus/tls/tls.crt }
- { name: NEXUS_TLS_CLIENT_KEY_FILE, value: /etc/nexus/tls/tls.key }
{{- end }}
{{- if or $t.enabled $cpTLS.enabled }}
- { name: NEXUS_TLS_CA_FILE, value: {{ $caFile | quote }} }
{{- end }}
{{- if $t.enabled }}
- { name: NEXUS_REQUIRE_TLS, value: {{ ternary "1" "0" $t.requirePeerTLS | quote }} }
{{- end }}
{{- if or $t.enabled $cpTLS.enabled }}
- { name: NEXUS_TLS_RELOAD_INTERVAL, value: {{ $t.reloadInterval | quote }} }
{{- end }}
{{- with $cpTLS.serverName }}
- { name: NEXUS_TLS_SERVER_NAME, value: {{ . | quote }} }
{{- end }}
{{- end -}}

{{- define "nexus.agentTlsVolumeMounts" -}}
{{- include "nexus.tlsVolumeMounts" . }}
{{- if ((.Values.agentDefaults.controlPlane | default dict).tls | default dict).enabled }}
- { name: control-plane-ca, mountPath: /etc/nexus/control-plane-ca, readOnly: true }
{{- end }}
{{- end -}}

{{- define "nexus.agentTlsVolumes" -}}
{{- include "nexus.tlsVolumes" (list . "agent") }}
{{- $cpTLS := (.Values.agentDefaults.controlPlane | default dict).tls | default dict -}}
{{- if $cpTLS.enabled }}
- name: control-plane-ca
  secret:
    secretName: {{ $cpTLS.caSecretName }}
    defaultMode: 0444
    items: [{ key: ca.crt, path: ca.crt }]
{{- end }}
{{- end -}}
