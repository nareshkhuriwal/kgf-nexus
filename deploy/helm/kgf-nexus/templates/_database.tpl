{{/*
Copyright 2026 Khuriwal Group
SPDX-License-Identifier: Apache-2.0

Database connections: TLS server verification (postgres.caBundle), per-
connection parameters (postgres.params, postgres.connections.<c>.params) and
least-privilege runtime logins (postgres.runtimeRoles). docs/OPERATIONS.md
§20.8 and §20.9.

Connection keys: apiGateway workflowManager dagEngine scheduler
resourceManager eventService artifactGC, and migrate (the migrate, bootstrap
and db-roles Jobs, always the owner login).
*/}}

{{/* The group role each runtime login joins; artifactGC is read-only. */}}
{{- define "nexus.dbGroupRoles" -}}
apiGateway: nexus_api_gateway
workflowManager: nexus_workflow_manager
dagEngine: nexus_dag_engine
scheduler: nexus_scheduler
resourceManager: nexus_resource_manager
eventService: nexus_event_service
artifactGC: ""
{{- end -}}

{{/* Connection keys that get a runtime login in this release. */}}
{{- define "nexus.dbRuntimeKeys" -}}
{{- $keys := list "apiGateway" "workflowManager" "dagEngine" "scheduler" "resourceManager" "eventService" -}}
{{- if .Values.artifactGC.enabled }}{{ $keys = append $keys "artifactGC" }}{{ end -}}
{{- toJson $keys -}}
{{- end -}}

{{- define "nexus.dbCAEnabled" -}}
{{- $ca := .Values.postgres.caBundle | default dict -}}
{{- if or $ca.secretName $ca.configMapName }}true{{ end -}}
{{- end -}}

{{/*
Refuse database settings that would connect less safely than they read, or
not at all. Called from nexus.required.
*/}}
{{- define "nexus.dbRequired" -}}
{{- $pg := .Values.postgres -}}
{{- $ca := $pg.caBundle | default dict -}}
{{- $verify := has $pg.sslmode (list "verify-ca" "verify-full") -}}
{{- if and $ca.secretName $ca.configMapName -}}
{{- fail "postgres.caBundle: set secretName OR configMapName, not both" -}}
{{- end -}}
{{- if and $verify (not (include "nexus.dbCAEnabled" .)) -}}
{{- fail (printf "postgres.sslmode=%s needs postgres.caBundle.secretName or .configMapName: the database's root CA bundle to verify the server against (docs/OPERATIONS.md §20.8)" $pg.sslmode) -}}
{{- end -}}
{{- if and (include "nexus.dbCAEnabled" .) (not $verify) -}}
{{- fail (printf "postgres.caBundle is set but postgres.sslmode=%s does not verify the server; use verify-full" $pg.sslmode) -}}
{{- end -}}
{{- if and $pg.enabled (not (has $pg.sslmode (list "disable" "allow" "prefer"))) -}}
{{- fail (printf "postgres.sslmode=%s: the in-cluster postgres (postgres.enabled=true) serves no TLS; use an external database for TLS, or sslmode=disable" $pg.sslmode) -}}
{{- end -}}
{{- $reserved := list "user" "password" "passfile" "host" "hostaddr" "port" "dbname" "sslmode" "sslrootcert" "sslcert" "sslkey" "sslpassword" "sslcrl" "sslcrldir" "sslsni" "service" "servicefile" -}}
{{- $all := dict "params" ($pg.params | default dict) -}}
{{- range $k, $c := ($pg.connections | default dict) }}{{ $_ := set $all (printf "connections.%s.params" $k) (($c | default dict).params | default dict) }}{{ end -}}
{{- range $where, $params := $all -}}
{{- range $k, $_ := $params -}}
{{- if has (lower $k) $reserved -}}
{{- fail (printf "postgres.%s.%s: this parameter has its own setting (postgres.user/host/port/database/sslmode/caBundle, runtimeRoles); it cannot be set as a free-form parameter" $where $k) -}}
{{- end -}}
{{- if not (regexMatch "^[a-z_]+$" $k) -}}
{{- fail (printf "postgres.%s: %q is not a libpq parameter name" $where $k) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if ($pg.runtimeRoles | default dict).enabled -}}
{{- $rr := $pg.runtimeRoles -}}
{{- $secret := $rr.existingSecret | default .Values.existingSecret -}}
{{- $seenUser := dict -}}{{- $seenKey := dict -}}
{{- range $key := (include "nexus.dbRuntimeKeys" . | fromJsonArray) -}}
{{- $c := index ($pg.connections | default dict) $key | default dict -}}
{{- if not (and $c.user $c.passwordKey) -}}
{{- fail (printf "postgres.runtimeRoles.enabled needs postgres.connections.%s.user and .passwordKey" $key) -}}
{{- end -}}
{{- if eq $c.user $pg.user -}}
{{- fail (printf "postgres.connections.%s.user is the owner login %q; a runtime login must be a separate role" $key $pg.user) -}}
{{- end -}}
{{- if hasKey $seenUser $c.user -}}
{{- fail (printf "postgres.connections: %s and %s both use login %q; each service needs its own" (index $seenUser $c.user) $key $c.user) -}}
{{- end -}}
{{- if hasKey $seenKey $c.passwordKey -}}
{{- fail (printf "postgres.connections: %s and %s share passwordKey %q" (index $seenKey $c.passwordKey) $key $c.passwordKey) -}}
{{- end -}}
{{- if and (eq $c.passwordKey "postgres-password") (eq $secret $.Values.existingSecret) -}}
{{- fail (printf "postgres.connections.%s.passwordKey is postgres-password, the OWNER's password; give each runtime login its own key" $key) -}}
{{- end -}}
{{- $_ := set $seenUser $c.user $key -}}{{- $_ := set $seenKey $c.passwordKey $key -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
The connection for one key, as YAML: user, secret, passwordKey, url. The URL
carries $(NEXUS_DB_PASSWORD), expanded by the kubelet from the Secret, so the
password is never rendered into a manifest. args: root, key.
*/}}
{{- define "nexus.dbConnection" -}}
{{- $root := index . 0 -}}{{- $key := index . 1 -}}
{{- $pg := $root.Values.postgres -}}
{{- $c := index ($pg.connections | default dict) $key | default dict -}}
{{- $rr := $pg.runtimeRoles | default dict -}}
{{- $user := $pg.user -}}
{{- $secret := $root.Values.existingSecret -}}
{{- $pwKey := "postgres-password" -}}
{{- if and $rr.enabled (ne $key "migrate") -}}
{{- $user = $c.user -}}
{{- $pwKey = $c.passwordKey -}}
{{- $secret = $rr.existingSecret | default $root.Values.existingSecret -}}
{{- end -}}
{{- $q := list (printf "sslmode=%s" $pg.sslmode) -}}
{{- if include "nexus.dbCAEnabled" $root -}}
{{- $q = append $q "sslrootcert=/etc/nexus/postgres-ca/ca.crt" -}}
{{- end -}}
{{- $params := merge (deepCopy ($c.params | default dict)) (deepCopy ($pg.params | default dict)) -}}
{{- range $k, $v := $params -}}
{{- $q = append $q (printf "%s=%s" $k (toString $v | urlquery)) -}}
{{- end -}}
user: {{ $user | quote }}
secret: {{ $secret | quote }}
passwordKey: {{ $pwKey | quote }}
url: {{ printf "postgres://%s:$(NEXUS_DB_PASSWORD)@%s:%v/%s?%s" $user $pg.host $pg.port $pg.database (join "&" $q) | quote }}
{{- end -}}

{{/*
DATABASE_URL for one connection key. The password is URL-safe by contract
(deploy/terraform generates alphanumerics; nexus-db-roles refuses anything
outside A-Z a-z 0-9 . _ ~ -). Required: the services have no built-in DSN and
exit at startup without it (internal/config.DSN). args: root, key.
*/}}
{{- define "nexus.dbEnv" -}}
{{- $c := include "nexus.dbConnection" . | fromYaml -}}
- name: NEXUS_DB_PASSWORD
  valueFrom:
    secretKeyRef: { name: {{ $c.secret }}, key: {{ $c.passwordKey }} }
- name: DATABASE_URL
  value: {{ $c.url | quote }}
{{- end -}}

{{- define "nexus.dbVolumeMounts" -}}
{{- if include "nexus.dbCAEnabled" . }}
- { name: postgres-ca, mountPath: /etc/nexus/postgres-ca, readOnly: true }
{{- end }}
{{- end -}}

{{/*
The CA bundle, projected to ca.crt whatever its key is called. No subPath, so
an updated Secret/ConfigMap reaches the file in place: libpq (the Jobs) reads
it per connection; the Go services read it when their pool is built, so a CA
ROTATION still needs a rollout restart (OPERATIONS §20.8).
*/}}
{{- define "nexus.dbVolumes" -}}
{{- $ca := .Values.postgres.caBundle | default dict -}}
{{- if include "nexus.dbCAEnabled" . }}
- name: postgres-ca
  {{- if $ca.secretName }}
  secret:
    secretName: {{ $ca.secretName }}
    defaultMode: 0444
    items: [{ key: {{ $ca.key | default "ca.crt" }}, path: ca.crt }]
  {{- else }}
  configMap:
    name: {{ $ca.configMapName }}
    items: [{ key: {{ $ca.key | default "ca.crt" }}, path: ca.crt }]
  {{- end }}
{{- end }}
{{- end -}}

{{/*
nexus-db-roles input: one entry per runtime login, naming the env var holding
its password (never the password). Rendered into the db-roles Job.
*/}}
{{- define "nexus.dbRolesSpec" -}}
{{- $groups := include "nexus.dbGroupRoles" . | fromYaml -}}
{{- $limit := -1 -}}
{{- $rrl := .Values.postgres.runtimeRoles.connectionLimit -}}
{{- if not (kindIs "invalid" $rrl) }}{{ $limit = int $rrl }}{{ end -}}
{{- $out := list -}}
{{- range $key := (include "nexus.dbRuntimeKeys" . | fromJsonArray) -}}
{{- $c := index $.Values.postgres.connections $key -}}
{{- $e := dict "login" $c.user "password_env" (printf "NEXUS_PW_%s" (snakecase $key | upper)) "connection_limit" $limit -}}
{{- $g := index $groups $key -}}
{{- if $g }}{{ $_ := set $e "member_of" (list $g) }}{{ else }}{{ $_ := set $e "read_only" true }}{{ end -}}
{{- $out = append $out $e -}}
{{- end -}}
{{- toJson $out -}}
{{- end -}}

{{/* Pod annotation that rolls every connecting pod when runtime passwords rotate. */}}
{{- define "nexus.dbRevisionAnnotation" -}}
{{- $rr := .Values.postgres.runtimeRoles | default dict -}}
{{- if $rr.enabled }}
nexus.khuriwalgroup.com/db-credentials-revision: {{ $rr.passwordRevision | toString | quote }}
{{- end }}
{{- end -}}
