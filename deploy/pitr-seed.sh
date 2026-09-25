#!/bin/sh

# Copyright 2026 Khuriwal Group
# SPDX-License-Identifier: Apache-2.0

# Seeds a realistic, sized, TAGGED dataset for the recovery drills, then exits 0.
#
# Usage:
#   deploy/pitr-seed.sh --batch A [--runs 2000] [--nodes 10] [--retry-pct 20]
#                       [--artifacts 1000] [--artifact-kib 1024] [--inflight 20]
#
# Writes, per batch, with every row findable by its batch tag:
#   * one workflow spec (pitr-drill-<batch>) and --runs workflow runs (params
#     carry {"drill_batch": "<batch>"}), each a --nodes chain with its edges;
#   * one SUCCEEDED attempt per node, plus a FAILED earlier attempt on
#     --retry-pct percent of nodes (retries are what make attempts outnumber
#     nodes in real history);
#   * run_events for every transition, in per-run seq order;
#   * --artifacts random blobs of --artifact-kib each, uploaded to the result
#     bucket content-addressed exactly as ADR-0008 lays them out
#     (<namespace>/<sha256>), and referenced from the attempts' output_ref in the
#     same shape the agents write ({"result":{"blob":{bucket,key,sha256}},...});
#   * --inflight runs left RUNNING, their current attempt held under a live lease
#     by a synthetic worker -- the in-flight state a restore brings back and fleet
#     reconciliation must clean up (worker declared DEAD, attempt LOST).
#
# Synthetic rows go through the same tables, constraints and tenant triggers as
# service writes, but NOT through the services: this is a data-volume generator
# for timing backup/restore, not a correctness test of the engine. Use a
# throwaway stack. It refuses to run against a database that already holds the
# same batch tag, so a drill can never double-count.
set -eu

PG_CONTAINER="${PG_CONTAINER:-nexus-postgres-1}"
MINIO_CONTAINER="${MINIO_CONTAINER:-nexus-minio-1}"
PG_USER="${PG_USER:-nexus}"
PG_DB="${PG_DB:-nexus}"
NAMESPACE="${NEXUS_NAMESPACE:-default}"
BUCKET="${RESULT_STORE_BUCKET:-${NEXUS_RESULT_BUCKET:-nexus-results}}"
ACCESS_KEY="${RESULT_STORE_ACCESS_KEY:-${NEXUS_S3_ACCESS_KEY_ID:-${MINIO_ROOT_USER:-nexusadmin}}}"
SECRET_KEY="${RESULT_STORE_SECRET_KEY:-${NEXUS_S3_SECRET_ACCESS_KEY:-${MINIO_ROOT_PASSWORD:-nexus-dev-password}}}"

BATCH=""
RUNS=2000
NODES=10
RETRY_PCT=20
ARTIFACTS=1000
ARTIFACT_KIB=1024
INFLIGHT=20
while [ "$#" -gt 0 ]; do
  case "$1" in
    --batch) BATCH="$2"; shift 2 ;;
    --runs) RUNS="$2"; shift 2 ;;
    --nodes) NODES="$2"; shift 2 ;;
    --retry-pct) RETRY_PCT="$2"; shift 2 ;;
    --artifacts) ARTIFACTS="$2"; shift 2 ;;
    --artifact-kib) ARTIFACT_KIB="$2"; shift 2 ;;
    --inflight) INFLIGHT="$2"; shift 2 ;;
    *) echo "pitr-seed: unknown argument $1" >&2; exit 2 ;;
  esac
done
case "$BATCH" in
  ''|*[!A-Za-z0-9_-]*) echo "pitr-seed: --batch <tag> is required ([A-Za-z0-9_-])" >&2; exit 2 ;;
esac
for n in "$RUNS" "$NODES" "$RETRY_PCT" "$ARTIFACTS" "$ARTIFACT_KIB" "$INFLIGHT"; do
  case "$n" in ''|*[!0-9]*) echo "pitr-seed: numeric arguments only" >&2; exit 2 ;; esac
done
[ "$ARTIFACTS" -ge 1 ] || { echo "pitr-seed: --artifacts must be >= 1" >&2; exit 2; }

psql_() { docker exec -i "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -v ON_ERROR_STOP=1 "$@"; }

existing=$(psql_ -tAc "SELECT count(*) FROM workflow_runs WHERE params->>'drill_batch' = '$BATCH'")
if [ "$existing" != "0" ]; then
  echo "pitr-seed: batch $BATCH already present ($existing runs); pick a new tag" >&2
  exit 1
fi

WORK="${TMPDIR:-/tmp}/nexus-pitr-seed-$$"
mkdir -p "$WORK/blobs/$NAMESPACE"
trap 'rm -rf "$WORK"; docker exec "$MINIO_CONTAINER" rm -rf "/tmp/nexus-seed-$$" >/dev/null 2>&1 || true' EXIT

sha() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }

# ── 1. Artifacts ──────────────────────────────────────────────────────────
# Random bytes, so compression cannot make the backup of them look cheaper than
# real model/data artifacts would be. Named by digest, uploaded in ONE mirror.
t0=$(date +%s)
echo "pitr-seed: [$BATCH] generating $ARTIFACTS artifacts of ${ARTIFACT_KIB} KiB"
: > "$WORK/blobs.tsv"
i=0
while [ "$i" -lt "$ARTIFACTS" ]; do
  f="$WORK/blob.tmp"
  dd if=/dev/urandom of="$f" bs=1024 count="$ARTIFACT_KIB" 2>/dev/null
  d=$(sha "$f")
  mv "$f" "$WORK/blobs/$NAMESPACE/$d"
  printf '%s\t%s\n' "$i" "$d" >> "$WORK/blobs.tsv"
  i=$((i + 1))
done
echo "pitr-seed: [$BATCH] uploading to $BUCKET via $MINIO_CONTAINER"
docker exec "$MINIO_CONTAINER" mkdir -p "/tmp/nexus-seed-$$"
docker cp "$WORK/blobs/." "$MINIO_CONTAINER:/tmp/nexus-seed-$$/" >/dev/null
docker exec "$MINIO_CONTAINER" mc alias set nexusseed http://localhost:9000 "$ACCESS_KEY" "$SECRET_KEY" >/dev/null
docker exec "$MINIO_CONTAINER" mc stat "nexusseed/$BUCKET" >/dev/null
docker exec "$MINIO_CONTAINER" mc mirror --quiet "/tmp/nexus-seed-$$" "nexusseed/$BUCKET" >/dev/null
docker exec "$MINIO_CONTAINER" rm -rf "/tmp/nexus-seed-$$"
rm -rf "$WORK/blobs"
t1=$(date +%s)

# ── 2. Rows ───────────────────────────────────────────────────────────────
# Set-based: generate_series + joins, one transaction. Row-at-a-time inserts
# would make seeding, not restoring, the slow part of the drill.
echo "pitr-seed: [$BATCH] inserting $RUNS runs x $NODES nodes (+$INFLIGHT in flight)"
{
  cat <<SQL
BEGIN;
CREATE TEMP TABLE seed_blobs (idx int PRIMARY KEY, sha text NOT NULL);
COPY seed_blobs FROM STDIN;
SQL
  cat "$WORK/blobs.tsv"
  cat <<SQL
\\.
INSERT INTO tenants (namespace, name) VALUES ('$NAMESPACE', '$NAMESPACE')
  ON CONFLICT (namespace) DO NOTHING;

INSERT INTO workflow_specs (namespace, name, version, spec_hash, spec_json, created_by)
VALUES ('$NAMESPACE', 'pitr-drill-$BATCH', 1, md5('pitr-drill-$BATCH'),
        jsonb_build_object('name', 'pitr-drill-$BATCH', 'drill', true, 'nodes', $NODES),
        'pitr-seed');

CREATE TEMP TABLE seed_runs AS
SELECT gen_random_uuid() AS id, g AS i,
       (g > $RUNS) AS inflight,
       now() - make_interval(secs => ($RUNS + $INFLIGHT - g) * 2) AS created
  FROM generate_series(1, $RUNS + $INFLIGHT) g;

INSERT INTO workflow_runs (id, spec_id, namespace, state, idempotency_key, trigger_kind,
                           params, created_at, started_at, ended_at)
SELECT r.id, s.id, '$NAMESPACE',
       CASE WHEN r.inflight THEN 'RUNNING'::run_state
            WHEN r.i % 50 = 0 THEN 'FAILED'::run_state ELSE 'SUCCEEDED'::run_state END,
       'pitr-$BATCH-' || r.i, 'api',
       jsonb_build_object('drill_batch', '$BATCH', 'i', r.i),
       r.created, r.created + interval '1 second',
       CASE WHEN r.inflight THEN NULL ELSE r.created + interval '90 seconds' END
  FROM seed_runs r
  JOIN workflow_specs s ON s.namespace = '$NAMESPACE' AND s.name = 'pitr-drill-$BATCH' AND s.version = 1;

CREATE TEMP TABLE seed_nodes AS
SELECT r.id AS run_id, r.i, r.inflight, r.created, n AS ord,
       'step_' || lpad(n::text, 3, '0') AS node_id,
       -- In-flight runs: nodes before the last are done, the last is RUNNING.
       (r.inflight AND n = $NODES) AS running,
       (NOT r.inflight AND ((r.i * 31 + n * 17) % 100) < $RETRY_PCT) AS retried
  FROM seed_runs r, generate_series(1, $NODES) n;

INSERT INTO task_nodes (run_id, node_id, task_ref, state, max_attempts, attempts_used, spec_json)
SELECT run_id, node_id, 'drill.step',
       CASE WHEN running THEN 'RUNNING'::node_state ELSE 'SUCCEEDED'::node_state END,
       -- max_attempts = attempts already used for in-flight nodes: when
       -- reconciliation declares the attempt LOST the node fails instead of
       -- retrying a task no real worker implements.
       CASE WHEN running THEN 1 ELSE 3 END,
       CASE WHEN retried THEN 2 ELSE 1 END,
       jsonb_build_object('task_ref', 'drill.step', 'ord', ord)
  FROM seed_nodes;

INSERT INTO task_edges (run_id, from_node_id, to_node_id)
SELECT run_id, 'step_' || lpad((ord - 1)::text, 3, '0'), node_id
  FROM seed_nodes WHERE ord > 1;

-- A synthetic worker per in-flight run group: ACTIVE, heartbeating "now", so the
-- restored snapshot contains a fleet that no longer exists.
CREATE TEMP TABLE seed_workers AS
SELECT gen_random_uuid() AS id, g AS w FROM generate_series(1, GREATEST(1, LEAST($INFLIGHT, 4))) g;
INSERT INTO workers (id, namespace, hostname, queues, executors, capacity, state,
                     agent_version, session_token_hash)
SELECT id, '$NAMESPACE', 'pitr-drill-$BATCH-worker-' || w, ARRAY['default'], ARRAY['subprocess'],
       '{"cpu_millis": 4000, "memory_bytes": 8589934592}'::jsonb, 'ACTIVE', 'pitr-seed',
       md5('pitr-drill-$BATCH-' || w)
  FROM seed_workers
 WHERE $INFLIGHT > 0;

CREATE TEMP TABLE seed_attempts AS
-- The earlier FAILED attempt of a retried node.
SELECT gen_random_uuid() AS id, n.run_id, n.node_id, 1 AS attempt_number, 'FAILED'::attempt_state AS state,
       n.i, n.ord, n.created, NULL::uuid AS worker_id
  FROM seed_nodes n WHERE n.retried
UNION ALL
-- The final attempt of every node.
SELECT gen_random_uuid(), n.run_id, n.node_id, CASE WHEN n.retried THEN 2 ELSE 1 END,
       CASE WHEN n.running THEN 'RUNNING'::attempt_state ELSE 'SUCCEEDED'::attempt_state END,
       n.i, n.ord, n.created,
       CASE WHEN n.running THEN (SELECT id FROM seed_workers ORDER BY w OFFSET (n.i % GREATEST(1, LEAST($INFLIGHT, 4))) LIMIT 1) END
  FROM seed_nodes n;

INSERT INTO task_attempts (id, run_id, node_id, attempt_number, state, queue, resource_request,
                           idempotency_key, worker_id, lease_id, output_ref, error, metrics,
                           scheduled_at, dispatched_at, started_at, ended_at)
SELECT a.id, a.run_id, a.node_id, a.attempt_number, a.state, 'default',
       '{"cpu_millis": 250, "memory_bytes": 268435456}'::jsonb,
       a.run_id::text || ':' || a.node_id,
       a.worker_id,
       CASE WHEN a.state = 'RUNNING' THEN gen_random_uuid() END,
       CASE WHEN a.state = 'SUCCEEDED' THEN jsonb_build_object('result', jsonb_build_object(
              'blob', jsonb_build_object('bucket', '$BUCKET',
                                         'key', '$NAMESPACE/' || b.sha, 'sha256', b.sha),
              'size_bytes', ($ARTIFACT_KIB * 1024)::text,
              'content_type', 'application/octet-stream')) END,
       CASE WHEN a.state = 'FAILED' THEN
         '{"kind": "TASK_ERROR", "message": "synthetic transient failure (pitr-seed)"}'::jsonb END,
       jsonb_build_object('duration_ms', 1000 + (a.i * 7 + a.ord * 13) % 9000),
       a.created + make_interval(secs => a.ord * 8),
       a.created + make_interval(secs => a.ord * 8 + 1),
       a.created + make_interval(secs => a.ord * 8 + 1),
       CASE WHEN a.state = 'RUNNING' THEN NULL ELSE a.created + make_interval(secs => a.ord * 8 + 7) END
  FROM seed_attempts a
  JOIN seed_blobs b ON b.idx = (abs(hashtext(a.run_id::text || a.node_id)) % $ARTIFACTS);

INSERT INTO leases (attempt_id, worker_id, token, expires_at)
SELECT id, worker_id, md5(id::text), now() + interval '30 seconds'
  FROM seed_attempts WHERE state = 'RUNNING';
INSERT INTO reservations (worker_id, attempt_id, resources)
SELECT worker_id, id, '{"cpu_millis": 250, "memory_bytes": 268435456}'::jsonb
  FROM seed_attempts WHERE state = 'RUNNING';

-- Events: created + started per run, then dispatched/started/finished per attempt,
-- then the run's terminal event. seq is dense per run, in time order.
INSERT INTO run_events (run_id, seq, node_id, attempt_id, kind, payload, actor, occurred_at)
SELECT run_id, row_number() OVER (PARTITION BY run_id ORDER BY at, tie), node_id, attempt_id,
       kind, payload, actor, at
  FROM (
    SELECT id AS run_id, NULL::text AS node_id, NULL::uuid AS attempt_id, 'RUN_CREATED' AS kind,
           '{}'::jsonb AS payload, 'pitr-seed' AS actor, created AS at, 0 AS tie FROM seed_runs
    UNION ALL
    SELECT a.run_id, a.node_id, a.id, k.kind, jsonb_build_object('attempt', a.attempt_number),
           'scheduler', a.created + make_interval(secs => a.ord * 8 + k.off), k.tie
      FROM seed_attempts a
      CROSS JOIN (VALUES ('ATTEMPT_DISPATCHED', 1, 1), ('ATTEMPT_STARTED', 1, 2),
                         ('ATTEMPT_FINISHED', 7, 3)) AS k(kind, off, tie)
     WHERE k.kind <> 'ATTEMPT_FINISHED' OR a.state <> 'RUNNING'
    UNION ALL
    SELECT r.id, NULL, NULL, CASE WHEN r.i % 50 = 0 THEN 'RUN_FAILED' ELSE 'RUN_SUCCEEDED' END,
           '{}'::jsonb, 'dag-engine', r.created + interval '90 seconds', 9 FROM seed_runs r
     WHERE NOT r.inflight
  ) ev;

-- The per-run event counter the services allocate seq from (migration 0002).
-- Without it the next service-written event collides with a seeded seq.
UPDATE workflow_runs r SET event_seq = e.max_seq
  FROM (SELECT run_id, max(seq) AS max_seq FROM run_events
         WHERE run_id IN (SELECT id FROM seed_runs) GROUP BY run_id) e
 WHERE r.id = e.run_id;

COMMIT;
SQL
} | psql_ -q >/dev/null
t2=$(date +%s)

psql_ -tA -F ' ' -c "
SELECT count(DISTINCT r.id), count(DISTINCT (n.run_id, n.node_id)), count(DISTINCT a.id)
  FROM workflow_runs r
  JOIN task_nodes n ON n.run_id = r.id
  JOIN task_attempts a ON a.run_id = r.id
 WHERE r.params->>'drill_batch' = '$BATCH'" | {
  read -r runs nodes attempts
  echo "pitr-seed: [$BATCH] done: runs=$runs nodes=$nodes attempts=$attempts" \
       "artifacts=$ARTIFACTS x ${ARTIFACT_KIB}KiB (upload $((t1 - t0))s, rows $((t2 - t1))s)"
  printf '{"batch":"%s","runs":%s,"nodes":%s,"attempts":%s,"artifacts":%s,"artifact_bytes":%s,"inflight_runs":%s,"upload_seconds":%s,"insert_seconds":%s}\n' \
    "$BATCH" "$runs" "$nodes" "$attempts" "$ARTIFACTS" "$((ARTIFACTS * ARTIFACT_KIB * 1024))" "$INFLIGHT" "$((t1 - t0))" "$((t2 - t1))"
}
