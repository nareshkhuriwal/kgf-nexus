// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package store

// Cluster registry for the API GATEWAY (migration 0018).
//
// The gateway OWNS the desired-state tables written here (clusters,
// cluster_namespace_bindings): they are operator intent, like platform_config.
// Everything observed about a cluster is owned by the resource manager and is
// only READ here, to build the console's view. See rm_cluster.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
)

var (
	// ErrClusterConflict covers a stale revision, a name registered elsewhere,
	// and a pool or Deployment already bound -- all "reload and retry" cases.
	ErrClusterConflict = errors.New("cluster registry changed or the name/pool is already bound; reload before saving")
	// ErrClusterMissing is a registry miss within the caller's namespace. A
	// cluster registered by another namespace is reported the same way, so the
	// API is not an oracle for other tenants' cluster names.
	ErrClusterMissing = errors.New("cluster not found in this namespace")
	// ErrClusterInvalid wraps a validation failure.
	ErrClusterInvalid = errors.New("invalid cluster registration")
)

// ClusterBindingSpec is one worker Deployment the controller may scale.
type ClusterBindingSpec struct {
	KubernetesNamespace      string `json:"kubernetes_namespace"`
	WorkerDeployment         string `json:"worker_deployment"`
	Pool                     string `json:"pool"`
	MaxReplicas              int    `json:"max_replicas"`
	MaxScaleDownStep         int    `json:"max_scale_down_step"`
	ScaleUpCooldownSeconds   int    `json:"scale_up_cooldown_seconds"`
	ScaleDownCooldownSeconds int    `json:"scale_down_cooldown_seconds"`
}

// ClusterSpec is the operator-editable registry entry.
type ClusterSpec struct {
	Name                  string               `json:"name"`
	Provider              string               `json:"provider"`
	ProviderResourceID    string               `json:"provider_resource_id"`
	ExpectedClusterUID    string               `json:"expected_cluster_uid"`
	ControlPlaneNamespace string               `json:"control_plane_namespace"`
	Mode                  string               `json:"mode"`
	DesiredRevision       int64                `json:"desired_revision"`
	Bindings              []ClusterBindingSpec `json:"bindings"`
}

var (
	dnsLabel      = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	dnsSubdomain  = regexp.MustCompile(`^[a-z0-9]([-.a-z0-9]*[a-z0-9])?$`)
	azureResource = regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.ContainerService/managedClusters/[^/]+$`)
	awsClusterARN = regexp.MustCompile(`^arn:aws[a-z-]*:eks:[a-z0-9-]+:\d{12}:cluster/[A-Za-z0-9_-]+$`)
)

// Validate mirrors the 0018 CHECK constraints (so the operator gets a legible
// 400 instead of a constraint name) and cross-checks pools against the
// namespace's current platform policy.
func (s *ClusterSpec) Validate(cfg platform.Config) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s", ErrClusterInvalid, fmt.Sprintf(format, a...))
	}
	if s.ControlPlaneNamespace == "" {
		s.ControlPlaneNamespace = "kgf-nexus"
	}
	if s.Mode == "" {
		s.Mode = "plan"
	}
	switch {
	case len(s.Name) > 63 || !dnsLabel.MatchString(s.Name):
		return bad("cluster name must be a DNS label")
	case s.Provider != "aks" && s.Provider != "eks" && s.Provider != "kubernetes":
		return bad("provider must be aks, eks or kubernetes")
	case s.Mode != "plan" && s.Mode != "apply":
		return bad("mode must be plan or apply")
	case len(s.ExpectedClusterUID) > 64:
		return bad("expected cluster UID is too long")
	case s.Mode == "apply" && strings.TrimSpace(s.ExpectedClusterUID) == "":
		// Apply against an unverified cluster identity is the one mistake no
		// bound or drain protects against.
		return bad("apply mode requires the expected cluster UID (kubectl get namespace kube-system -o jsonpath='{.metadata.uid}')")
	case len(s.ControlPlaneNamespace) > 63 || !dnsLabel.MatchString(s.ControlPlaneNamespace):
		return bad("control plane namespace must be a DNS label")
	case len(s.ProviderResourceID) > 1024:
		return bad("provider resource ID is too long")
	case s.Provider == "aks" && s.ProviderResourceID != "" && !azureResource.MatchString(s.ProviderResourceID):
		return bad("an AKS provider resource ID must be the managedClusters ARM ID")
	case s.Provider == "eks" && s.ProviderResourceID != "" && !awsClusterARN.MatchString(s.ProviderResourceID):
		return bad("an EKS provider resource ID must be the cluster ARN")
	case len(s.Bindings) > 50:
		return bad("at most 50 bindings per cluster")
	}
	pools := map[string]bool{}
	for _, p := range cfg.Pools {
		pools[p.Name] = true
	}
	targets, bound := map[string]bool{}, map[string]bool{}
	for i := range s.Bindings {
		b := &s.Bindings[i]
		if b.MaxReplicas == 0 && b.MaxScaleDownStep == 0 {
			// An omitted numeric block gets the schema's defaults.
			b.MaxReplicas, b.MaxScaleDownStep, b.ScaleUpCooldownSeconds, b.ScaleDownCooldownSeconds = 10, 1, 30, 300
		}
		key := b.KubernetesNamespace + "/" + b.WorkerDeployment
		switch {
		case len(b.KubernetesNamespace) > 63 || !dnsLabel.MatchString(b.KubernetesNamespace):
			return bad("binding %d: Kubernetes namespace must be a DNS label", i+1)
		case len(b.WorkerDeployment) > 253 || !dnsSubdomain.MatchString(b.WorkerDeployment):
			return bad("binding %d: worker Deployment must be a DNS subdomain", i+1)
		case !pools[b.Pool]:
			return bad("binding %d: pool %q is not defined in this namespace's platform configuration", i+1, b.Pool)
		case targets[key]:
			return bad("binding %d: %s is bound twice", i+1, key)
		case bound[b.Pool]:
			return bad("binding %d: pool %s is bound twice; one pool is scaled by one Deployment", i+1, b.Pool)
		case b.MaxReplicas < 0 || b.MaxReplicas > 10000:
			return bad("binding %d: max replicas must be 0-10000", i+1)
		case b.MaxScaleDownStep < 1 || b.MaxScaleDownStep > 100:
			return bad("binding %d: scale-down step must be 1-100", i+1)
		case b.ScaleUpCooldownSeconds < 0 || b.ScaleUpCooldownSeconds > 3600:
			return bad("binding %d: scale-up cooldown must be 0-3600 seconds", i+1)
		case b.ScaleDownCooldownSeconds < 60 || b.ScaleDownCooldownSeconds > 86400:
			return bad("binding %d: scale-down cooldown must be 60-86400 seconds", i+1)
		}
		targets[key], bound[b.Pool] = true, true
	}
	return nil
}

// GatewayClusterPut creates (DesiredRevision 0) or updates (DesiredRevision =
// the revision the operator loaded) a cluster and replaces its bindings, in one
// transaction with its audit entry. Every accepted write bumps
// desired_revision, which the controller acknowledges once reconciled.
func GatewayClusterPut(ctx context.Context, db GWDB, ns, actor string, spec ClusterSpec, audit AuditEntry) (ClusterSpec, error) {
	cfg, err := GatewayPlatformGet(ctx, db, ns)
	if err != nil {
		return spec, fmt.Errorf("load platform config: %w", err)
	}
	if err := spec.Validate(cfg); err != nil {
		return spec, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return spec, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var id string
	prev := spec.DesiredRevision
	if prev == 0 {
		err = tx.QueryRow(ctx, `
			INSERT INTO clusters (namespace, name, provider, provider_resource_id, expected_cluster_uid,
			       control_plane_namespace, mode, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT (name) DO NOTHING
			RETURNING id, desired_revision`,
			ns, spec.Name, spec.Provider, spec.ProviderResourceID, spec.ExpectedClusterUID,
			spec.ControlPlaneNamespace, spec.Mode, actor).Scan(&id, &spec.DesiredRevision)
	} else {
		err = tx.QueryRow(ctx, `
			UPDATE clusters SET provider = $3, provider_resource_id = $4, expected_cluster_uid = $5,
			       control_plane_namespace = $6, mode = $7, updated_by = $8, updated_at = now(),
			       desired_revision = desired_revision + 1
			 WHERE namespace = $1 AND name = $2 AND desired_revision = $9
			RETURNING id, desired_revision`,
			ns, spec.Name, spec.Provider, spec.ProviderResourceID, spec.ExpectedClusterUID,
			spec.ControlPlaneNamespace, spec.Mode, actor, prev).Scan(&id, &spec.DesiredRevision)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return spec, ErrClusterConflict
	}
	if err != nil {
		return spec, fmt.Errorf("write cluster: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM cluster_namespace_bindings WHERE cluster_id = $1`, id); err != nil {
		return spec, fmt.Errorf("replace bindings: %w", err)
	}
	for _, b := range spec.Bindings {
		if _, err := tx.Exec(ctx, `
			INSERT INTO cluster_namespace_bindings (cluster_id, namespace, kubernetes_namespace, worker_deployment, pool,
			       max_replicas, max_scale_down_step, scale_up_cooldown_seconds, scale_down_cooldown_seconds)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			id, ns, b.KubernetesNamespace, b.WorkerDeployment, b.Pool, b.MaxReplicas, b.MaxScaleDownStep,
			b.ScaleUpCooldownSeconds, b.ScaleDownCooldownSeconds); err != nil {
			if isUniqueViolation(err) {
				return spec, fmt.Errorf("%w: pool %s or %s/%s is already bound", ErrClusterConflict, b.Pool, b.KubernetesNamespace, b.WorkerDeployment)
			}
			return spec, fmt.Errorf("insert binding: %w", err)
		}
	}
	audit.Action = "platform.cluster.update"
	if prev == 0 {
		audit.Action = "platform.cluster.register"
	}
	audit.TargetKind, audit.TargetID, audit.Namespace, audit.Outcome = "cluster", spec.Name, ns, AuditSuccess
	audit.Detail = map[string]any{"revision": spec.DesiredRevision, "mode": spec.Mode, "bindings": len(spec.Bindings),
		"expected_cluster_uid_set": spec.ExpectedClusterUID != ""}
	if err := GatewayAppendAudit(ctx, tx, audit); err != nil {
		return spec, err
	}
	if err := tx.Commit(ctx); err != nil {
		return spec, fmt.Errorf("commit: %w", err)
	}
	return spec, nil
}

// GatewayClusterDelete unregisters a cluster. Refused while in apply mode, so
// a live mutating controller is always switched to plan (and acknowledged)
// before it loses its registry entry. The observed rows cascade with it.
func GatewayClusterDelete(ctx context.Context, db GWDB, ns, name string, revision int64, audit AuditEntry) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var mode string
	err = tx.QueryRow(ctx, `SELECT mode FROM clusters WHERE namespace = $1 AND name = $2 FOR UPDATE`, ns, name).Scan(&mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrClusterMissing
	}
	if err != nil {
		return fmt.Errorf("load cluster: %w", err)
	}
	if mode == "apply" {
		return fmt.Errorf("%w: switch the cluster to plan mode before unregistering it", ErrClusterInvalid)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM clusters WHERE namespace = $1 AND name = $2 AND desired_revision = $3`, ns, name, revision)
	if err != nil {
		return fmt.Errorf("delete cluster: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrClusterConflict
	}
	audit.Action, audit.TargetKind, audit.TargetID, audit.Namespace, audit.Outcome = "platform.cluster.delete", "cluster", name, ns, AuditSuccess
	audit.Detail = map[string]any{"revision": revision}
	if err := GatewayAppendAudit(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ── console read model ──────────────────────────────────────────────────────

// ClusterStatusView is the controller's latest self-report; nil when the
// controller has never reported (the console says "no controller connected").
type ClusterStatusView struct {
	ObservedRevision    int64      `json:"observed_revision"`
	ObservedClusterUID  string     `json:"observed_cluster_uid"`
	IdentityVerified    bool       `json:"identity_verified"`
	KubernetesVersion   string     `json:"kubernetes_version"`
	ControllerIdentity  string     `json:"controller_identity"`
	EffectiveMode       string     `json:"effective_mode"`
	EffectiveModeReason string     `json:"effective_mode_reason"`
	LastReportAt        *time.Time `json:"last_report_at"`
	LastError           string     `json:"last_error"`
	LastErrorAt         *time.Time `json:"last_error_at"`
}

// ObservationView is one reported Kubernetes object.
type ObservationView struct {
	Kind               string          `json:"kind"`
	Namespace          string          `json:"namespace"`
	Name               string          `json:"name"`
	Generation         int64           `json:"generation"`
	ObservedGeneration int64           `json:"observed_generation"`
	SpecReplicas       *int32          `json:"spec_replicas"`
	ReadyReplicas      *int32          `json:"ready_replicas"`
	Ready              bool            `json:"ready"`
	Summary            json.RawMessage `json:"summary"`
	CreatedAt          *time.Time      `json:"created_at"`
	ObservedAt         time.Time       `json:"observed_at"`
}

// ScalingStateView is the controller's persisted memory per Deployment.
type ScalingStateView struct {
	KubernetesNamespace      string          `json:"kubernetes_namespace"`
	WorkerDeployment         string          `json:"worker_deployment"`
	LastScaleAt              *time.Time      `json:"last_scale_at"`
	LastScaleDirection       string          `json:"last_scale_direction"`
	IdleSince                *time.Time      `json:"idle_since"`
	ObservedPlatformRevision int64           `json:"observed_platform_revision"`
	LastDecision             json.RawMessage `json:"last_decision"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

// ScalingEventView is one audit row.
type ScalingEventView struct {
	ID                  int64     `json:"id"`
	KubernetesNamespace string    `json:"kubernetes_namespace"`
	WorkerDeployment    string    `json:"worker_deployment"`
	Action              string    `json:"action"`
	Mode                string    `json:"mode"`
	Applied             bool      `json:"applied"`
	FromReplicas        *int32    `json:"from_replicas"`
	ToReplicas          *int32    `json:"to_replicas"`
	Reason              string    `json:"reason"`
	Actor               string    `json:"actor"`
	OccurredAt          time.Time `json:"occurred_at"`
}

// ReconcileErrorView is one controller failure.
type ReconcileErrorView struct {
	Target     string    `json:"target"`
	Message    string    `json:"message"`
	Actor      string    `json:"actor"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ClusterView is everything the console shows for one cluster.
type ClusterView struct {
	ID string `json:"id"`
	ClusterSpec
	CreatedAt    time.Time            `json:"created_at"`
	UpdatedAt    time.Time            `json:"updated_at"`
	UpdatedBy    string               `json:"updated_by"`
	Status       *ClusterStatusView   `json:"status"`
	Observations []ObservationView    `json:"observations"`
	Scaling      []ScalingStateView   `json:"scaling"`
	Events       []ScalingEventView   `json:"events"`
	Errors       []ReconcileErrorView `json:"errors"`
	// JobsShown counts the task Jobs included; the view carries at most
	// maxJobsInView, newest first, even when the controller reported more.
	JobsShown int `json:"jobs_shown"`
}

// maxJobsInView bounds the console payload; the controller may report more.
const maxJobsInView = 100

// GatewayClusterList returns the namespace's clusters with everything observed.
func GatewayClusterList(ctx context.Context, q GWQuerier, ns string) ([]ClusterView, error) {
	rows, err := q.Query(ctx, `
		SELECT id, name, provider, provider_resource_id, expected_cluster_uid, control_plane_namespace, mode,
		       desired_revision, created_at, updated_at, updated_by
		  FROM clusters WHERE namespace = $1 ORDER BY name`, ns)
	if err != nil {
		return nil, fmt.Errorf("list clusters: %w", err)
	}
	var out []ClusterView
	for rows.Next() {
		var c ClusterView
		if err := rows.Scan(&c.ID, &c.Name, &c.Provider, &c.ProviderResourceID, &c.ExpectedClusterUID,
			&c.ControlPlaneNamespace, &c.Mode, &c.DesiredRevision, &c.CreatedAt, &c.UpdatedAt, &c.UpdatedBy); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan cluster: %w", err)
		}
		c.Bindings, c.Observations, c.Scaling, c.Events, c.Errors =
			[]ClusterBindingSpec{}, []ObservationView{}, []ScalingStateView{}, []ScalingEventView{}, []ReconcileErrorView{}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clusters: %w", err)
	}
	for i := range out {
		if err := fillClusterView(ctx, q, &out[i]); err != nil {
			return nil, err
		}
	}
	if out == nil {
		out = []ClusterView{}
	}
	return out, nil
}

func fillClusterView(ctx context.Context, q GWQuerier, c *ClusterView) error {
	rows, err := q.Query(ctx, `
		SELECT kubernetes_namespace, worker_deployment, pool, max_replicas, max_scale_down_step,
		       scale_up_cooldown_seconds, scale_down_cooldown_seconds
		  FROM cluster_namespace_bindings WHERE cluster_id = $1 ORDER BY kubernetes_namespace, worker_deployment`, c.ID)
	if err != nil {
		return fmt.Errorf("list bindings: %w", err)
	}
	for rows.Next() {
		var b ClusterBindingSpec
		if err := rows.Scan(&b.KubernetesNamespace, &b.WorkerDeployment, &b.Pool, &b.MaxReplicas, &b.MaxScaleDownStep,
			&b.ScaleUpCooldownSeconds, &b.ScaleDownCooldownSeconds); err != nil {
			rows.Close()
			return fmt.Errorf("scan binding: %w", err)
		}
		c.Bindings = append(c.Bindings, b)
	}
	rows.Close()

	var st ClusterStatusView
	err = q.QueryRow(ctx, `
		SELECT observed_revision, observed_cluster_uid, kubernetes_version, controller_identity, effective_mode,
		       effective_mode_reason, last_report_at, last_error, last_error_at
		  FROM cluster_status WHERE cluster_id = $1`, c.ID).Scan(
		&st.ObservedRevision, &st.ObservedClusterUID, &st.KubernetesVersion, &st.ControllerIdentity,
		&st.EffectiveMode, &st.EffectiveModeReason, &st.LastReportAt, &st.LastError, &st.LastErrorAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return fmt.Errorf("load cluster status: %w", err)
	default:
		st.IdentityVerified = c.ExpectedClusterUID != "" && st.ObservedClusterUID == c.ExpectedClusterUID
		c.Status = &st
	}

	// Jobs are bounded to the newest; every other kind is small by nature.
	rows, err = q.Query(ctx, `
		(SELECT kind, kubernetes_namespace, name, generation, observed_generation, spec_replicas, ready_replicas,
		        ready, summary, object_created_at, observed_at
		   FROM cluster_observations WHERE cluster_id = $1 AND kind <> 'Job')
		UNION ALL
		(SELECT kind, kubernetes_namespace, name, generation, observed_generation, spec_replicas, ready_replicas,
		        ready, summary, object_created_at, observed_at
		   FROM cluster_observations WHERE cluster_id = $1 AND kind = 'Job'
		  ORDER BY object_created_at DESC NULLS LAST LIMIT $2)
		ORDER BY kind, kubernetes_namespace, name`, c.ID, maxJobsInView)
	if err != nil {
		return fmt.Errorf("list observations: %w", err)
	}
	for rows.Next() {
		var o ObservationView
		var summary []byte
		if err := rows.Scan(&o.Kind, &o.Namespace, &o.Name, &o.Generation, &o.ObservedGeneration, &o.SpecReplicas,
			&o.ReadyReplicas, &o.Ready, &summary, &o.CreatedAt, &o.ObservedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan observation: %w", err)
		}
		o.Summary = summary
		if o.Kind == "Job" {
			c.JobsShown++
		}
		c.Observations = append(c.Observations, o)
	}
	rows.Close()

	rows, err = q.Query(ctx, `
		SELECT kubernetes_namespace, worker_deployment, last_scale_at, last_scale_direction, idle_since,
		       observed_platform_revision, last_decision, updated_at
		  FROM cluster_scaling_state WHERE cluster_id = $1 ORDER BY kubernetes_namespace, worker_deployment`, c.ID)
	if err != nil {
		return fmt.Errorf("list scaling state: %w", err)
	}
	for rows.Next() {
		var s ScalingStateView
		var decision []byte
		if err := rows.Scan(&s.KubernetesNamespace, &s.WorkerDeployment, &s.LastScaleAt, &s.LastScaleDirection,
			&s.IdleSince, &s.ObservedPlatformRevision, &decision, &s.UpdatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan scaling state: %w", err)
		}
		s.LastDecision = decision
		c.Scaling = append(c.Scaling, s)
	}
	rows.Close()

	rows, err = q.Query(ctx, `
		SELECT id, kubernetes_namespace, worker_deployment, action, mode, applied, from_replicas, to_replicas,
		       reason, actor, occurred_at
		  FROM cluster_scaling_events WHERE cluster_id = $1 ORDER BY id DESC LIMIT 50`, c.ID)
	if err != nil {
		return fmt.Errorf("list scaling events: %w", err)
	}
	for rows.Next() {
		var e ScalingEventView
		if err := rows.Scan(&e.ID, &e.KubernetesNamespace, &e.WorkerDeployment, &e.Action, &e.Mode, &e.Applied,
			&e.FromReplicas, &e.ToReplicas, &e.Reason, &e.Actor, &e.OccurredAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan scaling event: %w", err)
		}
		c.Events = append(c.Events, e)
	}
	rows.Close()

	rows, err = q.Query(ctx, `
		SELECT target, message, actor, occurred_at FROM cluster_reconcile_errors
		 WHERE cluster_id = $1 ORDER BY id DESC LIMIT 20`, c.ID)
	if err != nil {
		return fmt.Errorf("list reconcile errors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e ReconcileErrorView
		if err := rows.Scan(&e.Target, &e.Message, &e.Actor, &e.OccurredAt); err != nil {
			return fmt.Errorf("scan reconcile error: %w", err)
		}
		c.Errors = append(c.Errors, e)
	}
	return rows.Err()
}
