#!/usr/bin/env bash
# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Database TLS drill: prove sslmode=verify-full end to end against a real
# Postgres that serves a certificate from a private CA, with the SAME images
# the chart runs (docs/OPERATIONS.md §20.11).
#
#   TAG=<image tag> deploy/scripts/db-tls-drill.sh
#
# Positive: the migrate Job's psql (libpq), the db-roles Job (pgx) and every Go
# control-plane service connect with verify-full + sslrootcert, the services as
# their own least-privilege logins.
# Negative (each must be REFUSED): a CA that did not sign the server
# certificate, a host name the certificate does not name, no TLS at all
# (sslmode=disable, refused by the server's hostssl-only pg_hba), and the old
# sslmode=require-without-CA is shown to accept the rogue-CA server, which is
# exactly the gap verify-full closes.
#
# Environment:
#   TAG       image tag of kgf-nexus/* (required)
#   PREFIX    container/network name prefix (default nexus-dbtls-drill)
#   PG_PORT   host port for the TLS Postgres (default 55491)
#   KEEP=1    leave the containers running for inspection
set -euo pipefail

: "${TAG:?TAG must name the kgf-nexus image tag to test}"
prefix=${PREFIX:-nexus-dbtls-drill}
pg_port=${PG_PORT:-55491}
net="${prefix}-net"
pg="${prefix}-pg"
work=$(mktemp -d)
pass=0
fail=0

cleanup() {
  if [ "${KEEP:-0}" != 1 ]; then
    ids=$(docker ps -aq --filter "name=^${prefix}-")
    if [ -n "$ids" ]; then docker rm -f $ids >/dev/null 2>&1 || true; fi
    docker network rm "$net" >/dev/null 2>&1 || true
    rm -rf "$work"
  else
    echo "KEEP=1: containers ${prefix}-* and $work left in place"
  fi
}
trap cleanup EXIT

ok()  { echo "PASS  $*"; pass=$((pass + 1)); }
bad() { echo "FAIL  $*"; fail=$((fail + 1)); }

# ── certificates ─────────────────────────────────────────────────────────
# A CA, a server certificate it signs for the names clients will use, and a
# second (rogue) CA that signs an equally valid-looking certificate for the
# same names. Keys are EC P-256; nothing here is reused outside the drill.
gen_ca() { # name
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
    -subj "/CN=$1" -keyout "$work/$1.key" -out "$work/$1.crt" \
    -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
}
gen_ca drill-ca
gen_ca rogue-ca
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=$pg" \
  -keyout "$work/server.key" -out "$work/server.csr" 2>/dev/null
cat >"$work/san.ext" <<EOF
subjectAltName=DNS:$pg,DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth
keyUsage=critical,digitalSignature
EOF
openssl x509 -req -in "$work/server.csr" -CA "$work/drill-ca.crt" -CAkey "$work/drill-ca.key" \
  -CAcreateserial -days 2 -extfile "$work/san.ext" -out "$work/server.crt" 2>/dev/null
chmod 0644 "$work"/*.crt "$work"/*.key

# ── TLS-only Postgres ────────────────────────────────────────────────────
docker network create "$net" >/dev/null
cat >"$work/pg_hba.conf" <<'EOF'
local   all  all                trust
hostssl all  all  0.0.0.0/0     scram-sha-256
hostssl all  all  ::/0          scram-sha-256
EOF
owner_pw=DrillOwner$(openssl rand -hex 12)
docker run -d --name "$pg" --network "$net" --network-alias wrong-name.example \
  -p "127.0.0.1:${pg_port}:5432" \
  -e POSTGRES_USER=nexus -e POSTGRES_DB=nexus -e POSTGRES_PASSWORD="$owner_pw" \
  -e POSTGRES_INITDB_ARGS=--auth-host=scram-sha-256 \
  -v "$work:/drill:ro" --entrypoint sh postgres:16-alpine -c '
    install -o postgres -g postgres -m 0600 /drill/server.key /var/lib/postgresql/server.key
    install -o postgres -g postgres -m 0644 /drill/server.crt /var/lib/postgresql/server.crt
    install -o postgres -g postgres -m 0644 /drill/pg_hba.conf /var/lib/postgresql/pg_hba.conf
    exec docker-entrypoint.sh postgres -c ssl=on \
      -c ssl_cert_file=/var/lib/postgresql/server.crt -c ssl_key_file=/var/lib/postgresql/server.key \
      -c ssl_min_protocol_version=TLSv1.2 -c hba_file=/var/lib/postgresql/pg_hba.conf' >/dev/null
for _ in $(seq 60); do
  docker exec "$pg" pg_isready -U nexus -h 127.0.0.1 >/dev/null 2>&1 && break
  sleep 1
done
sleep 2

ca_mount=(-v "$work/drill-ca.crt:/etc/nexus/postgres-ca/ca.crt:ro")
rogue_mount=(-v "$work/rogue-ca.crt:/etc/nexus/postgres-ca/ca.crt:ro")
dsn() { # user password host [sslmode]
  echo "postgres://$1:$2@$3:5432/nexus?sslmode=${4:-verify-full}&sslrootcert=/etc/nexus/postgres-ca/ca.crt"
}
psql_as() { # ca|rogue dsn sql
  local ca=drill-ca
  [ "$1" = rogue ] && ca=rogue-ca
  docker run --rm --network "$net" -v "$work/$ca.crt:/etc/nexus/postgres-ca/ca.crt:ro" \
    --entrypoint psql "kgf-nexus/migrate:$TAG" "$2" -tAc "$3" 2>&1
}

# ── libpq: the migrate Job ───────────────────────────────────────────────
if out=$(docker run --rm --network "$net" "${ca_mount[@]}" -e DATABASE_URL="$(dsn nexus "$owner_pw" "$pg")" \
    "kgf-nexus/migrate:$TAG" 2>&1); then
  ok "migrate (psql/libpq) applied the schema over verify-full: $(echo "$out" | tail -n1)"
else
  bad "migrate over verify-full: $out"
fi
got=$(psql_as ca "$(dsn nexus "$owner_pw" "$pg")" "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
[ "$got" = t ] && ok "libpq session is encrypted (pg_stat_ssl.ssl = t)" || bad "pg_stat_ssl: $got"

out=$(psql_as rogue "$(dsn nexus "$owner_pw" "$pg")" "SELECT 1") \
  && bad "libpq accepted a server certificate from the wrong CA" \
  || { echo "$out" | grep -q "certificate verify failed" && ok "libpq refused the wrong CA: $(echo "$out" | grep -o 'certificate verify failed.*' | head -n1)" || bad "wrong CA refused for another reason: $out"; }
out=$(psql_as ca "$(dsn nexus "$owner_pw" wrong-name.example)" "SELECT 1") \
  && bad "libpq accepted a host name the certificate does not name" \
  || { echo "$out" | grep -qi "does not match host name" && ok "libpq refused the wrong host name" || bad "wrong host refused for another reason: $out"; }
out=$(psql_as ca "postgres://nexus:${owner_pw}@${pg}:5432/nexus?sslmode=disable" "SELECT 1") \
  && bad "the server accepted a plaintext connection" \
  || { echo "$out" | grep -q "no encryption" && ok "the server refused plaintext (hostssl only)" || bad "plaintext refused for another reason: $out"; }
# libpq quirk: sslmode=require WITH an sslrootcert file present verifies the
# CA (it behaves as verify-ca). Without one it verifies nothing; that is the
# F115 status quo the chart shipped before, shown here for contrast.
got=$(docker run --rm --network "$net" --entrypoint psql "kgf-nexus/migrate:$TAG" \
  "postgres://nexus:${owner_pw}@${pg}:5432/nexus?sslmode=require" -tAc "SELECT 1" 2>&1)
[ "$got" = 1 ] && ok "(contrast) sslmode=require with no CA connects without verifying the server at all" \
  || bad "(contrast) sslmode=require: $got"

# ── pgx: the db-roles Job creates the runtime logins ─────────────────────
# Written for bash 3.2 (macOS): no associative arrays.
services="api-gateway workflow-manager dag-engine scheduler resource-manager event-service"
group_of() { echo "nexus_$(echo "$1" | tr - _)"; }
login_of() { echo "$(group_of "$1")_login"; }
pw_of() { cat "$work/pw.$1"; }
spec="["
envs=()
for svc in $services; do
  echo "Drill$(openssl rand -hex 16)" >"$work/pw.$svc"
  var="PW_$(echo "$svc" | tr 'a-z-' 'A-Z_')"
  spec+="{\"login\":\"$(login_of "$svc")\",\"member_of\":[\"$(group_of "$svc")\"],\"password_env\":\"$var\"},"
  envs+=(-e "$var=$(pw_of "$svc")")
done
spec="${spec%,}]"
if out=$(docker run --rm --network "$net" "${ca_mount[@]}" "${envs[@]}" \
    -e DATABASE_URL="$(dsn nexus "$owner_pw" "$pg")" -e NEXUS_DB_ROLES="$spec" \
    --entrypoint nexus-db-roles "kgf-nexus/migrate:$TAG" 2>&1); then
  ok "db-roles (pgx) created $(echo "$out" | grep -c 'runtime login in place') logins over verify-full"
else
  bad "db-roles: $out"
fi
out=$(docker run --rm --network "$net" "${rogue_mount[@]}" "${envs[@]}" \
    -e DATABASE_URL="$(dsn nexus "$owner_pw" "$pg")" -e NEXUS_DB_ROLES="$spec" \
    --entrypoint nexus-db-roles "kgf-nexus/migrate:$TAG" 2>&1) \
  && bad "db-roles (pgx) accepted the wrong CA" \
  || { echo "$out" | grep -q "x509\|certificate" && ok "db-roles (pgx) refused the wrong CA" || bad "db-roles wrong CA: $out"; }

# A runtime login may not change the schema.
got=$(psql_as ca "$(dsn nexus_scheduler_login "$(pw_of scheduler)" "$pg")" "CREATE TABLE drill_x (x int)" || true)
echo "$got" | grep -q "permission denied" && ok "runtime login refused DDL: $(echo "$got" | head -n1)" || bad "runtime login DDL: $got"

# ── Go services, each as its own login ───────────────────────────────────
svc_env() { # service
  local s=$1
  echo "-e DATABASE_URL=$(dsn "$(login_of "$s")" "$(pw_of "$s")" "$pg")"
}
common=(-e LOG_FORMAT=json -e NEXUS_NAMESPACE=default
  -e HTTP_ADDR=:8080 -e NEXUS_HTTP_ADDR=:8080 -e NEXUS_REST_ADDR=:8080 -e NEXUS_WFM_HEALTH_ADDR=:8080 -e RM_HTTP_ADDR=:8080
  -e GRPC_ADDR=:9090 -e NEXUS_GRPC_ADDR=:9090 -e NEXUS_WFM_GRPC_ADDR=:9090 -e RM_GRPC_ADDR=:9090
  -e WORKFLOW_MANAGER_ADDR=${prefix}-workflow-manager:9090 -e NEXUS_WORKFLOW_MANAGER_ADDR=${prefix}-workflow-manager:9090
  -e DAG_ENGINE_ADDR=${prefix}-dag-engine:9090 -e NEXUS_DAG_ENGINE_ADDR=${prefix}-dag-engine:9090
  -e SCHEDULER_ADDR=${prefix}-scheduler:9090 -e NEXUS_SCHEDULER_ADDR=${prefix}-scheduler:9090
  -e RESOURCE_MANAGER_ADDR=${prefix}-resource-manager:9090 -e NEXUS_RESOURCE_MANAGER_ADDR=${prefix}-resource-manager:9090
  -e EVENT_SERVICE_ADDR=${prefix}-event-service:9090 -e NEXUS_EVENT_SERVICE_ADDR=${prefix}-event-service:9090
  -e NEXUS_QUEUE=default -e NEXUS_NATS_REQUIRE_AUTH=0 -e AUTH_MODE=api_key
  -e RESULT_STORE_ENDPOINT=http://unused:9000 -e RESULT_STORE_BUCKET=nexus-results
  -e RESULT_STORE_ACCESS_KEY=unused -e RESULT_STORE_SECRET_KEY=unused-unused)
for svc in workflow-manager dag-engine scheduler resource-manager event-service api-gateway; do
  # shellcheck disable=SC2046
  docker run -d --name "${prefix}-${svc}" --network "$net" "${ca_mount[@]}" "${common[@]}" $(svc_env "$svc") \
    "kgf-nexus/${svc}:$TAG" >/dev/null
done
check_connected() { # service -> 0 when its pool is open (a session from its login exists)
  local s=$1 n
  n=$(docker exec "$pg" psql -U nexus -d nexus -tAc \
    "SELECT count(*) FROM pg_stat_activity a JOIN pg_stat_ssl s USING (pid) WHERE a.usename = '$(login_of "$s")' AND s.ssl")
  [ "${n:-0}" -gt 0 ]
}
for svc in workflow-manager dag-engine scheduler resource-manager event-service api-gateway; do
  connected=0
  for _ in $(seq 30); do
    if check_connected "$svc"; then connected=1; break; fi
    sleep 1
  done
  if [ "$connected" = 1 ]; then
    ok "$svc connected over verify-full as $(login_of "$svc") (TLS session in pg_stat_ssl)"
  else
    bad "$svc did not connect: $(docker logs --tail 5 "${prefix}-${svc}" 2>&1 | tr '\n' ' ')"
  fi
done

# The same service pointed at the wrong CA, or at a host name its certificate
# does not carry, must never get a session: every connect attempt fails
# certificate verification and the service is never ready (it either exits
# or reports /readyz != 200, so Kubernetes keeps it out of its Service).
refused_check() { # container what
  local c=$1 what=$2 logs running ready
  sleep 10
  logs=$(docker logs "$c" 2>&1 | tail -n 20)
  running=$(docker inspect -f '{{.State.Running}}' "$c")
  ready=down
  if [ "$running" = true ] && docker exec "$c" wget -q -O /dev/null http://127.0.0.1:8080/readyz 2>/dev/null; then
    ready=up
  fi
  if echo "$logs" | grep -q "x509" && [ "$ready" = down ]; then
    ok "event-service with $what: refused ($(echo "$logs" | grep -o 'x509: [^"]*' | head -n1)); running=$running, not ready"
  else
    bad "event-service with $what: ready=$ready running=$running logs=$logs"
  fi
}
docker run -d --name "${prefix}-es-rogue" --network "$net" "${rogue_mount[@]}" "${common[@]}" \
  -e DATABASE_URL="$(dsn "$(login_of event-service)" "$(pw_of event-service)" "$pg")" "kgf-nexus/event-service:$TAG" >/dev/null
refused_check "${prefix}-es-rogue" "the wrong CA"
docker run -d --name "${prefix}-es-host" --network "$net" "${ca_mount[@]}" "${common[@]}" \
  -e DATABASE_URL="$(dsn "$(login_of event-service)" "$(pw_of event-service)" wrong-name.example)" "kgf-nexus/event-service:$TAG" >/dev/null
refused_check "${prefix}-es-host" "a host name the certificate lacks"
# And the positive one is ready.
if docker exec "${prefix}-event-service" wget -q -O /dev/null http://127.0.0.1:8080/readyz; then
  ok "event-service over verify-full answers /readyz 200"
else
  bad "event-service over verify-full is not ready"
fi

echo
echo "db-tls-drill: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
