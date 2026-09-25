{{/*
Copyright 2026 Khuriwal Group
SPDX-License-Identifier: Apache-2.0
*/}}

{{- define "nexus.required" -}}
{{- if and .Values.postgres.pitr.enabled (not .Values.postgres.enabled) -}}
{{- fail "postgres.pitr.enabled needs the in-cluster postgres (postgres.enabled=true). A managed database (Azure Flexible Server, RDS, Cloud SQL) must use the provider's automated backups and point-in-time restore instead -- see docs/OPERATIONS.md §4." -}}
{{- end -}}
{{- if and .Values.postgres.pitr.enabled (or (not .Values.postgres.pitr.existingSecret) (not .Values.postgres.pitr.repo.s3.bucket) (not .Values.postgres.pitr.repo.s3.endpoint)) -}}
{{- fail "postgres.pitr.enabled requires postgres.pitr.existingSecret (s3-key, s3-key-secret, cipher-pass), postgres.pitr.repo.s3.bucket and postgres.pitr.repo.s3.endpoint" -}}
{{- end -}}
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

{{- define "nexus.natsEnv" -}}
{{- if .Values.nats.enabled }}
- { name: NEXUS_NATS_URL, value: "nats://nats:4222" }
{{- end }}
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

{{/*
pgBackRest settings for the postgres container (archive-push/archive-get) and
the pgbackrest sidecar (stanza, check, base backups). Both MUST see the same
repository and passphrase, so they share this one definition. Same variables as
deploy/docker-compose.pitr.yml.
*/}}
{{- define "nexus.pgbackrestEnv" -}}
{{- $p := .Values.postgres.pitr -}}
- { name: PGBACKREST_STANZA, value: {{ $p.stanza | quote }} }
- { name: PGBACKREST_PG1_PATH, value: /var/lib/postgresql/data/pgdata }
- { name: PGBACKREST_PG1_USER, value: {{ .Values.postgres.user | quote }} }
- { name: PGBACKREST_PG1_SOCKET_PATH, value: /var/run/postgresql }
- { name: PGBACKREST_REPO1_TYPE, value: s3 }
- { name: PGBACKREST_REPO1_PATH, value: {{ $p.repo.path | quote }} }
- { name: PGBACKREST_REPO1_S3_BUCKET, value: {{ $p.repo.s3.bucket | quote }} }
- { name: PGBACKREST_REPO1_S3_ENDPOINT, value: {{ $p.repo.s3.endpoint | quote }} }
- { name: PGBACKREST_REPO1_S3_REGION, value: {{ $p.repo.s3.region | quote }} }
- { name: PGBACKREST_REPO1_STORAGE_PORT, value: {{ $p.repo.s3.port | quote }} }
- { name: PGBACKREST_REPO1_S3_URI_STYLE, value: {{ $p.repo.s3.uriStyle | quote }} }
- name: PGBACKREST_REPO1_S3_KEY
  valueFrom: { secretKeyRef: { name: {{ $p.existingSecret }}, key: s3-key } }
- name: PGBACKREST_REPO1_S3_KEY_SECRET
  valueFrom: { secretKeyRef: { name: {{ $p.existingSecret }}, key: s3-key-secret } }
{{- if $p.repo.s3.caSecret }}
- { name: PGBACKREST_REPO1_STORAGE_CA_FILE, value: /etc/pgbackrest/ca/ca.crt }
{{- end }}
- { name: PGBACKREST_REPO1_CIPHER_TYPE, value: {{ $p.cipherType | quote }} }
{{- if ne $p.cipherType "none" }}
- name: PGBACKREST_REPO1_CIPHER_PASS
  valueFrom: { secretKeyRef: { name: {{ $p.existingSecret }}, key: cipher-pass } }
{{- end }}
- { name: PGBACKREST_REPO1_RETENTION_FULL, value: {{ $p.retention.full | quote }} }
- { name: PGBACKREST_REPO1_RETENTION_FULL_TYPE, value: {{ $p.retention.fullType | quote }} }
- { name: PGBACKREST_REPO1_RETENTION_DIFF, value: {{ $p.retention.diff | quote }} }
- { name: PGBACKREST_REPO1_BUNDLE, value: "y" }
- { name: PGBACKREST_REPO1_BLOCK, value: "y" }
- { name: PGBACKREST_COMPRESS_TYPE, value: zst }
- { name: PGBACKREST_PROCESS_MAX, value: {{ $p.processMax | quote }} }
- { name: PGBACKREST_START_FAST, value: "y" }
- { name: PGBACKREST_DELTA, value: "y" }
- { name: PGBACKREST_ARCHIVE_ASYNC, value: "y" }
- { name: PGBACKREST_SPOOL_PATH, value: /var/spool/pgbackrest }
- { name: PGBACKREST_LOG_LEVEL_CONSOLE, value: info }
- { name: PGBACKREST_LOG_LEVEL_FILE, value: "off" }
{{- with $p.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end -}}
