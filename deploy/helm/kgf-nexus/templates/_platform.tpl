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
{{- range $section := list "placement" "podDisruptionBudget" "autoscaling" "tls" "clusterController" "artifactGC" "serviceAccounts" "resultStore" "tracing" "agentPolicy" "kgf" "claimPlacement" "components" "cluster" "networkPolicy" -}}
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
       affinity that REPLACE the global ones (agents[] and schedulers[]
       entries), and a topologySpread merged key by key over the global one
       (schedulers[] entries). */ -}}
{{- $o := dict -}}{{- if gt (len .) 2 }}{{ $o = index . 2 | default dict }}{{ end -}}
{{- $nodeSelector := ternary $o.nodeSelector $p.nodeSelector (hasKey $o "nodeSelector") -}}
{{- $tolerations := ternary $o.tolerations $p.tolerations (hasKey $o "tolerations") -}}
{{- $affinity := ternary $o.affinity $p.affinity (hasKey $o "affinity") -}}
{{- /* Key by key with hasKey, not sprig's merge, which treats false as
       unset: a scheduler's topologySpread.enabled=false must win. */ -}}
{{- $ts := deepCopy $p.topologySpread -}}
{{- range $k, $v := ($o.topologySpread | default dict) }}{{ $_ := set $ts $k $v }}{{ end -}}
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
{{- if $ts.enabled }}
topologySpreadConstraints:
{{- range $ts.topologyKeys }}
  - maxSkew: {{ $ts.maxSkew }}
    topologyKey: {{ . }}
    whenUnsatisfiable: {{ $ts.whenUnsatisfiable }}
    # Only this revision's pods count, so a rolling update is not skewed by
    # the pods it is about to replace.
    matchLabelKeys: [pod-template-hash]
    labelSelector:
      matchLabels: {{- include "nexus.selector" (list $root $component) | nindent 8 }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Placement overrides for one schedulers[] entry, as YAML for nexus.placement's
override argument: only the keys the entry sets, so an absent one keeps
placement.*. args: root, scheduler entry (nil for the other services).
*/}}
{{- define "nexus.schedulerPlacement" -}}
{{- $s := index . 1 | default dict -}}
{{- $o := dict -}}
{{- range $k := list "nodeSelector" "tolerations" "affinity" "topologySpread" }}{{ if hasKey $s $k }}{{ $_ := set $o $k (index $s $k) }}{{ end }}{{ end -}}
{{- toYaml $o -}}
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
"true" when the cluster controller owns the scheduler HPAs (ADR-0036): it
runs (clusterController.enabled), in apply mode (apply.enabled), with
apply.schedulerAutoscaling (unset counts as true). ONE condition for both
halves of the opt-in, so they cannot disagree: templates/cluster-controller.yaml
binds patch on the scheduler HPAs under it, and nexus.hpa annotates those HPAs
nexus.khuriwalgroup.com/autoscaling-owner=cluster-controller under it.
*/}}
{{- define "nexus.controllerOwnsSchedulerHPAs" -}}
{{- $cc := .Values.clusterController | default dict -}}
{{- $apply := $cc.apply | default dict -}}
{{- if and $cc.enabled $apply.enabled (ne (toString $apply.schedulerAutoscaling) "false") }}true{{ end -}}
{{- end -}}

{{/*
"true" when the cluster controller owns one agent Deployment's replica count:
the agent opts in (agents[].replicasManagedByController) and the controller
that scales it can apply. In a release that runs the control plane that is
this release's controller in apply mode (clusterController.enabled and
apply.enabled); a worker-pool release has no controller of its own, and the
one scaling its agents runs in the control-plane release, so there the
opt-in alone decides. args: root, agent entry.
*/}}
{{- define "nexus.controllerManagesAgentReplicas" -}}
{{- $root := index . 0 -}}{{- $a := index . 1 -}}
{{- $cc := $root.Values.clusterController | default dict -}}
{{- if $a.replicasManagedByController -}}
{{- if or (not (include "nexus.controlPlaneEnabled" $root)) (and $cc.enabled ($cc.apply | default dict).enabled) }}true{{ end -}}
{{- end -}}
{{- end -}}

{{/*
An agent Deployment's spec.replicas. The value, unless the cluster
controller owns the count (nexus.controllerManagesAgentReplicas) and the
Deployment exists: then the live count, so `helm upgrade` never resets what
the controller scaled to -- a reset it would only undo again, after its
cooldown, possibly draining workers on the way. 0 is a count like any other.
args: root, agent entry, the live Deployment (an empty dict when there is
none, or under `helm template`, where lookup returns nothing).
*/}}
{{- define "nexus.agentReplicas" -}}
{{- $root := index . 0 -}}{{- $a := index . 1 -}}{{- $live := index . 2 | default dict -}}
{{- $n := $a.replicas | default 1 -}}
{{- if include "nexus.controllerManagesAgentReplicas" (list $root $a) -}}
{{- $spec := $live.spec | default dict -}}
{{- if hasKey $spec "replicas" }}{{ $n = $spec.replicas }}{{ end -}}
{{- end -}}
{{- int $n -}}
{{- end -}}

{{/*
The autoscaling settings of a controller-owned HPA. The controller patches
four fields of an HPA it owns (internal/platform/autoscaling.go):
minReplicas, maxReplicas, the CPU utilization target and
behavior.scaleDown.stabilizationWindowSeconds. Re-rendering them from values
would make every `helm upgrade` revert what the controller applied, and the
controller would then walk them back in one-minute steps. So while the LIVE
HPA carries the owner annotation, those four come from it; everything else
(the memory metric, the scale-down policies, scale-up) still comes from
values. A live HPA without the annotation is still Helm's: its values win,
which is also what the first upgrade that hands it to the controller needs.
args: effective autoscaling dict, the live HPA (an empty dict when there is
none, or under `helm template`, where lookup returns nothing). Returns YAML.
*/}}
{{- define "nexus.hpaOwnedSettings" -}}
{{- $a := deepCopy (index . 0) -}}{{- $live := index . 1 | default dict -}}
{{- $ann := (($live.metadata | default dict).annotations | default dict) -}}
{{- if eq (index $ann "nexus.khuriwalgroup.com/autoscaling-owner" | default "") "cluster-controller" -}}
{{- $spec := $live.spec | default dict -}}
{{- /* Kubernetes' default when minReplicas is unset. */ -}}
{{- $_ := set $a "minReplicas" (int ($spec.minReplicas | default 1)) -}}
{{- with $spec.maxReplicas }}{{ $_ := set $a "maxReplicas" (int .) }}{{ end -}}
{{- $found := false -}}
{{- range ($spec.metrics | default list) -}}
{{- $r := .resource | default dict -}}{{- $t := $r.target | default dict -}}
{{- if and (not $found) (eq (.type | default "") "Resource") (eq ($r.name | default "") "cpu") (eq ($t.type | default "") "Utilization") $t.averageUtilization -}}
{{- $_ := set $a "targetCPUUtilizationPercentage" (int $t.averageUtilization) -}}
{{- $found = true -}}
{{- end -}}
{{- end -}}
{{- $window := ((($spec.behavior | default dict).scaleDown | default dict).stabilizationWindowSeconds) -}}
{{- if not (kindIs "invalid" $window) -}}
{{- $b := deepCopy ($a.behavior | default dict) -}}
{{- $sd := deepCopy ($b.scaleDown | default dict) -}}
{{- $_ := set $sd "stabilizationWindowSeconds" (int $window) -}}
{{- $_ := set $b "scaleDown" $sd -}}
{{- $_ := set $a "behavior" $b -}}
{{- end -}}
{{- end -}}
{{- toYaml $a -}}
{{- end -}}

{{/*
A HorizontalPodAutoscaler for Deployment <name>. args: root, name, component,
effective autoscaling dict, and optionally whether the cluster controller owns
it (nexus.controllerOwnsSchedulerHPAs, for scheduler HPAs only): annotated for
the controller, and its four managed fields kept at their live values.
*/}}
{{- define "nexus.hpa" -}}
{{- $root := index . 0 -}}{{- $name := index . 1 -}}{{- $component := index . 2 -}}{{- $a := index . 3 -}}
{{- $owned := false -}}{{- if gt (len .) 4 }}{{ $owned = index . 4 }}{{ end -}}
{{- if $owned -}}
{{- $live := lookup "autoscaling/v2" "HorizontalPodAutoscaler" $root.Release.Namespace $name -}}
{{- $a = include "nexus.hpaOwnedSettings" (list $a $live) | fromYaml -}}
{{- end -}}
{{- if gt (int $a.minReplicas) (int $a.maxReplicas) -}}
{{- fail (printf "autoscaling for %s: minReplicas %v is above maxReplicas %v" $name $a.minReplicas $a.maxReplicas) -}}
{{- end }}
---
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: {{ $name }}
  labels: {{- include "nexus.labels" (list $root $component) | nindent 4 }}
  {{- if $owned }}
  # ADR-0036: the cluster controller applies the scheduler settings saved in
  # the console to this HPA (clusterController.apply.schedulerAutoscaling).
  annotations:
    nexus.khuriwalgroup.com/autoscaling-owner: cluster-controller
  {{- end }}
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
