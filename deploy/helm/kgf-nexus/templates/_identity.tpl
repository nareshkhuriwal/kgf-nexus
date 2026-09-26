{{/*
Copyright 2026 Khuriwal Group
SPDX-License-Identifier: Apache-2.0

Pod identity (ServiceAccounts) and agent hardware (GPU requests, pool
placement). values.yaml `serviceAccounts` and `agents` document the knobs.
*/}}

{{- define "nexus.saDefaultNames" -}}
apiGateway: nexus-api-gateway
workflowManager: nexus-workflow-manager
dagEngine: nexus-dag-engine
scheduler: nexus-scheduler
resourceManager: nexus-resource-manager
eventService: nexus-event-service
agent: nexus-agent
artifactGC: nexus-artifact-gc
ui: nexus-ui
jobs: nexus-jobs
{{- end -}}

{{/*
The effective ServiceAccount of one pod, as YAML {name, create, automount,
annotations, podLabels}. args: root, component key, per-agent override (dict
or nil). An override REPLACES serviceAccounts.<key>; the global annotations
still apply.
*/}}
{{- define "nexus.saFor" -}}
{{- $root := index . 0 -}}{{- $key := index . 1 -}}{{- $over := index . 2 | default dict -}}
{{- $executors := "" -}}{{- if gt (len .) 3 }}{{ $executors = index . 3 | default "" }}{{ end -}}
{{- $sas := $root.Values.serviceAccounts | default dict -}}
{{- $cfg := index $sas $key | default dict -}}
{{- if $over }}{{ $cfg = $over }}{{ end -}}
{{- $create := ternary true $sas.create (kindIs "invalid" $sas.create) -}}
{{- $name := $cfg.name | default "" -}}
{{- if not $name -}}
{{- $name = ternary (index (include "nexus.saDefaultNames" $root | fromYaml) $key) "default" $create -}}
{{- end -}}
{{- $automount := $sas.automountServiceAccountToken | default false -}}
{{- if and (eq $key "agent") (kindIs "invalid" $cfg.automountServiceAccountToken) -}}
{{- /* The kubernetes executor launches task Jobs through the API. */ -}}
{{- $automount = or $automount (has "kubernetes" (splitList "," ($executors | nospace))) -}}
{{- end -}}
{{- if not (kindIs "invalid" $cfg.automountServiceAccountToken) }}{{ $automount = $cfg.automountServiceAccountToken }}{{ end -}}
{{- $ann := merge (deepCopy ($cfg.annotations | default dict)) (deepCopy ($sas.annotations | default dict)) -}}
name: {{ $name | quote }}
create: {{ $create }}
automount: {{ $automount }}
annotations: {{ toYaml $ann | nindent 2 }}
podLabels: {{ toYaml ($cfg.podLabels | default dict) | nindent 2 }}
{{- end -}}

{{/* Pod-spec fields for an effective ServiceAccount (nexus.saFor output). */}}
{{- define "nexus.podIdentity" -}}
serviceAccountName: {{ .name }}
automountServiceAccountToken: {{ .automount }}
{{- end -}}

{{/* Pod-template labels a ServiceAccount asks for (Azure Workload Identity). */}}
{{- define "nexus.saPodLabels" -}}
{{- range $k, $v := .podLabels }}
{{ $k }}: {{ $v | toString | quote }}
{{- end }}
{{- end -}}

{{/* The GPU extended-resource name for one agent entry. args: root, agent. */}}
{{- define "nexus.agentGPUResource" -}}
{{- $root := index . 0 -}}{{- $a := index . 1 -}}
{{- (($a.gpu | default dict).resourceName) | default $root.Values.agentDefaults.gpuResourceName | default "nvidia.com/gpu" -}}
{{- end -}}

{{- define "nexus.agentRequestsGPUs" -}}
{{- $a := index . 1 -}}
{{- $g := $a.gpu | default dict -}}
{{- if and (gt (int ($a.gpuCount | default 0)) 0) (or (kindIs "invalid" $g.requestDevices) $g.requestDevices) }}true{{ end -}}
{{- end -}}

{{/*
Container resources for one agent: its resources plus, for a GPU agent, the
device request that makes advertised capacity equal allocated capacity. An
explicit device limit that disagrees with gpuCount is refused: the scheduler
would place work on GPUs the pod does not have (or strand ones it does).
args: root, agent.
*/}}
{{- define "nexus.agentResources" -}}
{{- $root := index . 0 -}}{{- $a := index . 1 -}}
{{- $r := deepCopy ($a.resources | default dict) -}}
{{- $count := int ($a.gpuCount | default 0) -}}
{{- $res := include "nexus.agentGPUResource" . -}}
{{- $g := $a.gpu | default dict -}}
{{- if include "nexus.agentRequestsGPUs" . -}}
{{- $limits := $r.limits | default dict -}}
{{- $requests := $r.requests | default dict -}}
{{- range $m := list $limits $requests -}}
{{- if and (hasKey $m $res) (ne (int (index $m $res)) $count) -}}
{{- fail (printf "agents[%s]: resources %s=%v disagrees with gpuCount=%d; the agent would advertise GPUs the pod was not allocated (omit it: the chart sets it from gpuCount)" $a.name $res (index $m $res) $count) -}}
{{- end -}}
{{- end -}}
{{- $_ := set $limits $res $count -}}
{{- $_ := set $requests $res $count -}}
{{- $_ := set $r "limits" $limits -}}
{{- $_ := set $r "requests" $requests -}}
{{- else if gt $count 0 -}}
{{- if not (has "simulated=true" (splitList "," ($a.labels | default "" | nospace))) -}}
{{- fail (printf "agents[%s]: gpuCount=%d with gpu.requestDevices=false advertises GPUs the pod is not allocated. That is SIMULATED capacity and must carry the label simulated=true (a production agent requests its devices: leave requestDevices unset)" $a.name $count) -}}
{{- end -}}
{{- end -}}
{{- toYaml $r -}}
{{- end -}}

{{/*
Placement overrides for one agent entry: its own nodeSelector/tolerations/
affinity replace placement.*, and a GPU agent also tolerates the
<resourceName>:NoSchedule taint (AKS/EKS pools in deploy/terraform carry
nvidia.com/gpu=present:NoSchedule; GKE adds the toleration itself for pods that
request GPUs). Tolerating a taint only permits a node, it never attracts, so
the GPU pool is chosen by nodeSelector/affinity plus the device request.
args: root, agent. Returns YAML for nexus.placement's override argument.
*/}}
{{- define "nexus.agentPlacement" -}}
{{- $root := index . 0 -}}{{- $a := index . 1 -}}
{{- $o := dict -}}
{{- if hasKey $a "nodeSelector" }}{{ $_ := set $o "nodeSelector" $a.nodeSelector }}{{ end -}}
{{- if hasKey $a "affinity" }}{{ $_ := set $o "affinity" $a.affinity }}{{ end -}}
{{- $tol := ternary $a.tolerations $root.Values.placement.tolerations (hasKey $a "tolerations") | default list -}}
{{- if include "nexus.agentRequestsGPUs" . -}}
{{- $res := include "nexus.agentGPUResource" . -}}
{{- $has := false -}}
{{- range $tol }}{{ if eq (.key | default "") $res }}{{ $has = true }}{{ end }}{{ end -}}
{{- if and (not $has) (ne (($a.gpu | default dict).tolerate | toString) "false") -}}
{{- $tol = append $tol (dict "key" $res "operator" "Exists" "effect" "NoSchedule") -}}
{{- end -}}
{{- end -}}
{{- $_ := set $o "tolerations" $tol -}}
{{- toYaml $o -}}
{{- end -}}
