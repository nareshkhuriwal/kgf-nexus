// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// Cluster-controller support for the RESOURCE MANAGER (migration 0018).
//
// The resource manager OWNS the observed-cluster tables written here:
// cluster_status, cluster_observations, cluster_scaling_state,
// cluster_reconcile_errors and cluster_scaling_events. It READS the gateway-owned
// registry (clusters, cluster_namespace_bindings), platform_config and the
// scheduler-owned leases, which ADR-0011 permits. The only non-observation write
// is workers.state -> DRAINING, and workers is already this service's table.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
)

var (
	// ErrClusterNotFound is a registry miss: the controller was started with a
	// name no operator registered.
	ErrClusterNotFound = errors.New("store: cluster not registered")
	// ErrClusterNotApplying refuses a mutation for a cluster that is not in
	// apply mode, or whose identity is unverified. Mapped to FailedPrecondition
	// so the controller logs it as a policy refusal, not an outage.
	ErrClusterNotApplying = errors.New("store: cluster is not enabled for apply")
	// ErrBindingNotFound refuses an action on a namespace/deployment the
	// registry did not hand to this cluster.
	ErrBindingNotFound = errors.New("store: no such binding for this cluster")
	// ErrInvalidClusterReport marks a malformed controller report, so the API
	// answers InvalidArgument (a controller bug) rather than Internal.
	ErrInvalidClusterReport = errors.New("store: invalid cluster report")
)

// ClusterNameByID resolves a registry id to its name.
func (s *RMStore) ClusterNameByID(ctx context.Context, id string) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT name FROM clusters WHERE id::text = $1`, id).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrClusterNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: resolve cluster %s: %w", id, err)
	}
	return name, nil
}

// MaxObservationSummaryBytes bounds a single reported object's summary. The
// metadata store is not a Kubernetes object cache; a summary that needs more
// than this is carrying something it should not.
const MaxObservationSummaryBytes = 8 << 10

// ClusterAssignment is a cluster's desired state as the controller consumes it.
type ClusterAssignment struct {
	ID                    string
	Name                  string
	Namespace             string
	Provider              string
	ProviderResourceID    string
	ExpectedClusterUID    string
	ControlPlaneNamespace string
	Mode                  string
	DesiredRevision       int64
	Bindings              []ClusterBinding
}

// ClusterBinding is one worker Deployment and the pool policy that drives it.
type ClusterBinding struct {
	Namespace           string
	KubernetesNamespace string
	WorkerDeployment    string
	PoolName            string
	MaxReplicas         int
	MaxScaleDownStep    int
	ScaleUpCooldown     time.Duration
	ScaleDownCooldown   time.Duration
	Pool                platform.Pool
	Profile             platform.PodProfile
	PlatformRevision    int64
	LastScaleAt         *time.Time
	LastScaleDirection  string
	IdleSince           *time.Time
	// ConfigError is set when the binding names a pool or profile that the
	// namespace's platform_config no longer has. Reported, never guessed around.
	ConfigError string
}

// EffectiveMax is the smaller of the pool policy and the registry ceiling: a
// pool edit may lower capacity but never raise it past what the cluster owner
// allowed.
func (b ClusterBinding) EffectiveMax() int {
	if b.MaxReplicas < b.Pool.MaxWorkers {
		return b.MaxReplicas
	}
	return b.Pool.MaxWorkers
}

// ClusterAssignmentByName loads the registry entry, its bindings, each
// binding's pool policy and the persisted scaling state.
func (s *RMStore) ClusterAssignmentByName(ctx context.Context, name string) (*ClusterAssignment, error) {
	var a ClusterAssignment
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, namespace, provider, provider_resource_id, expected_cluster_uid,
		       control_plane_namespace, mode, desired_revision
		  FROM clusters WHERE name = $1`, name).Scan(
		&a.ID, &a.Name, &a.Namespace, &a.Provider, &a.ProviderResourceID, &a.ExpectedClusterUID,
		&a.ControlPlaneNamespace, &a.Mode, &a.DesiredRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrClusterNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: load cluster %q: %w", name, err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT b.namespace, b.kubernetes_namespace, b.worker_deployment, b.pool, b.max_replicas,
		       b.max_scale_down_step, b.scale_up_cooldown_seconds, b.scale_down_cooldown_seconds,
		       s.last_scale_at, COALESCE(s.last_scale_direction, ''), s.idle_since
		  FROM cluster_namespace_bindings b
		  LEFT JOIN cluster_scaling_state s
		    ON s.cluster_id = b.cluster_id AND s.kubernetes_namespace = b.kubernetes_namespace
		   AND s.worker_deployment = b.worker_deployment
		 WHERE b.cluster_id = $1
		 ORDER BY b.kubernetes_namespace, b.worker_deployment`, a.ID)
	if err != nil {
		return nil, fmt.Errorf("store: load bindings for cluster %s: %w", a.ID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var b ClusterBinding
		var upSec, downSec int
		if err := rows.Scan(&b.Namespace, &b.KubernetesNamespace, &b.WorkerDeployment, &b.PoolName,
			&b.MaxReplicas, &b.MaxScaleDownStep, &upSec, &downSec,
			&b.LastScaleAt, &b.LastScaleDirection, &b.IdleSince); err != nil {
			return nil, fmt.Errorf("store: scan binding: %w", err)
		}
		b.ScaleUpCooldown = time.Duration(upSec) * time.Second
		b.ScaleDownCooldown = time.Duration(downSec) * time.Second
		a.Bindings = append(a.Bindings, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate bindings: %w", err)
	}

	// Pool policy lives in platform_config (0010) and is resolved per binding
	// here rather than copied into the registry, so there is one source of truth
	// for min/max/slots and a pool edit takes effect on the next cycle.
	configs := map[string]platform.Config{}
	for i := range a.Bindings {
		b := &a.Bindings[i]
		cfg, ok := configs[b.Namespace]
		if !ok {
			cfg, err = GatewayPlatformGet(ctx, s.pool, b.Namespace)
			if err != nil {
				return nil, fmt.Errorf("store: load platform config for %s: %w", b.Namespace, err)
			}
			configs[b.Namespace] = cfg
		}
		b.PlatformRevision = cfg.Revision
		resolveBindingPolicy(b, cfg)
	}
	return &a, nil
}

func resolveBindingPolicy(b *ClusterBinding, cfg platform.Config) {
	for _, p := range cfg.Pools {
		if p.Name == b.PoolName {
			b.Pool = p
		}
	}
	if b.Pool.Name == "" {
		b.ConfigError = fmt.Sprintf("pool %q is not defined in namespace %s platform config revision %d", b.PoolName, b.Namespace, cfg.Revision)
		return
	}
	for _, pr := range cfg.Profiles {
		if pr.Name == b.Pool.Profile {
			b.Profile = pr
		}
	}
	if b.Profile.Name == "" {
		b.ConfigError = fmt.Sprintf("pool %q references unknown pod profile %q", b.PoolName, b.Pool.Profile)
	}
}

// ObservedObject is one row of cluster_observations.
type ObservedObject struct {
	Kind               string
	Namespace          string
	Name               string
	UID                string
	Generation         int64
	ObservedGeneration int64
	SpecReplicas       *int32
	ReadyReplicas      *int32
	Ready              bool
	ResourceVersion    string
	SummaryJSON        []byte
	CreatedAt          *time.Time
}

// ObservationReport is one controller self-report.
type ObservationReport struct {
	ClusterID           string
	ControllerIdentity  string
	ObservedClusterUID  string
	KubernetesVersion   string
	ObservedRevision    int64
	EffectiveMode       string
	EffectiveModeReason string
	CompleteKinds       []string
	Objects             []ObservedObject
	LastError           string

	// ADR-0031: placement labels and ResourceQuota headroom per task
	// namespace. Each replaces the stored value only when reported, so an
	// older controller that sends neither leaves them alone.
	Labels           map[string]string
	LabelsReported   bool
	Headroom         map[string]ClusterHeadroom
	HeadroomReported bool
}

// ClusterHeadroom is one namespace's remaining ResourceQuota room; -1 means
// unbounded. The JSON shape is what the claim reads (headroomVec).
type ClusterHeadroom struct {
	CPUMillis   int64            `json:"cpu_millis"`
	MemoryBytes int64            `json:"memory_bytes"`
	GPUCount    int64            `json:"gpu_count"`
	Pods        int64            `json:"pods"`
	Extended    map[string]int64 `json:"extended,omitempty"`
}

// Bounds on a report's placement facts: they are read by every claim on the
// cluster's workers, so their size is a dispatch cost.
const (
	maxClusterLabels      = 64
	maxClusterLabelLen    = 256
	maxHeadroomNamespaces = 64
)

var observationKinds = map[string]bool{
	"SchedulerDeployment": true, "WorkerDeployment": true, "Deployment": true,
	"Node": true, "Job": true, "HorizontalPodAutoscaler": true,
}

// ReportObservations stores a controller self-report and returns the cluster's
// current desired revision.
//
// Kinds are replaced only when listed as complete. A kind whose list failed
// keeps its last rows (with their old observed_at, which the console shows),
// because replacing them with nothing would report "zero Nodes" when the truth
// is "unknown" -- the same zero-versus-failed confusion platform.Plan warns about.
func (s *RMStore) ReportObservations(ctx context.Context, r ObservationReport) (int64, error) {
	if r.EffectiveMode != "apply" {
		r.EffectiveMode = "plan"
	}
	complete := map[string]bool{}
	for _, k := range r.CompleteKinds {
		if !observationKinds[k] {
			return 0, fmt.Errorf("%w: unknown observation kind %q", ErrInvalidClusterReport, k)
		}
		complete[k] = true
	}
	for _, o := range r.Objects {
		if !observationKinds[o.Kind] {
			return 0, fmt.Errorf("%w: unknown observation kind %q", ErrInvalidClusterReport, o.Kind)
		}
		if !complete[o.Kind] {
			return 0, fmt.Errorf("%w: object %s/%s of kind %s reported without its kind marked complete", ErrInvalidClusterReport, o.Namespace, o.Name, o.Kind)
		}
		if len(o.SummaryJSON) > MaxObservationSummaryBytes {
			return 0, fmt.Errorf("%w: summary for %s %s/%s is %d bytes, over the %d byte bound", ErrInvalidClusterReport, o.Kind, o.Namespace, o.Name, len(o.SummaryJSON), MaxObservationSummaryBytes)
		}
		if len(o.SummaryJSON) > 0 && !json.Valid(o.SummaryJSON) {
			return 0, fmt.Errorf("%w: summary for %s %s/%s is not valid JSON", ErrInvalidClusterReport, o.Kind, o.Namespace, o.Name)
		}
	}

	if len(r.Labels) > maxClusterLabels {
		return 0, fmt.Errorf("%w: %d cluster labels; the bound is %d", ErrInvalidClusterReport, len(r.Labels), maxClusterLabels)
	}
	for k, v := range r.Labels {
		if k == "" || len(k) > maxClusterLabelLen || len(v) > maxClusterLabelLen {
			return 0, fmt.Errorf("%w: cluster label %q is empty or longer than %d", ErrInvalidClusterReport, k, maxClusterLabelLen)
		}
	}
	if len(r.Headroom) > maxHeadroomNamespaces {
		return 0, fmt.Errorf("%w: headroom for %d namespaces; the bound is %d", ErrInvalidClusterReport, len(r.Headroom), maxHeadroomNamespaces)
	}

	var desired int64
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT desired_revision FROM clusters WHERE id = $1`, r.ClusterID).Scan(&desired); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrClusterNotFound
			}
			return fmt.Errorf("store: load cluster %s: %w", r.ClusterID, err)
		}
		// observed_revision never moves backwards: a report from a demoted
		// leader that raced a newer one must not un-acknowledge a revision.
		if _, err := tx.Exec(ctx, `
			INSERT INTO cluster_status (cluster_id, observed_revision, observed_cluster_uid, kubernetes_version,
			       controller_identity, effective_mode, effective_mode_reason, last_report_at, last_error, last_error_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, now(), $8, CASE WHEN $8 = '' THEN NULL ELSE now() END)
			ON CONFLICT (cluster_id) DO UPDATE SET
			       observed_revision = GREATEST(cluster_status.observed_revision, EXCLUDED.observed_revision),
			       observed_cluster_uid = EXCLUDED.observed_cluster_uid,
			       kubernetes_version = EXCLUDED.kubernetes_version,
			       controller_identity = EXCLUDED.controller_identity,
			       effective_mode = EXCLUDED.effective_mode,
			       effective_mode_reason = EXCLUDED.effective_mode_reason,
			       last_report_at = now(),
			       last_error = EXCLUDED.last_error,
			       last_error_at = CASE WHEN EXCLUDED.last_error = '' THEN cluster_status.last_error_at ELSE now() END`,
			r.ClusterID, r.ObservedRevision, r.ObservedClusterUID, r.KubernetesVersion,
			r.ControllerIdentity, r.EffectiveMode, r.EffectiveModeReason, r.LastError); err != nil {
			return fmt.Errorf("store: upsert cluster status: %w", err)
		}
		if r.LabelsReported {
			labels := r.Labels
			if labels == nil {
				labels = map[string]string{}
			}
			b, err := json.Marshal(labels)
			if err != nil {
				return fmt.Errorf("store: encode cluster labels: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE cluster_status SET labels = $2 WHERE cluster_id = $1`, r.ClusterID, b); err != nil {
				return fmt.Errorf("store: record cluster labels: %w", err)
			}
		}
		if r.HeadroomReported {
			h := r.Headroom
			if h == nil {
				h = map[string]ClusterHeadroom{}
			}
			b, err := json.Marshal(h)
			if err != nil {
				return fmt.Errorf("store: encode cluster headroom: %w", err)
			}
			// Observed "now" by the database clock, the one the claim compares
			// against (invariant 7: control-plane time).
			if _, err := tx.Exec(ctx,
				`UPDATE cluster_status SET headroom = $2, headroom_observed_at = now() WHERE cluster_id = $1`,
				r.ClusterID, b); err != nil {
				return fmt.Errorf("store: record cluster headroom: %w", err)
			}
		}
		for kind := range complete {
			if _, err := tx.Exec(ctx, `DELETE FROM cluster_observations WHERE cluster_id = $1 AND kind = $2`, r.ClusterID, kind); err != nil {
				return fmt.Errorf("store: clear %s observations: %w", kind, err)
			}
		}
		for _, o := range r.Objects {
			summary := o.SummaryJSON
			if len(summary) == 0 {
				summary = []byte("{}")
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO cluster_observations (cluster_id, kind, kubernetes_namespace, name, uid, generation,
				       observed_generation, spec_replicas, ready_replicas, ready, resource_version, summary,
				       object_created_at, observed_at)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, now())
				ON CONFLICT (cluster_id, kind, kubernetes_namespace, name) DO NOTHING`,
				r.ClusterID, o.Kind, o.Namespace, o.Name, o.UID, o.Generation, o.ObservedGeneration,
				o.SpecReplicas, o.ReadyReplicas, o.Ready, o.ResourceVersion, summary, o.CreatedAt); err != nil {
				return fmt.Errorf("store: insert observation %s %s/%s: %w", o.Kind, o.Namespace, o.Name, err)
			}
		}
		return nil
	})
	return desired, err
}

// PodWorkers aggregates every worker row registered from one pod. In a
// Kubernetes worker Deployment the agent's hostname IS the pod name, which is
// the only join between a pod and a worker row that does not need the agent to
// cooperate.
type PodWorkers struct {
	Hostname         string
	Active           int
	Draining         int
	Quarantined      int
	LeaseCount       int
	ReservationCount int
	LastHeartbeat    *time.Time
}

// Removable reports whether the pod can be deleted without losing work: nothing
// from it may still accept dispatch, and nothing may hold a lease or capacity.
func (p PodWorkers) Removable() (bool, string) {
	switch {
	case p.Active > 0:
		return false, fmt.Sprintf("%d ACTIVE worker(s) still accept dispatch", p.Active)
	case p.Quarantined > 0:
		return false, fmt.Sprintf("%d QUARANTINED worker(s) not drained", p.Quarantined)
	case p.LeaseCount > 0:
		return false, fmt.Sprintf("holds %d live lease(s)", p.LeaseCount)
	case p.ReservationCount > 0:
		return false, fmt.Sprintf("holds %d capacity reservation(s)", p.ReservationCount)
	case p.Draining == 0:
		// No live row at all: never registered, or already DEAD and released.
		// Safe to remove -- there is nothing on it for Nexus to lose.
		return true, "no live worker registered from this pod"
	}
	return true, "drained with no leases or reservations"
}

// PodWorkerStates returns one aggregate per requested hostname (zero-valued
// for a hostname with no worker rows).
func (s *RMStore) PodWorkerStates(ctx context.Context, namespace string, hostnames []string) (map[string]PodWorkers, error) {
	return podWorkerStates(ctx, s.pool, namespace, hostnames)
}

func podWorkerStates(ctx context.Context, q GWQuerier, namespace string, hostnames []string) (map[string]PodWorkers, error) {
	out := make(map[string]PodWorkers, len(hostnames))
	for _, h := range hostnames {
		out[h] = PodWorkers{Hostname: h}
	}
	if len(hostnames) == 0 {
		return out, nil
	}
	// Leases and reservations are counted across EVERY row for the hostname,
	// DEAD included: the liveness sweep marks a worker DEAD before its leases
	// are expired and recovered, and in that window work may still be running
	// in the pod.
	rows, err := q.Query(ctx, `
		SELECT w.hostname,
		       count(*) FILTER (WHERE w.state = 'ACTIVE'),
		       count(*) FILTER (WHERE w.state = 'DRAINING'),
		       count(*) FILTER (WHERE w.state = 'QUARANTINED'),
		       COALESCE(sum(lc.n), 0),
		       COALESCE(sum(rc.n), 0),
		       max(w.last_heartbeat_at) FILTER (WHERE w.state <> 'DEAD')
		  FROM workers w
		  LEFT JOIN LATERAL (SELECT count(*) AS n FROM leases l WHERE l.worker_id = w.id) lc ON true
		  LEFT JOIN LATERAL (SELECT count(*) AS n FROM reservations r WHERE r.worker_id = w.id) rc ON true
		 WHERE w.namespace = $1 AND w.hostname = ANY($2)
		 GROUP BY w.hostname`, namespace, hostnames)
	if err != nil {
		return nil, fmt.Errorf("store: pod worker states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p PodWorkers
		if err := rows.Scan(&p.Hostname, &p.Active, &p.Draining, &p.Quarantined,
			&p.LeaseCount, &p.ReservationCount, &p.LastHeartbeat); err != nil {
			return nil, fmt.Errorf("store: scan pod worker state: %w", err)
		}
		out[p.Hostname] = p
	}
	return out, rows.Err()
}

// PoolDemand observes a bound pool's queue demand. It reuses the exact query
// behind the console's plan preview (a read of task_attempts and workers), so
// the preview and the controller can never disagree about what demand is.
func (s *RMStore) PoolDemand(ctx context.Context, b ClusterBinding) (platform.Demand, error) {
	d, err := GatewayPlatformDemand(ctx, s.pool, b.Namespace, b.Pool, b.Profile)
	if err != nil {
		return d, fmt.Errorf("store: observe demand for pool %s/%s: %w", b.Namespace, b.PoolName, err)
	}
	return d, nil
}

// clusterForMutation loads the cluster row and refuses unless it is
// registered for apply with a verified identity. Checked here -- in the owning
// service, inside the mutating transaction -- rather than trusted from the
// controller, so a controller running in the wrong mode or against the wrong
// cluster cannot drain anything.
func clusterForMutation(ctx context.Context, tx pgx.Tx, clusterID, namespace, observedUID, k8sNamespace, deployment string) error {
	var mode, expected, owner string
	// No FOR SHARE: a row lock needs UPDATE privilege on the registry, which
	// the resource manager deliberately lacks. The check is therefore as of
	// this statement; an operator switching to plan concurrently takes effect
	// on the controller's very next call, and a drain is harmless on its own
	// (it only stops new dispatch to one pod's workers).
	err := tx.QueryRow(ctx, `SELECT mode, expected_cluster_uid, namespace FROM clusters WHERE id = $1`, clusterID).
		Scan(&mode, &expected, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrClusterNotFound
	}
	if err != nil {
		return fmt.Errorf("store: load cluster %s: %w", clusterID, err)
	}
	if mode != "apply" {
		return fmt.Errorf("%w: registry mode is %q", ErrClusterNotApplying, mode)
	}
	if expected == "" || observedUID != expected {
		return fmt.Errorf("%w: observed cluster uid %q does not match the registered %q", ErrClusterNotApplying, observedUID, expected)
	}
	var bound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cluster_namespace_bindings
		WHERE cluster_id = $1 AND namespace = $2 AND kubernetes_namespace = $3 AND worker_deployment = $4)`,
		clusterID, namespace, k8sNamespace, deployment).Scan(&bound); err != nil {
		return fmt.Errorf("store: check binding: %w", err)
	}
	if !bound || namespace != owner {
		return fmt.Errorf("%w: %s -> %s/%s", ErrBindingNotFound, namespace, k8sNamespace, deployment)
	}
	return nil
}

// DrainPod moves every ACTIVE or QUARANTINED worker registered from the pod to
// DRAINING and records the event, in one transaction.
//
// Why this is enough to stop new dispatch: the scheduler's claim selects
// workers `WHERE state = 'ACTIVE' ... FOR UPDATE`, so this UPDATE waits for any
// claim holding the row and every later claim sees DRAINING. Once it commits,
// every lease that will ever exist on these workers already exists and is
// visible to CheckPodRemovable. DRAINING is also sticky: SetWorkerState has no
// DRAINING -> ACTIVE transition, so nothing can quietly re-open admission.
func (s *RMStore) DrainPod(ctx context.Context, clusterID, namespace, k8sNamespace, deployment, hostname, observedUID, reason, actor string) (drained, already int, err error) {
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		if err := clusterForMutation(ctx, tx, clusterID, namespace, observedUID, k8sNamespace, deployment); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `
			UPDATE workers SET state = 'DRAINING'
			 WHERE namespace = $1 AND hostname = $2 AND state IN ('ACTIVE','QUARANTINED')`, namespace, hostname)
		if err != nil {
			return fmt.Errorf("store: drain workers of pod %s: %w", hostname, err)
		}
		drained = int(tag.RowsAffected())
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workers WHERE namespace = $1 AND hostname = $2 AND state = 'DRAINING'`,
			namespace, hostname).Scan(&already); err != nil {
			return fmt.Errorf("store: count draining workers: %w", err)
		}
		already -= drained
		detail, _ := json.Marshal(map[string]any{"hostname": hostname, "drained": drained, "already_draining": already})
		_, err = tx.Exec(ctx, `
			INSERT INTO cluster_scaling_events (cluster_id, kubernetes_namespace, worker_deployment, action, mode,
			       applied, reason, actor, detail)
			VALUES ($1, $2, $3, 'drain', 'apply', true, $4, $5, $6)`, clusterID, k8sNamespace, deployment, reason, actor, detail)
		if err != nil {
			return fmt.Errorf("store: record drain event: %w", err)
		}
		return nil
	})
	return drained, already, err
}

// CheckPodRemovable is the last check before capacity is removed. It is a
// fresh read, never a cached one: the controller calls it immediately before
// patching the Deployment.
func (s *RMStore) CheckPodRemovable(ctx context.Context, clusterID, namespace, hostname string) (PodWorkers, bool, string, error) {
	var owner string
	err := s.pool.QueryRow(ctx, `SELECT namespace FROM clusters WHERE id = $1`, clusterID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return PodWorkers{}, false, "", ErrClusterNotFound
	}
	if err != nil {
		return PodWorkers{}, false, "", fmt.Errorf("store: load cluster %s: %w", clusterID, err)
	}
	if owner != namespace {
		return PodWorkers{}, false, "", fmt.Errorf("%w: namespace %s", ErrBindingNotFound, namespace)
	}
	states, err := s.PodWorkerStates(ctx, namespace, []string{hostname})
	if err != nil {
		return PodWorkers{}, false, "", err
	}
	p := states[hostname]
	ok, why := p.Removable()
	return p, ok, why, nil
}

// ScalingRecord is one controller decision to persist.
type ScalingRecord struct {
	ClusterID                string
	KubernetesNamespace      string
	WorkerDeployment         string
	Action                   string
	Mode                     string
	Applied                  bool
	FromReplicas, ToReplicas *int32
	Reason                   string
	Actor                    string
	Detail                   []byte
	ScaledDirection          string
	IdleUpdate               string // "", "set", "clear"
	IdleSince                time.Time
	ObservedPlatformRevision int64
	Decision                 []byte
}

// RecordScaling appends the event (when Action is set) and updates the
// persisted cooldown/idle state for the Deployment, in one transaction so the
// audit trail and the state it justifies cannot disagree.
func (s *RMStore) RecordScaling(ctx context.Context, r ScalingRecord) error {
	if r.Mode != "apply" {
		r.Mode = "plan"
	}
	if len(r.Detail) == 0 {
		r.Detail = []byte("{}")
	}
	if len(r.Decision) == 0 {
		r.Decision = []byte("{}")
	}
	if !json.Valid(r.Detail) || !json.Valid(r.Decision) {
		return fmt.Errorf("%w: scaling record detail/decision must be JSON", ErrInvalidClusterReport)
	}
	switch r.ScaledDirection {
	case "", "out", "in":
	default:
		return fmt.Errorf("%w: scaled direction %q must be out or in", ErrInvalidClusterReport, r.ScaledDirection)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM cluster_namespace_bindings
			WHERE cluster_id = $1 AND kubernetes_namespace = $2 AND worker_deployment = $3)`,
			r.ClusterID, r.KubernetesNamespace, r.WorkerDeployment).Scan(&bound); err != nil {
			return fmt.Errorf("store: check binding: %w", err)
		}
		if !bound {
			return fmt.Errorf("%w: %s/%s", ErrBindingNotFound, r.KubernetesNamespace, r.WorkerDeployment)
		}
		if r.Action != "" {
			if _, err := tx.Exec(ctx, `
				INSERT INTO cluster_scaling_events (cluster_id, kubernetes_namespace, worker_deployment, action, mode,
				       applied, from_replicas, to_replicas, reason, actor, detail)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
				r.ClusterID, r.KubernetesNamespace, r.WorkerDeployment, r.Action, r.Mode, r.Applied,
				r.FromReplicas, r.ToReplicas, r.Reason, r.Actor, r.Detail); err != nil {
				return fmt.Errorf("store: append scaling event: %w", err)
			}
		}
		var idle *time.Time
		if r.IdleUpdate == "set" {
			t := r.IdleSince
			idle = &t
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO cluster_scaling_state (cluster_id, kubernetes_namespace, worker_deployment, last_scale_at,
			       last_scale_direction, idle_since, observed_platform_revision, last_decision, updated_at)
			VALUES ($1,$2,$3, CASE WHEN $4 <> '' THEN now() END, $4, $5, $6, $7, now())
			ON CONFLICT (cluster_id, kubernetes_namespace, worker_deployment) DO UPDATE SET
			       last_scale_at = CASE WHEN $4 <> '' THEN now() ELSE cluster_scaling_state.last_scale_at END,
			       last_scale_direction = CASE WHEN $4 <> '' THEN $4 ELSE cluster_scaling_state.last_scale_direction END,
			       idle_since = CASE $8 WHEN 'set' THEN COALESCE(cluster_scaling_state.idle_since, $5)
			                            WHEN 'clear' THEN NULL
			                            ELSE cluster_scaling_state.idle_since END,
			       observed_platform_revision = GREATEST(cluster_scaling_state.observed_platform_revision, $6),
			       last_decision = $7,
			       updated_at = now()`,
			r.ClusterID, r.KubernetesNamespace, r.WorkerDeployment, r.ScaledDirection, idle,
			r.ObservedPlatformRevision, r.Decision, r.IdleUpdate); err != nil {
			return fmt.Errorf("store: upsert scaling state: %w", err)
		}
		return nil
	})
}

// maxRetainedReconcileErrors bounds the error table per cluster: a controller
// failing every 15s for a week must not grow it without limit.
const maxRetainedReconcileErrors = 500

// ReportReconcileError appends an error, prunes old ones, and mirrors the
// latest onto cluster_status so the console shows it without a scan.
func (s *RMStore) ReportReconcileError(ctx context.Context, clusterID, target, message, actor string) error {
	if len(message) > 4096 {
		message = message[:4096]
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM clusters WHERE id = $1)`, clusterID).Scan(&exists); err != nil {
			return fmt.Errorf("store: load cluster %s: %w", clusterID, err)
		}
		if !exists {
			return ErrClusterNotFound
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cluster_reconcile_errors (cluster_id, target, message, actor) VALUES ($1,$2,$3,$4)`,
			clusterID, target, message, actor); err != nil {
			return fmt.Errorf("store: append reconcile error: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM cluster_reconcile_errors WHERE cluster_id = $1 AND id NOT IN (
			    SELECT id FROM cluster_reconcile_errors WHERE cluster_id = $1 ORDER BY id DESC LIMIT $2)`,
			clusterID, maxRetainedReconcileErrors); err != nil {
			return fmt.Errorf("store: prune reconcile errors: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO cluster_status (cluster_id, last_error, last_error_at) VALUES ($1, $2, now())
			ON CONFLICT (cluster_id) DO UPDATE SET last_error = EXCLUDED.last_error, last_error_at = now()`,
			clusterID, target+": "+message); err != nil {
			return fmt.Errorf("store: mirror last error: %w", err)
		}
		return nil
	})
}
