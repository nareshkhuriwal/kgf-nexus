{{/*
Copyright 2026 Khuriwal Group
SPDX-License-Identifier: Apache-2.0

Platform helpers: pod placement, PodDisruptionBudgets, HorizontalPodAutoscalers
and service-to-service TLS. Kept apart from _helpers.tpl so the two can change
independently.
*/}}

{{/*
`helm upgrade --reuse-values` replays the PREVIOUS release's computed values
and ignores this chart's defaults, so sections added since then are simply
absent. Fail with the fix rather than a nil-pointer error deep in a template.
*/}}
{{- define "nexus.valuesComplete" -}}
{{- range $section := list "placement" "podDisruptionBudget" "autoscaling" "tls" "clusterController" "artifactGC" "serviceAccounts" -}}
{{- if not (hasKey $.Values $section) -}}
{{- fail (printf "values section %q is missing: this looks like `helm upgrade --reuse-values` from an older chart version, which drops the new version's defaults. Upgrade with --reset-then-reuse-values (keeps your previous overrides) or with -f <your values file> (docs/OPERATIONS.md §20.2)" $section) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Pod-spec placement fields for one component. Rendered at pod-spec level:
  {{- include "nexus.placement" (list $root "api-gateway") | nindent 6 }}
*/}}
{{- define "nexus.placement" -}}
{{- $root := index . 0 -}}{{- $component := index . 1 -}}
{{- $p := $root.Values.placement -}}
{{- /* Optional third argument: per-workload nodeSelector / tolerations /
       affinity that REPLACE the global ones (agents[] entries). */ -}}
{{- $o := dict -}}{{- if gt (len .) 2 }}{{ $o = index . 2 | default dict }}{{ end -}}
{{- $nodeSelector := ternary $o.nodeSelector $p.nodeSelector (hasKey $o "nodeSelector") -}}
{{- $tolerations := ternary $o.tolerations $p.tolerations (hasKey $o "tolerations") -}}
{{- $affinity := ternary $o.affinity $p.affinity (hasKey $o "affinity") -}}
{{- with $p.priorityClassName }}
priorityClassName: {{ . | quote }}
{{- end }}
{{- with $nodeSelector }}
nodeSelector: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $tolerations }}
tolerations: {{- toYaml . | nindent 2 }}
{{- end }}
{{- with $affinity }}
affinity: {{- toYaml . | nindent 2 }}
{{- end }}
{{- if $p.topologySpread.enabled }}
topologySpreadConstraints:
{{- range $p.topologySpread.topologyKeys }}
  - maxSkew: {{ $p.topologySpread.maxSkew }}
    topologyKey: {{ . }}
    whenUnsatisfiable: {{ $p.topologySpread.whenUnsatisfiable }}
    # Only this revision's pods count, so a rolling update is not skewed by
    # the pods it is about to replace.
    matchLabelKeys: [pod-template-hash]
    labelSelector:
      matchLabels: {{- include "nexus.selector" (list $root $component) | nindent 8 }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Effective autoscaling settings: the global block with a per-component override
merged over it. args: root, override dict (may be empty), default-enabled bool.
Returns YAML; callers use fromYaml.
*/}}
{{- define "nexus.autoscalingFor" -}}
{{- $root := index . 0 -}}{{- $over := index . 1 | default dict -}}{{- $defaultOn := index . 2 -}}
{{- $a := deepCopy $root.Values.autoscaling -}}
{{- $_ := set $a "enabled" (and $defaultOn $root.Values.autoscaling.enabled) -}}
{{- $a = mergeOverwrite $a (deepCopy $over) -}}
{{- toYaml $a -}}
{{- end -}}

{{/*
A HorizontalPodAutoscaler for Deployment <name>. args: root, name, component,
effective autoscaling dict.
*/}}
{{- define "nexus.hpa" -}}
{{- $root := index . 0 -}}{{- $name := index . 1 -}}{{- $component := index . 2 -}}{{- $a := index . 3 -}}
{{- if gt (int $a.minReplicas) (int $a.maxReplicas) -}}
{{- fail (printf "autoscaling for %s: minReplicas %v is above maxReplicas %v" $name $a.minReplicas $a.maxReplicas) -}}
{{- end }}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ $name }}
  labels: {{- include "nexus.labels" (list $root $component) | nindent 4 }}
spec:
  scaleTargetRef: { apiVersion: apps/v1, kind: Deployment, name: {{ $name }} }
  minReplicas: {{ $a.minReplicas }}
  maxReplicas: {{ $a.maxReplicas }}
  metrics:
    - type: Resource
      resource:
        name: cpu
        target: { type: Utilization, averageUtilization: {{ $a.targetCPUUtilizationPercentage }} }
    {{- with $a.targetMemoryUtilizationPercentage }}
    - type: Resource
      resource:
        name: memory
        target: { type: Utilization, averageUtilization: {{ . }} }
    {{- end }}
  {{- with $a.behavior }}
  behavior: {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}

{{/*
A PodDisruptionBudget, rendered whatever the replica count. args: root, name,
component, replicas (the floor: HPA minReplicas when autoscaled), override dict.
*/}}
{{- define "nexus.pdb" -}}
{{- $root := index . 0 -}}{{- $name := index . 1 -}}{{- $component := index . 2 -}}
{{- $replicas := int (index . 3) -}}{{- $over := index . 4 | default dict -}}
{{- $p := mergeOverwrite (deepCopy $root.Values.podDisruptionBudget) (deepCopy $over) -}}
{{- if $p.enabled -}}
{{- $hasMin := not (kindIs "invalid" $p.minAvailable) -}}
{{- $hasMax := not (kindIs "invalid" $p.maxUnavailable) -}}
{{- if and $hasMin $hasMax -}}
{{- fail (printf "podDisruptionBudget for %s: set minAvailable OR maxUnavailable, not both (set the other to null)" $name) -}}
{{- end -}}
{{- if not (or $hasMin $hasMax) -}}
{{- fail (printf "podDisruptionBudget for %s: set minAvailable or maxUnavailable" $name) -}}
{{- end -}}
{{- if and $hasMin (not (kindIs "string" $p.minAvailable)) (ge (int $p.minAvailable) $replicas) -}}
{{- fail (printf "podDisruptionBudget for %s: minAvailable %v with %d replica(s) would block every node drain; use maxUnavailable or add replicas" $name $p.minAvailable $replicas) -}}
{{- end }}
---
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ $name }}
  labels: {{- include "nexus.labels" (list $root $component) | nindent 4 }}
spec:
  {{- if $hasMin }}
  minAvailable: {{ $p.minAvailable }}
  {{- else }}
  maxUnavailable: {{ $p.maxUnavailable }}
  {{- end }}
  selector:
    matchLabels: {{- include "nexus.selector" (list $root $component) | nindent 6 }}
{{- end -}}
{{- end -}}

{{/*
gRPC TLS environment (docs/TLS.md §3). args: root, role.
  server  -- serves gRPC and dials peers (the control-plane services)
  client  -- dials only (agents, cluster controller)
Mounted by nexus.tlsVolumeMounts at /etc/nexus/tls (and the CA bundle at
/etc/nexus/tls-ca when tls.caBundleConfigMap is set).
*/}}
{{- define "nexus.tlsEnv" -}}
{{- $root := index . 0 -}}{{- $role := index . 1 -}}
{{- $t := $root.Values.tls -}}
{{- if $t.enabled }}
{{- if eq $role "server" }}
- { name: NEXUS_TLS_CERT_FILE, value: /etc/nexus/tls/tls.crt }
- { name: NEXUS_TLS_KEY_FILE, value: /etc/nexus/tls/tls.key }
- { name: NEXUS_TLS_REQUIRE_CLIENT_CERT, value: {{ ternary "1" "0" $t.requireClientCert | quote }} }
{{- end }}
- { name: NEXUS_TLS_CLIENT_CERT_FILE, value: /etc/nexus/tls/tls.crt }
- { name: NEXUS_TLS_CLIENT_KEY_FILE, value: /etc/nexus/tls/tls.key }
- { name: NEXUS_TLS_CA_FILE, value: {{ ternary "/etc/nexus/tls-ca/ca.crt" "/etc/nexus/tls/ca.crt" (ne $t.caBundleConfigMap "") | quote }} }
- { name: NEXUS_REQUIRE_TLS, value: {{ ternary "1" "0" $t.requirePeerTLS | quote }} }
- { name: NEXUS_TLS_RELOAD_INTERVAL, value: {{ $t.reloadInterval | quote }} }
{{- end }}
{{- end -}}

{{- define "nexus.tlsVolumeMounts" -}}
{{- $t := .Values.tls -}}
{{- if $t.enabled }}
- { name: nexus-tls, mountPath: /etc/nexus/tls, readOnly: true }
{{- if $t.caBundleConfigMap }}
- { name: nexus-tls-ca, mountPath: /etc/nexus/tls-ca, readOnly: true }
{{- end }}
{{- end }}
{{- end -}}

{{/*
No subPath and no items: a renewed Secret must reach the pod in place, or hot
reload silently becomes "serve the old certificate until it expires".
args: root, leaf name (the <leaf> in nexus-<leaf>-tls).
*/}}
{{- define "nexus.tlsVolumes" -}}
{{- $root := index . 0 -}}{{- $leaf := index . 1 -}}
{{- $t := $root.Values.tls -}}
{{- if $t.enabled }}
- name: nexus-tls
  secret: { secretName: {{ printf "nexus-%s-tls" $leaf }}, defaultMode: 0400 }
{{- if $t.caBundleConfigMap }}
- name: nexus-tls-ca
  configMap: { name: {{ $t.caBundleConfigMap }} }
{{- end }}
{{- end }}
{{- end -}}

{{/* The TLS leaf each control-plane Deployment mounts. */}}
{{- define "nexus.tlsLeafFor" -}}
{{- $root := index . 0 -}}{{- $name := index . 1 -}}
{{- $isScheduler := false -}}
{{- range $root.Values.schedulers }}{{ if eq .name $name }}{{ $isScheduler = true }}{{ end }}{{ end -}}
{{- ternary "scheduler" $name $isScheduler -}}
{{- end -}}
