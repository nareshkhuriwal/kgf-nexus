// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	nexusv1 "github.com/khuriwalgroup/kgf-nexus/gen/nexus/v1"
	"github.com/khuriwalgroup/kgf-nexus/internal/domain"
)

// Claim-time placement (ADR-0029 §4, ADR-0030, ADR-0031).
//
// Everything in this file either CHOOSES among workers that already fit (push)
// or DECLINES a candidate for now (pull). None of it spends capacity: the fit
// decision stays domain.Fits under the worker's row lock, the quota decision
// stays under the tenant advisory lock, and the dispatch stays one transaction.
// A wrong answer here costs placement quality or a bounded delay, never an
// over-commit.

// workerFacts are the placement-relevant facts of one worker, read with its row.
type workerFacts struct {
	// Labels are the EFFECTIVE labels: the worker's own overlaid with its
	// cluster's (domain.EffectiveLabels).
	Labels map[string]string
	// Cached is the worker's advertised artifact-cache keys.
	Cached map[string]bool
	// Headroom is the cluster's published ResourceQuota room per task
	// namespace; nil when the worker has no cluster or none was published.
	Headroom map[string]headroomVec
	// HeadroomAt is when that headroom was observed.
	HeadroomAt *time.Time
}

// headroomVec is one namespace's remaining quota room; -1 = unbounded.
type headroomVec struct {
	CPUMillis   int64            `json:"cpu_millis"`
	MemoryBytes int64            `json:"memory_bytes"`
	GPUCount    int64            `json:"gpu_count"`
	Pods        int64            `json:"pods"`
	Extended    map[string]int64 `json:"extended"`
}

// placementCols is the select-list fragment that reads a worker's placement
// facts, aliased w, from the pf join placementJoin adds. With migration 0035
// missing it reads constants and placementJoin adds nothing, so a scheduler
// started ahead of the migration keeps dispatching (the DetectQuotaSchema
// argument).
func (s *SchedulerStore) placementCols() string {
	if s.placementMissing {
		return `'{}'::jsonb, '{}'::text[], '{}'::jsonb, '{}'::jsonb, NULL::timestamptz`
	}
	return `w.labels, w.cached_artifacts,
       COALESCE(pf.labels, '{}'::jsonb), COALESCE(pf.headroom, '{}'::jsonb), pf.headroom_observed_at`
}

// placementJoin resolves the worker's cluster facts in ONE lateral lookup,
// evaluated only for a worker that carries the cluster label: a worker without
// one (every worker before ADR-0031, and every bench worker) pays nothing. The
// cluster is matched by name WITHIN the worker's tenant, so naming another
// tenant's cluster yields no labels. LEFT JOIN on the non-nullable side keeps
// `FOR UPDATE OF w` valid.
func (s *SchedulerStore) placementJoin() string {
	if s.placementMissing {
		return ""
	}
	return `
  LEFT JOIN LATERAL (
    SELECT cs.labels, cs.headroom, cs.headroom_observed_at
      FROM clusters c JOIN cluster_status cs ON cs.cluster_id = c.id
     WHERE w.labels ? 'nexus.io/cluster'
       AND c.name = w.labels ->> 'nexus.io/cluster' AND c.tenant_id = w.tenant_id
  ) pf ON true`
}

// scanFacts decodes the placementCols values. Malformed JSON degrades to "no
// facts" rather than failing the claim.
func scanFacts(labelsJSON []byte, cached []string, clusterLabels, headroom []byte, at *time.Time) workerFacts {
	var own, cl map[string]string
	_ = json.Unmarshal(labelsJSON, &own)
	_ = json.Unmarshal(clusterLabels, &cl)
	f := workerFacts{Labels: domain.EffectiveLabels(own, cl), HeadroomAt: at}
	if f.Labels == nil {
		f.Labels = map[string]string{}
	}
	if len(cached) > 0 {
		f.Cached = make(map[string]bool, len(cached))
		for _, k := range cached {
			f.Cached[k] = true
		}
	}
	if len(headroom) > 2 {
		_ = json.Unmarshal(headroom, &f.Headroom)
	}
	return f
}

// DetectPlacementSchema checks for migration 0035 and falls back to reading no
// placement facts without it. Call once at startup, before claims.
func (s *SchedulerStore) DetectPlacementSchema(ctx context.Context) (bool, error) {
	var ok bool
	if err := s.pool.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'workers' AND column_name = 'cached_artifacts')
   AND EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'cluster_status' AND column_name = 'headroom')
   AND EXISTS (SELECT 1 FROM information_schema.columns
                WHERE table_name = 'task_nodes' AND column_name = 'preemptions_used')`).Scan(&ok); err != nil {
		return false, fmt.Errorf("detect migration 0035: %w", err)
	}
	s.placementMissing = !ok
	return ok, nil
}

func (s *SchedulerStore) scoreConfig() domain.ScoreConfig {
	return domain.ScoreConfig{Policy: s.cfg.PlacementPolicy, Cost: s.cfg.Cost}
}

func isKubernetesExecutor(kind string) bool {
	return domain.CanonicalExecutor(kind) == domain.ExecutorKubernetes
}

// headroomBlocks applies a cluster's published ResourceQuota headroom (ADR-0031
// §3) to an attempt that will create pods in the worker's task namespace. It
// returns a pending_reason when the attempt does not fit, "" otherwise.
//
// executor is the one the attempt runs on if this worker takes it
// (domain.EffectiveExecutor): a task that names none becomes a pod on a
// kubernetes-default worker, and is gated like one that names kubernetes.
//
// The observed headroom is reduced by what was reserved on the same cluster's
// workers for that namespace SINCE the observation, which the quota controller
// had not counted yet. Stale or missing observations gate nothing: this is a
// routing guard, and the API server's quota admission is the enforcement.
func (s *SchedulerStore) headroomBlocks(
	ctx context.Context, tx pgx.Tx, now time.Time, workerID uuid.UUID, f *workerFacts, cand *Candidate,
	executor string,
) (string, error) {
	if !isKubernetesExecutor(executor) || f.HeadroomAt == nil || len(f.Headroom) == 0 {
		return "", nil
	}
	ns, cluster := f.Labels[domain.LabelK8sTaskNamespace], f.Labels[domain.LabelCluster]
	h, ok := f.Headroom[ns]
	if ns == "" || cluster == "" || !ok || now.Sub(*f.HeadroomAt) > s.cfg.HeadroomTTL {
		return "", nil
	}
	var cpu, mem, gpu, pods int64
	if err := tx.QueryRow(ctx, `
SELECT COALESCE(sum(COALESCE((r.resources ->> 'cpu_millis')::bigint, 0)), 0)::bigint,
       COALESCE(sum(COALESCE((r.resources ->> 'memory_bytes')::bigint, 0)), 0)::bigint,
       COALESCE(sum(COALESCE((r.resources ->> 'gpu_count')::bigint, 0)), 0)::bigint,
       count(*)
  FROM reservations r
  JOIN workers w ON w.id = r.worker_id
 WHERE w.tenant_id = (SELECT tenant_id FROM workers WHERE id = $1)
   AND w.labels ->> 'nexus.io/cluster' = $2
   AND w.labels ->> 'nexus.io/k8s-task-namespace' = $3
   AND r.created_at > $4`, workerID, cluster, ns, *f.HeadroomAt).Scan(&cpu, &mem, &gpu, &pods); err != nil {
		return "", fmt.Errorf("read reservations since headroom observation: %w", err)
	}
	req := cand.ResourceRequest
	check := func(dim string, room, used, need int64) string {
		if room < 0 || need <= 0 {
			return ""
		}
		if left := room - used; need > left {
			if left < 0 {
				left = 0
			}
			return fmt.Sprintf("waiting_for_cluster_quota: cluster %s namespace %s %s (need %d, headroom %d)",
				cluster, ns, dim, need, left)
		}
		return ""
	}
	for _, r := range []string{
		check("pods", h.Pods, pods, 1),
		check("cpu_millis", h.CPUMillis, cpu, req.GetCpuMillis()),
		check("memory_bytes", h.MemoryBytes, mem, req.GetMemoryBytes()),
		check("gpu_count", h.GPUCount, gpu, int64(req.GetGpuCount())),
	} {
		if r != "" {
			return r, nil
		}
	}
	return "", nil
}

// resolveSameHost fills Placement.SameHost from prefer_same_worker_as: the
// hostname of the worker whose attempt of the named node most recently
// SUCCEEDED in this run (ADR-0030 §5). Hostname rather than worker id because
// ids are per session and the data lives on the host.
func (s *SchedulerStore) resolveSameHost(ctx context.Context, tx pgx.Tx, cand *Candidate) error {
	ref := cand.Spec.PreferSameWorkerAs
	if ref == "" {
		return nil
	}
	if cand.Spec.Placement == nil {
		cand.Spec.Placement = &domain.Placement{}
	}
	if cand.Spec.Placement.SameHost != "" {
		return nil
	}
	var host string
	err := tx.QueryRow(ctx, `
SELECT w.hostname
  FROM task_attempts a JOIN workers w ON w.id = a.worker_id
 WHERE a.run_id = $1 AND a.node_id = $2 AND a.state = 'SUCCEEDED'
 ORDER BY a.attempt_number DESC LIMIT 1`, cand.RunID, ref).Scan(&host)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve prefer_same_worker_as %q: %w", ref, err)
	}
	cand.Spec.Placement.SameHost = host
	return nil
}

// peerWorker is another worker as the pull claim's deferral sees it: read
// WITHOUT a lock, because it is only ever a reason to decline, never to spend.
type peerWorker struct {
	view  domain.WorkerView
	facts workerFacts
	// executors and defaultExecutor decide whether the peer could run the
	// candidate at all: a peer that cannot is no reason to wait.
	executors       []string
	defaultExecutor string
}

// canRun reports whether the peer could run cand: it advertises the executor
// the task names, or, for a task naming none, its default can run it.
func (p *peerWorker) canRun(cand *Candidate) bool {
	if named := cand.Spec.Executor; named != "" {
		for _, e := range p.executors {
			if e == named {
				return true
			}
		}
		return false
	}
	return domain.CanRunUnnamed(domain.WorkerDefaultExecutor(
		map[string]string{domain.LabelDefaultExecutor: p.defaultExecutor}), cand.Spec.Image != "")
}

// peerSet lazily loads the polling worker's peers once per claim transaction.
type peerSet struct {
	loaded bool
	peers  []peerWorker
}

// maxDeferralPeers bounds the peer read. A fleet larger than this per
// tenant+queue still defers correctly against the first maxDeferralPeers
// workers by id; it only misses better workers beyond them.
const maxDeferralPeers = 256

func (s *SchedulerStore) loadPeers(ctx context.Context, tx pgx.Tx, now time.Time, worker *pollWorker, ps *peerSet) error {
	if ps.loaded {
		return nil
	}
	ps.loaded = true
	rows, err := tx.Query(ctx, `
SELECT w.hostname, w.capacity, w.committed, w.executors,
       COALESCE(w.labels ->> '`+domain.LabelDefaultExecutor+`', ''), `+s.placementCols()+`
  FROM workers w`+s.placementJoin()+`
 WHERE w.tenant_id = (SELECT tenant_id FROM workers WHERE id = $1)
   AND w.id <> $1
   AND w.state = 'ACTIVE'
   AND $2 = ANY(w.queues)
   AND w.last_heartbeat_at > $3
 ORDER BY w.id
 LIMIT $4`, worker.ID, s.cfg.Queue, now.Add(-s.cfg.LivenessWindow), maxDeferralPeers)
	if err != nil {
		return fmt.Errorf("read deferral peers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			host                         string
			capJSON, comJSON, labelsJSON []byte
			executors, cached            []string
			defaultExecutor              string
			clLabels, headroom           []byte
			at                           *time.Time
		)
		if err := rows.Scan(&host, &capJSON, &comJSON, &executors, &defaultExecutor,
			&labelsJSON, &cached, &clLabels, &headroom, &at); err != nil {
			return fmt.Errorf("scan deferral peer: %w", err)
		}
		capacity, err := decodeVector(capJSON)
		if err != nil {
			continue
		}
		committed, err := decodeVector(comJSON)
		if err != nil {
			continue
		}
		f := scanFacts(labelsJSON, cached, clLabels, headroom, at)
		ps.peers = append(ps.peers, peerWorker{facts: f, executors: executors, defaultExecutor: defaultExecutor,
			view: domain.WorkerView{
				Hostname: host, Labels: f.Labels, Capacity: capacity, Committed: committed, Cached: f.Cached,
			}})
	}
	return rows.Err()
}

// shouldDefer is bounded delay scheduling for the pull path (ADR-0029 §4): true
// when another live worker would score strictly better for this candidate AND
// fits it right now, and the candidate has not yet waited PlacementWait since
// it became due. The candidate is then skipped -- left PENDING for that
// worker's own poll -- exactly like a look-ahead skip.
func (s *SchedulerStore) shouldDefer(
	ctx context.Context, tx pgx.Tx, now time.Time, worker *pollWorker, cand *Candidate, ps *peerSet,
) (bool, error) {
	if s.cfg.PlacementWait <= 0 || now.Sub(cand.ScheduledAt) >= s.cfg.PlacementWait {
		return false, nil
	}
	if err := s.resolveSameHost(ctx, tx, cand); err != nil {
		return false, err
	}
	if !domain.WantsScoring(cand.ResourceRequest, cand.Spec.Placement) {
		return false, nil
	}
	cfg := s.scoreConfig()
	mine := domain.PlacementScore(domain.WorkerView{
		Hostname: worker.Hostname, Labels: worker.facts.Labels, Capacity: worker.Capacity,
		Committed: worker.Committed, Cached: worker.facts.Cached,
	}, cand.ResourceRequest, cand.Spec.Placement, cfg)
	if err := s.loadPeers(ctx, tx, now, worker, ps); err != nil {
		return false, err
	}
	for i := range ps.peers {
		p := &ps.peers[i]
		if !p.canRun(cand) {
			continue
		}
		if ok, _ := domain.Eligible(p.facts.Labels, cand.Spec.Placement); !ok {
			continue
		}
		if !labelsContain(p.facts.Labels, cand.Spec.NodeSelector) {
			continue
		}
		if domain.BetterWorker(domain.PlacementScore(p.view, cand.ResourceRequest, cand.Spec.Placement, cfg), mine) {
			return true, nil
		}
	}
	return false, nil
}

func labelsContain(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// placeOn runs domain.Place against a locked worker and records the placed
// vector on it for commitDispatch.
func (s *SchedulerStore) placeOn(w *WorkerCandidate, req *nexusv1.ResourceVector) []domain.Shortfall {
	placed, ok, short := domain.Place(w.Capacity, w.Committed, req, s.cfg.PlacementPolicy)
	if !ok {
		return short
	}
	w.Placed = placed
	return nil
}

// chargePreemption marks a retry timer whose retry is charged to
// task_nodes.preemptions_used.
const chargePreemption = "preemption"

// preemptionsUsedCol reads task_nodes.preemptions_used, or 0 before 0035.
func (s *SchedulerStore) preemptionsUsedCol() string {
	if s.placementMissing {
		return "0"
	}
	return "n.preemptions_used"
}
