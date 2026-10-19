// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Attempt placement (migration 0042, ADR-0037): where each attempt's workload
// runs, as the agent reports it through WorkerService.ReportPlacement, and the
// live task-runner listing built on it. attempt_runtime is the gateway's
// table, like attempt_logs: it is written here, on the report path, and read
// by the REST listing.

// AttemptPlacement is one attempt's reported placement. Every field may be
// empty ("not known").
type AttemptPlacement struct {
	Executor     string
	Cluster      string
	K8sNamespace string
	Pod          string
	K8sNode      string
}

// ErrInvalidPlacement means a report the gateway refuses to store: a field
// over its column's bound or an executor name outside the column's grammar.
// A protocol outcome, not a transport error: resending cannot fix it.
var ErrInvalidPlacement = errors.New("invalid_placement")

// executorRE is attempt_runtime.executor's CHECK.
var executorRE = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)

// Validate checks p against the table's CHECKs, so a bad report is refused
// with a reason instead of failing the INSERT.
func (p AttemptPlacement) Validate() error {
	switch {
	case !executorRE.MatchString(p.Executor):
		return fmt.Errorf("%w: executor %q", ErrInvalidPlacement, p.Executor)
	case len(p.Cluster) > 253, len(p.Pod) > 253, len(p.K8sNode) > 253:
		return fmt.Errorf("%w: a cluster, pod or node name is longer than 253 bytes", ErrInvalidPlacement)
	case len(p.K8sNamespace) > 63:
		return fmt.Errorf("%w: a Kubernetes namespace is longer than 63 bytes", ErrInvalidPlacement)
	}
	return nil
}

// GatewayRecordAttemptPlacement stores p as the placement of lease's attempt
// and reports whether it was stored.
//
// FENCED IN THE STATEMENT. The caller has already checked the lease token
// (GatewayVerifyLease, constant-time); this write is conditioned on the SAME
// lease row -- its id, still unexpired by the database clock -- so a lease
// reclaimed between the check and the write cannot be written through. And a
// stored row from a newer lease epoch is never overwritten by an older one's
// report. Either case returns false: the caller answers stale_lease_token.
//
// No lock is taken on task_attempts: the attempt is read with a plain SELECT
// for its run, and the row references the run (migration 0042's header).
func GatewayRecordAttemptPlacement(ctx context.Context, q GWQuerier, lease LeaseView, p AttemptPlacement) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	tag, err := q.Exec(ctx, `
		INSERT INTO attempt_runtime AS rt
		       (attempt_id, run_id, lease_epoch, executor, cluster, k8s_namespace,
		        pod, k8s_node, first_reported_at, reported_at)
		SELECT l.attempt_id, a.run_id, l.epoch, $3, $4, $5, $6, $7, now(), now()
		  FROM leases l
		  JOIN task_attempts a ON a.id = l.attempt_id
		 WHERE l.id = $1 AND l.attempt_id = $2 AND l.expires_at > now()
		ON CONFLICT (attempt_id) DO UPDATE
		   SET first_reported_at = CASE WHEN rt.lease_epoch < EXCLUDED.lease_epoch
		                                THEN EXCLUDED.first_reported_at
		                                ELSE rt.first_reported_at END,
		       lease_epoch   = EXCLUDED.lease_epoch,
		       executor      = EXCLUDED.executor,
		       cluster       = EXCLUDED.cluster,
		       k8s_namespace = EXCLUDED.k8s_namespace,
		       pod           = EXCLUDED.pod,
		       k8s_node      = EXCLUDED.k8s_node,
		       reported_at   = EXCLUDED.reported_at
		 WHERE rt.lease_epoch <= EXCLUDED.lease_epoch`,
		lease.ID, lease.AttemptID, p.Executor, p.Cluster, p.K8sNamespace, p.Pod, p.K8sNode)
	if err != nil {
		return false, fmt.Errorf("record placement of attempt %s: %w", lease.AttemptID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// GatewayAttemptRuntimePresent reports whether migration 0042 is applied. The
// gateway asks once at startup and advertises ReportPlacement only when it
// is, so agents never report into a table that is not there (a gateway rolled
// out before its migration, ADR-0011 rule 5).
func GatewayAttemptRuntimePresent(ctx context.Context, q GWQuerier) (bool, error) {
	var ok bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('public.attempt_runtime') IS NOT NULL`).Scan(&ok); err != nil {
		return false, fmt.Errorf("detect attempt_runtime: %w", err)
	}
	return ok, nil
}

// Task-runner listing bounds: the console's live view asks for the default;
// an operator investigating a large fan-out may ask for up to the maximum.
const (
	DefaultTaskRunnerPage = 200
	MaxTaskRunnerPage     = 1000
)

// TaskRunnerRow is one in-flight attempt with where it runs. Placement fields
// are empty strings when the agent has not reported them (an agent that
// predates ReportPlacement, a pod not yet scheduled).
type TaskRunnerRow struct {
	AttemptID  string
	RunID      string
	NodeID     string
	Workflow   string
	WorkerID   string
	WorkerHost string

	Executor     string
	Cluster      string
	K8sNamespace string
	Pod          string
	K8sNode      string

	// DISPATCHED or RUNNING.
	State string
	// Nil while DISPATCHED: the agent has not confirmed the start.
	StartedAt *time.Time
	// The latest ReportProgress message (attempt_progress), "" when none.
	ProgressMessage string
	// Nil when no placement was reported.
	PlacementReportedAt *time.Time
}

// TaskRunnerSummary counts EVERY in-flight attempt of the namespace, not just
// the returned page. The empty key counts attempts with no reported executor
// or cluster.
type TaskRunnerSummary struct {
	Running    int
	ByExecutor map[string]int
	ByCluster  map[string]int
}

// GatewayListTaskRunners lists the namespace's in-flight attempts (DISPATCHED
// and RUNNING), newest dispatch first, at most limit of them, with the summary
// over all of them and the database time of the read.
//
// Namespace-scoped through the run, exactly as GatewayGetAttempt is: an
// attempt belongs to the namespace of its run, whichever worker holds it.
//
// ONE statement, so the summary and the page are read from the same snapshot:
// an attempt finishing mid-read cannot be counted and missing, or listed and
// uncounted. The summary rides on every row (and on the single all-NULL row
// an empty page leaves), aggregated as JSON.
func GatewayListTaskRunners(ctx context.Context, q GWQuerier, namespace string, limit int) (time.Time, TaskRunnerSummary, []TaskRunnerRow, error) {
	limit = ClampLimit(limit, DefaultTaskRunnerPage, MaxTaskRunnerPage)
	sum := TaskRunnerSummary{ByExecutor: map[string]int{}, ByCluster: map[string]int{}}

	// LEFT JOINs throughout the page: an attempt whose worker row was reaped,
	// whose spec row is unreadable or that reported nothing is still in
	// flight, and the view must not lose it.
	rows, err := q.Query(ctx, `
		WITH inflight AS MATERIALIZED (
		    SELECT a.id, a.run_id, a.node_id, a.worker_id, a.state, a.started_at,
		           a.dispatched_at, r.spec_id
		      FROM task_attempts a
		      JOIN workflow_runs r ON r.id = a.run_id
		     WHERE r.namespace = $1
		       AND a.state IN ('DISPATCHED','RUNNING')
		), summary AS (
		    SELECT coalesce(jsonb_agg(jsonb_build_array(g.executor, g.cluster, g.n)), '[]'::jsonb) AS counts
		      FROM (SELECT coalesce(rt.executor, '') AS executor, coalesce(rt.cluster, '') AS cluster,
		                   count(*)::int AS n
		              FROM inflight i
		              LEFT JOIN attempt_runtime rt ON rt.attempt_id = i.id
		             GROUP BY 1, 2) g
		), page AS (
		    SELECT i.id, i.dispatched_at, i.run_id, i.node_id, s.name AS workflow, i.worker_id,
		           w.hostname, rt.executor, rt.cluster, rt.k8s_namespace, rt.pod, rt.k8s_node,
		           i.state, i.started_at, p.message, rt.reported_at
		      FROM inflight i
		      LEFT JOIN workflow_specs s ON s.id = i.spec_id
		      LEFT JOIN workers w ON w.id = i.worker_id
		      LEFT JOIN attempt_progress p ON p.attempt_id = i.id
		      LEFT JOIN attempt_runtime rt ON rt.attempt_id = i.id
		     ORDER BY i.dispatched_at DESC NULLS LAST, i.id DESC
		     LIMIT $2
		)
		SELECT now(), summary.counts::text,
		       page.id::text, page.run_id::text, page.node_id, coalesce(page.workflow, ''),
		       coalesce(page.worker_id::text, ''), coalesce(page.hostname, ''),
		       coalesce(page.executor, ''), coalesce(page.cluster, ''), coalesce(page.k8s_namespace, ''),
		       coalesce(page.pod, ''), coalesce(page.k8s_node, ''),
		       page.state::text, page.started_at, coalesce(page.message, ''), page.reported_at
		  FROM summary
		  LEFT JOIN page ON true
		 ORDER BY page.dispatched_at DESC NULLS LAST, page.id DESC`, namespace, limit)
	if err != nil {
		return time.Time{}, sum, nil, fmt.Errorf("list task runners: %w", err)
	}
	defer rows.Close()

	var (
		at      time.Time
		counts  string
		out     = []TaskRunnerRow{}
		counted bool
	)
	for rows.Next() {
		var (
			t                                           TaskRunnerRow
			id, runID, nodeID, workflow, workerID, host *string
			executor, cluster, k8sNS, pod, node, state  *string
			message                                     *string
		)
		if err := rows.Scan(&at, &counts, &id, &runID, &nodeID, &workflow, &workerID, &host,
			&executor, &cluster, &k8sNS, &pod, &node, &state, &t.StartedAt, &message,
			&t.PlacementReportedAt); err != nil {
			return time.Time{}, sum, nil, fmt.Errorf("scan task runner: %w", err)
		}
		if !counted {
			if err := decodeRunnerCounts(counts, &sum); err != nil {
				return time.Time{}, sum, nil, err
			}
			counted = true
		}
		if id == nil {
			continue // the empty page's single row
		}
		t.AttemptID, t.RunID, t.NodeID, t.Workflow = *id, *runID, *nodeID, *workflow
		t.WorkerID, t.WorkerHost = *workerID, *host
		t.Executor, t.Cluster, t.K8sNamespace, t.Pod, t.K8sNode = *executor, *cluster, *k8sNS, *pod, *node
		t.State, t.ProgressMessage = *state, *message
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, sum, nil, fmt.Errorf("list task runners: %w", err)
	}
	return at.UTC(), sum, out, nil
}

// decodeRunnerCounts folds the summary's [[executor, cluster, n], ...] into
// sum.
func decodeRunnerCounts(raw string, sum *TaskRunnerSummary) error {
	var groups [][3]any
	if err := json.Unmarshal([]byte(raw), &groups); err != nil {
		return fmt.Errorf("decode task runner summary: %w", err)
	}
	for _, g := range groups {
		executor, _ := g[0].(string)
		cluster, _ := g[1].(string)
		n, _ := g[2].(float64)
		sum.Running += int(n)
		sum.ByExecutor[executor] += int(n)
		sum.ByCluster[cluster] += int(n)
	}
	return nil
}
