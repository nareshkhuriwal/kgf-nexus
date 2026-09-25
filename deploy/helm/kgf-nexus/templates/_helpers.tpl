{{/*
Copyright 2026 Khuriwal Group
SPDX-License-Identifier: Apache-2.0
*/}}

{{- define "nexus.required" -}}
{{- if not .Values.existingSecret -}}
{{- fail "existingSecret is required: the name of a Secret holding postgres-password, minio-root-user, minio-root-password and worker-api-key (deploy/terraform creates it)" -}}
{{- end -}}
{{- if not .Values.image.tag -}}
{{- fail "image.tag is required: pin an immutable tag (a git SHA), never latest" -}}
{{- end -}}
{{- end -}}

{{/* <registry><prefix><component>:<tag> */}}
{{- define "nexus.image" -}}
{{- $root := index . 0 -}}{{- $component := index . 1 -}}
{{- printf "%s%s%s:%s" $root.Values.image.registry $root.Values.image.repositoryPrefix $component $root.Values.image.tag -}}
{{- end -}}

{{- define "nexus.labels" -}}
{{- $root := index . 0 -}}{{- $component := index . 1 -}}
app.kubernetes.io/name: kgf-nexus
app.kubernetes.io/instance: {{ $root.Release.Name }}
app.kubernetes.io/component: {{ $component }}
app.kubernetes.io/version: {{ $root.Values.image.tag | quote }}
app.kubernetes.io/managed-by: {{ $root.Release.Service }}
helm.sh/chart: {{ printf "%s-%s" $root.Chart.Name $root.Chart.Version }}
{{- end -}}

{{- define "nexus.selector" -}}
{{- $root := index . 0 -}}{{- $component := index . 1 -}}
app.kubernetes.io/name: kgf-nexus
app.kubernetes.io/instance: {{ $root.Release.Name }}
app.kubernetes.io/component: {{ $component }}
{{- end -}}

{{- define "nexus.pullSecrets" -}}
{{- with .Values.image.pullSecrets }}
imagePullSecrets:
{{- range . }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
The database URL is assembled in the pod from the Secret, never rendered into
a manifest: $(NEXUS_DB_PASSWORD) is Kubernetes' own env expansion, so the
password exists only in the Secret and the container's environment.
The password must be URL-safe (deploy/terraform generates alphanumerics).
*/}}
{{- define "nexus.dbEnv" -}}
- name: NEXUS_DB_PASSWORD
  valueFrom:
    secretKeyRef: { name: {{ .Values.existingSecret }}, key: postgres-password }
- name: DATABASE_URL
  value: "postgres://{{ .Values.postgres.user }}:$(NEXUS_DB_PASSWORD)@{{ .Values.postgres.host }}:{{ .Values.postgres.port }}/{{ .Values.postgres.database }}?sslmode={{ .Values.postgres.sslmode }}"
{{- end -}}

{{/* Peer addresses and logging, shared by every control-plane service. */}}
{{- define "nexus.serviceEnv" -}}
{{ include "nexus.dbEnv" . }}
- { name: LOG_LEVEL, value: {{ .Values.logLevel | quote }} }
- { name: NEXUS_LOG_LEVEL, value: {{ .Values.logLevel | quote }} }
- { name: LOG_FORMAT, value: json }
- { name: NEXUS_NAMESPACE, value: {{ .Values.nexusNamespace | quote }} }
- { name: WORKFLOW_MANAGER_ADDR, value: "workflow-manager:9090" }
- { name: NEXUS_WORKFLOW_MANAGER_ADDR, value: "workflow-manager:9090" }
- { name: DAG_ENGINE_ADDR, value: "dag-engine:9090" }
- { name: NEXUS_DAG_ENGINE_ADDR, value: "dag-engine:9090" }
- { name: SCHEDULER_ADDR, value: {{ include "nexus.schedulerAddrs" . | quote }} }
- { name: NEXUS_SCHEDULER_ADDR, value: {{ include "nexus.schedulerAddrs" . | quote }} }
- { name: RESOURCE_MANAGER_ADDR, value: "resource-manager:9090" }
- { name: NEXUS_RESOURCE_MANAGER_ADDR, value: "resource-manager:9090" }
- { name: EVENT_SERVICE_ADDR, value: "event-service:9090" }
- { name: NEXUS_EVENT_SERVICE_ADDR, value: "event-service:9090" }
- { name: API_GATEWAY_ADDR, value: "api-gateway:9091" }
{{- end -}}

{{/*
Listener addresses. The services read different variable names for the same
two listeners (config drift recorded in F38); all are set, as compose does.
*/}}
{{- define "nexus.listenEnv" -}}
{{- $grpc := index . 0 -}}
- { name: HTTP_ADDR, value: ":8080" }
- { name: NEXUS_HTTP_ADDR, value: ":8080" }
- { name: NEXUS_REST_ADDR, value: ":8080" }
- { name: NEXUS_WFM_HEALTH_ADDR, value: ":8080" }
- { name: RM_HTTP_ADDR, value: ":8080" }
- { name: GRPC_ADDR, value: {{ printf ":%v" $grpc | quote }} }
- { name: NEXUS_GRPC_ADDR, value: {{ printf ":%v" $grpc | quote }} }
- { name: NEXUS_WFM_GRPC_ADDR, value: {{ printf ":%v" $grpc | quote }} }
- { name: RM_GRPC_ADDR, value: {{ printf ":%v" $grpc | quote }} }
{{- end -}}

{{- define "nexus.schedulerAddrs" -}}
{{- $out := list -}}
{{- range .Values.schedulers }}{{ $out = append $out (printf "%s:9090" .name) }}{{ end -}}
{{- join "," $out -}}
{{- end -}}

{{- define "nexus.resultStoreEnv" -}}
- name: NEXUS_S3_ACCESS_KEY_ID
  valueFrom: { secretKeyRef: { name: {{ .Values.existingSecret }}, key: minio-root-user } }
- name: RESULT_STORE_ACCESS_KEY
  valueFrom: { secretKeyRef: { name: {{ .Values.existingSecret }}, key: minio-root-user } }
- name: NEXUS_S3_SECRET_ACCESS_KEY
  valueFrom: { secretKeyRef: { name: {{ .Values.existingSecret }}, key: minio-root-password } }
- name: RESULT_STORE_SECRET_KEY
  valueFrom: { secretKeyRef: { name: {{ .Values.existingSecret }}, key: minio-root-password } }
- { name: RESULT_STORE_ENDPOINT, value: {{ .Values.minio.endpoint | quote }} }
- { name: NEXUS_RESULT_STORE_ENDPOINT, value: {{ .Values.minio.endpoint | quote }} }
- { name: NEXUS_S3_USE_TLS, value: {{ .Values.minio.useTLS | toString | quote }} }
- { name: RESULT_STORE_BUCKET, value: {{ .Values.minio.bucket | quote }} }
- { name: NEXUS_RESULT_STORE_BUCKET, value: {{ .Values.minio.bucket | quote }} }
- { name: RESULT_STORE_REGION, value: {{ .Values.minio.region | quote }} }
- { name: NEXUS_RESULT_STORE_REGION, value: {{ .Values.minio.region | quote }} }
- { name: RESULT_STORE_INLINE_THRESHOLD_BYTES, value: {{ .Values.minio.inlineThresholdBytes | int64 | toString | quote }} }
{{- end -}}

{{/*
NATS client settings for one role ("event-service" or "agent"). Under
nats.auth.mode=nkey the client authenticates with the seed nexus.natsVolumes
mounts, and NEXUS_NATS_REQUIRE_AUTH makes a missing seed fatal rather than a
quiet unauthenticated connection.
*/}}
{{- define "nexus.natsEnv" -}}
{{- $root := index . 0 -}}{{- $role := index . 1 -}}
{{- $n := $root.Values.nats -}}
{{- if $n.enabled }}
- { name: NEXUS_NATS_URL, value: "nats://nats:4222" }
{{- if eq $n.auth.mode "none" }}
- { name: NEXUS_NATS_REQUIRE_AUTH, value: "0" }
{{- else }}
- { name: NEXUS_NATS_REQUIRE_AUTH, value: "1" }
- { name: NEXUS_NATS_NKEY_SEED_FILE, value: /etc/nexus/nats/user.nk }
{{- end }}
{{- if $n.tls.enabled }}
- { name: NEXUS_NATS_TLS, value: "1" }
- { name: NEXUS_NATS_TLS_CA_FILE, value: /etc/nexus/nats-tls/ca.crt }
{{- if $n.tls.verifyClients }}
- { name: NEXUS_NATS_TLS_CERT_FILE, value: /etc/nexus/nats-tls/tls.crt }
- { name: NEXUS_NATS_TLS_KEY_FILE, value: /etc/nexus/nats-tls/tls.key }
{{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "nexus.natsVolumeMounts" -}}
{{- $n := .Values.nats -}}
{{- if $n.enabled }}
{{- if ne $n.auth.mode "none" }}
- { name: nats-nkey, mountPath: /etc/nexus/nats, readOnly: true }
{{- end }}
{{- if $n.tls.enabled }}
- { name: nats-tls, mountPath: /etc/nexus/nats-tls, readOnly: true }
{{- end }}
{{- end }}
{{- end -}}

{{/*
Volumes holding ONE role's NATS material. The seed is projected from the Secret
by key, so a pod receives its own seed and nothing else; no subPath, so a
rotated Secret is visible without recreating the pod (an NKey rotation still
needs a restart, docs/OPERATIONS.md §11.8). args: root, role, per-agent key override.
*/}}
{{- define "nexus.natsVolumes" -}}
{{- $root := index . 0 -}}{{- $role := index . 1 -}}{{- $keyOverride := index . 2 -}}
{{- $n := $root.Values.nats -}}
{{- if $n.enabled }}
{{- if ne $n.auth.mode "none" }}
{{- $key := ternary $n.auth.eventServiceSeedKey ($keyOverride | default $n.auth.agentSeedKey) (eq $role "event-service") }}
- name: nats-nkey
  secret:
    secretName: {{ $n.auth.seedSecret | default $root.Values.existingSecret }}
    defaultMode: 0400
    items:
      - { key: {{ $key }}, path: user.nk }
{{- end }}
{{- if $n.tls.enabled }}
- name: nats-tls
  secret:
    secretName: {{ ternary $n.tls.clientSecrets.eventService $n.tls.clientSecrets.agent (eq $role "event-service") | default $n.tls.caSecret }}
    defaultMode: 0400
    {{- if not $n.tls.verifyClients }}
    items:
      - { key: ca.crt, path: ca.crt }
    {{- end }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
Refuse to render a broker that would accept what it must not. Public NKeys are
validated here because a typo would otherwise surface as every client being
rejected at runtime, with nothing in the manifest pointing at why.
*/}}
{{- define "nexus.natsAuthRequired" -}}
{{- $n := .Values.nats -}}
{{- $a := $n.auth -}}
{{- if not (has $a.mode (list "nkey" "none")) -}}
{{- fail (printf "nats.auth.mode=%q: use nkey (production) or none (development only)" $a.mode) -}}
{{- end -}}
{{- if eq $a.mode "nkey" -}}
{{- $keyRe := "^U[A-Z2-7]{55}$" -}}
{{- range $role := list "eventService" "monitor" -}}
{{- $keys := (index $a $role).publicKeys | default list -}}
{{- if not $keys -}}
{{- fail (printf "nats.auth.%s.publicKeys is empty: generate identities with deploy/nats/nexus-nats-auth.sh init and paste `helm-values` (or set nats.auth.mode=none for a throwaway dev cluster)" $role) -}}
{{- end -}}
{{- range $keys }}{{ if not (regexMatch $keyRe .) }}{{ fail (printf "nats.auth.%s.publicKeys: %q is not a user public NKey (U...)" $role .) }}{{ end }}{{ end -}}
{{- end -}}
{{- $seen := dict -}}
{{- range $a.tenants -}}
{{- if not (regexMatch "^[A-Za-z0-9_-]+$" (.namespace | default "")) -}}
{{- fail (printf "nats.auth.tenants: namespace %q must match [A-Za-z0-9_-]+ (other names are percent-encoded on the bus; configure them by hand)" .namespace) -}}
{{- end -}}
{{- if not .publicKeys -}}
{{- fail (printf "nats.auth.tenants[%s].publicKeys is empty" .namespace) -}}
{{- end -}}
{{- range .publicKeys }}{{ if not (regexMatch $keyRe .) }}{{ fail (printf "nats.auth.tenants: %q is not a user public NKey (U...)" .) }}{{ end }}{{ end -}}
{{- $_ := set $seen .namespace true -}}
{{- end -}}
{{- if and .Values.agents (not (hasKey $seen .Values.nexusNamespace)) -}}
{{- fail (printf "nats.auth.tenants has no entry for nexusNamespace %q, so this chart's agents could not receive dispatch hints" .Values.nexusNamespace) -}}
{{- end -}}
{{- end -}}
{{- if $n.tls.enabled -}}
{{- if not $n.tls.secretName -}}
{{- fail "nats.tls.enabled needs nats.tls.secretName: a Secret with tls.crt, tls.key and ca.crt for the broker (cert-manager Certificate)" -}}
{{- end -}}
{{- if $n.tls.verifyClients -}}
{{- if not (and $n.tls.clientSecrets.eventService $n.tls.clientSecrets.agent) -}}
{{- fail "nats.tls.verifyClients needs nats.tls.clientSecrets.eventService and .agent (Secrets with tls.crt, tls.key, ca.crt)" -}}
{{- end -}}
{{- else if not (or $n.tls.caSecret (and $n.tls.clientSecrets.eventService $n.tls.clientSecrets.agent)) -}}
{{- fail "nats.tls.enabled needs nats.tls.caSecret (a Secret with ca.crt) so clients can verify the broker" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
wait-for-schema: the Kubernetes form of compose's
"depends_on: migrate: service_completed_successfully". A service must not start
against a schema older than the code it runs, so it waits until the NEWEST
migration shipped in the migrate image of the SAME tag is recorded -- which is
exactly "the migration job for this release has finished".
*/}}
{{- define "nexus.waitForSchema" -}}
- name: wait-for-schema
  image: {{ include "nexus.image" (list . "migrate") }}
  imagePullPolicy: {{ .Values.image.pullPolicy }}
  command: ["/bin/sh", "-c"]
  args:
    - |
      set -eu
      latest=$(ls /migrations/*.up.sql | sort | tail -n 1 | xargs basename | sed 's/\.up\.sql$//')
      echo "waiting for migration $latest"
      until [ "$(psql "$DATABASE_URL" -tAc "SELECT 1 FROM schema_migrations WHERE version = '$latest'" 2>/dev/null)" = "1" ]; do
        sleep 2
      done
      echo "schema at $latest"
  env:
{{ include "nexus.dbEnv" . | indent 4 }}
  securityContext:
{{ include "nexus.containerSecurity" . | indent 4 }}
  resources:
    requests: { cpu: 10m, memory: 16Mi }
    limits: { memory: 64Mi }
{{- end -}}

{{/* Non-root, no privilege escalation, no capabilities: the Nexus images run as uid 10001. */}}
{{- define "nexus.podSecurity" -}}
runAsNonRoot: true
runAsUser: 10001
runAsGroup: 10001
fsGroup: 10001
seccompProfile: { type: RuntimeDefault }
{{- end -}}

{{- define "nexus.containerSecurity" -}}
allowPrivilegeEscalation: false
capabilities: { drop: ["ALL"] }
{{- end -}}

{{/*
Suffix for the per-release Jobs: a hash of the WHOLE image tag. A Job's pod
template is immutable, so a new release needs a new Job name. A truncated tag
is not enough -- every dirty build of one commit shares its 12-character
prefix, and reusing that name makes the upgrade fail on an immutable field.
*/}}
{{- define "nexus.jobSuffix" -}}
{{- .Values.image.tag | sha256sum | trunc 10 -}}
{{- end -}}
