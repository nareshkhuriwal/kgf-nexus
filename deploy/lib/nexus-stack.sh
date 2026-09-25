# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0
#
# Shared stack operations for the recovery drills (restore-drill.sh --rto,
# pitr-drill.sh). SOURCED, not executed. Expects PG_CONTAINER, PG_USER, PG_DB,
# MINIO_CONTAINER and WORK to be set by the caller.
#
# Everything here is keyed off the compose project of PG_CONTAINER, read from its
# labels, so the same functions drive the default `nexus` stack and an isolated
# drill project (e.g. `nexus-pitr`) without a second configuration.
#
# SC2034: REFS_*, STORE_KEYS, SAMPLE_*, RECONCILE_* are outputs the sourcing
# script reads. SC2013: word-splitting a file of container names / object keys
# is intended; neither ever contains whitespace.
# shellcheck disable=SC2034,SC2013

# The control plane and fleet: what a recovery stops before touching the
# database and starts again afterwards. Infrastructure (postgres, minio, nats,
# the backup store) is deliberately NOT here.
NEXUS_APP_SERVICES="${NEXUS_APP_SERVICES:-api-gateway workflow-manager dag-engine scheduler scheduler-gpu resource-manager event-service agent-cpu agent-gpu agent-gpu-a10 ui}"
# Agents are the fleet whose re-registration "fleet reconciliation" waits for.
NEXUS_AGENT_SERVICES="${NEXUS_AGENT_SERVICES:-agent-cpu agent-gpu agent-gpu-a10}"

pg_q() {
  docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "${2:-$PG_DB}" -v ON_ERROR_STOP=1 -tAqc "$1"
}

stack_project() {
  docker inspect -f '{{ index .Config.Labels "com.docker.compose.project" }}' "$PG_CONTAINER"
}

# Containers of this project whose compose service is in the given list.
# $1 = space-separated service list, $2 = "running" or "all"
stack_containers() {
  _proj=$(stack_project)
  _flt="--filter label=com.docker.compose.project=$_proj"
  [ "${2:-running}" = running ] || _flt="$_flt -a"
  # shellcheck disable=SC2086
  docker ps $_flt --format '{{.Names}} {{.Label "com.docker.compose.service"}}' |
    while read -r _name _svc; do
      for _want in $1; do
        if [ "$_svc" = "$_want" ]; then echo "$_name"; fi
      done
    done
}

# Stop every running app container, remembering which, so exactly those restart.
stack_stop_app() {
  stack_containers "$NEXUS_APP_SERVICES" running > "$WORK/.stopped-app"
  if [ -s "$WORK/.stopped-app" ]; then
    # One `docker stop` with every name: the daemon stops them concurrently, which
    # is what an orchestrator does and what the RTO should measure.
    # shellcheck disable=SC2046
    docker stop -t "${NEXUS_STOP_TIMEOUT:-30}" $(cat "$WORK/.stopped-app") >/dev/null
  fi
  echo "stack: stopped $(wc -l < "$WORK/.stopped-app" | tr -d ' ') app container(s)"
}

stack_start_app() {
  [ -s "$WORK/.stopped-app" ] || { echo "stack: no app containers to start"; return 0; }
  # Retried, per container: Docker Desktop releases a published host port a few
  # seconds after the container that held it stops, and a start inside that
  # window fails with "address already in use". An orchestrator retries; so do we.
  # A start that fails that way can also leave the container DETACHED from its
  # network (observed on Docker Desktop 29), after which every restart dies on
  # DNS. So before each attempt, a container with no network is reattached to
  # the project network under its compose service name.
  _net=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$PG_CONTAINER" | awk '{print $1}')
  _try=1
  while :; do
    _left=""
    for _c in $(cat "$WORK/.stopped-app"); do
      [ "$(docker inspect -f '{{.State.Running}}' "$_c")" = true ] && continue
      if [ -z "$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{end}}' "$_c")" ]; then
        _svc=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.service" }}' "$_c")
        echo "stack: $_c lost its network; reattaching to $_net as $_svc"
        docker network connect --alias "$_svc" "$_net" "$_c" || true
      fi
      docker start "$_c" >/dev/null 2>"$WORK/.start-err" || _left="$_left $_c"
    done
    [ -z "$_left" ] && return 0
    if [ "$_try" -ge 30 ]; then
      echo "stack: could not start:$_left" >&2
      cat "$WORK/.start-err" >&2
      return 1
    fi
    _try=$((_try + 1))
    sleep 1
  done
}

# Wait until every restarted container is healthy (or running, if it has no
# healthcheck). Fails loudly on a container that exits.
stack_wait_healthy() {
  _deadline=$(( $(date +%s) + ${1:-300} ))
  [ -s "$WORK/.stopped-app" ] || return 0
  while :; do
    _pending=""
    for _c in $(cat "$WORK/.stopped-app"); do
      _st=$(docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$_c")
      case "$_st" in
        "running healthy"|"running none") ;;
        exited*|dead*) echo "stack: $_c is $_st" >&2; docker logs --tail 20 "$_c" >&2; return 1 ;;
        *) _pending="$_pending $_c($_st)" ;;
      esac
    done
    [ -z "$_pending" ] && return 0
    if [ "$(date +%s)" -ge "$_deadline" ]; then
      echo "stack: not healthy in time:$_pending" >&2
      return 1
    fi
    sleep 1
  done
}

# Fleet reconciliation after a restore, as three conditions read from Postgres.
# "Before the recovery began" is the dividing line: every worker row that has not
# heartbeated since then came out of the restored snapshot and describes a fleet
# that no longer exists (an agent that restarts registers a NEW worker).
#
#   stale     snapshot workers still ACTIVE/DRAINING/QUARANTINED. The scheduler's
#             liveness sweep must declare each DEAD -- which it only does once the
#             heartbeat is older than its liveness window (default 30s), so this
#             term is where most of the reconciliation time goes.
#   orphaned  attempts DISPATCHED/RUNNING not held by a worker that heartbeated
#             since the recovery began. They must be expired to LOST (then
#             retried, or failed at max_attempts).
#   fresh     workers that heartbeated since the recovery began: the real fleet
#             re-registered. Required only when agents were running before.
#
# Reconciled = stale = 0 AND orphaned = 0 AND fresh >= the number of agent
# containers restarted. Prints the counts on every change so a stall is visible.
# $1 = timeout seconds, $2 = epoch seconds the recovery began
stack_wait_reconciled() {
  _deadline=$(( $(date +%s) + ${1:-600} ))
  _since="$2"
  _need=0
  if [ -s "$WORK/.stopped-app" ]; then
    for _c in $(cat "$WORK/.stopped-app"); do
      _svc=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.service" }}' "$_c")
      for _a in $NEXUS_AGENT_SERVICES; do if [ "$_svc" = "$_a" ]; then _need=$((_need + 1)); fi; done
    done
  fi
  _last=""
  while :; do
    _row=$(pg_q "
SELECT (SELECT count(*) FROM workers
         WHERE state IN ('ACTIVE','DRAINING','QUARANTINED')
           AND last_heartbeat_at < to_timestamp($_since))
    || ' ' ||
       (SELECT count(*) FROM task_attempts a
         WHERE a.state IN ('DISPATCHED','RUNNING')
           AND NOT EXISTS (SELECT 1 FROM workers w
                            WHERE w.id = a.worker_id AND w.state = 'ACTIVE'
                              AND w.last_heartbeat_at >= to_timestamp($_since)))
    || ' ' ||
       (SELECT count(*) FROM workers
         WHERE state = 'ACTIVE' AND last_heartbeat_at >= to_timestamp($_since))" 2>/dev/null || echo "? ? ?")
    # shellcheck disable=SC2086
    set -- $_row
    if [ "$_row" != "$_last" ]; then
      echo "stack: reconcile stale_workers=$1 orphaned_attempts=$2 fresh_workers=$3 (need $_need)"
      _last="$_row"
    fi
    if [ "$1" = 0 ] && [ "$2" = 0 ] && [ "$3" != "?" ] && [ "$3" -ge "$_need" ]; then
      RECONCILE_FRESH_WORKERS="$3"
      return 0
    fi
    if [ "$(date +%s)" -ge "$_deadline" ]; then
      echo "stack: fleet did not reconcile in time" >&2
      return 1
    fi
    sleep 1
  done
}

# Snapshot of the fleet-relevant state, for the report: what reconciliation had
# to clean up.
stack_inflight_json() {
  pg_q "SELECT json_build_object(
    'workers_live_state', (SELECT count(*) FROM workers WHERE state IN ('ACTIVE','DRAINING','QUARANTINED')),
    'attempts_in_flight', (SELECT count(*) FROM task_attempts WHERE state IN ('DISPATCHED','RUNNING')),
    'leases', (SELECT count(*) FROM leases),
    'reservations', (SELECT count(*) FROM reservations))"
}

# Every result-store reference in task_attempts must name an object that exists.
# Same traversal as internal/artifactgc (jsonb_path_query '$.**.blob'), so nested
# and multi-output refs count. Compares the whole key set in one listing rather
# than one stat per key -- tens of thousands of stats would dominate the drill.
# $1 = bucket. Sets REFS_TOTAL and REFS_MISSING; writes missing keys to
# $WORK/missing-refs.
stack_check_refs() {
  _bucket="$1"
  pg_q "SELECT DISTINCT jsonb_path_query(output_ref, 'strict \$.**.blob') ->> 'key'
          FROM task_attempts
         WHERE output_ref IS NOT NULL
           AND jsonb_path_exists(output_ref, 'strict \$.**.blob.key')" |
    sed '/^$/d' | LC_ALL=C sort -u > "$WORK/db-keys"
  docker exec -i "$MINIO_CONTAINER" mc find "${MC_ALIAS:-nexusdrill}/$_bucket" |
    sed "s|^${MC_ALIAS:-nexusdrill}/$_bucket/||" | LC_ALL=C sort -u > "$WORK/store-keys"
  LC_ALL=C comm -23 "$WORK/db-keys" "$WORK/store-keys" > "$WORK/missing-refs"
  REFS_TOTAL=$(wc -l < "$WORK/db-keys" | tr -d ' ')
  REFS_MISSING=$(wc -l < "$WORK/missing-refs" | tr -d ' ')
  STORE_KEYS=$(wc -l < "$WORK/store-keys" | tr -d ' ')
}

# Re-hash a sample of referenced objects and compare to the digest in their key
# (content addressing, ADR-0008: the key's last path element IS the sha256).
# $1 = bucket, $2 = sample size. Sets SAMPLE_OK and SAMPLE_BAD.
stack_verify_sample() {
  SAMPLE_OK=0
  SAMPLE_BAD=0
  for _k in $(awk -v n="$2" 'NR % 97 == 1 && c < n { print; c++ }' "$WORK/db-keys"); do
    _want="${_k##*/}"
    _got=$(docker exec -i "$MINIO_CONTAINER" mc cat "${MC_ALIAS:-nexusdrill}/$1/$_k" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -d' ' -f1)
    if [ "$_got" = "$_want" ]; then SAMPLE_OK=$((SAMPLE_OK + 1)); else SAMPLE_BAD=$((SAMPLE_BAD + 1)); echo "  digest mismatch: $_k" >&2; fi
  done
}
