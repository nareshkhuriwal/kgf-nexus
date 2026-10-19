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
	"sort"
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

// ScalingEventView is one audit row. A scheduler HPA's events (ADR-0036)
// carry worker_deployment "hpa/<name>" and an hpa_* action, with the settings
// before and after in detail.
type ScalingEventView struct {
	ID                  int64           `json:"id"`
	KubernetesNamespace string          `json:"kubernetes_namespace"`
	WorkerDeployment    string          `json:"worker_deployment"`
	Action              string          `json:"action"`
	Mode                string          `json:"mode"`
	Applied             bool            `json:"applied"`
	FromReplicas        *int32          `json:"from_replicas"`
	ToReplicas          *int32          `json:"to_replicas"`
	Reason              string          `json:"reason"`
	Actor               string          `json:"actor"`
	Detail              json.RawMessage `json:"detail"`
	OccurredAt          time.Time       `json:"occurred_at"`
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

	// Labels are the cluster's placement labels as the controller last
	// published them (ADR-0031): operator-declared over the observed
	// nexus.io/region and nexus.io/provider. {} before the first report.
	Labels map[string]string `json:"labels"`
	// Headroom is the remaining ResourceQuota room per task namespace
	// (ADR-0031 §3), keyed by Kubernetes namespace; -1 = unbounded. {} before
	// the first complete quota observation.
	Headroom map[string]ClusterHeadroom `json:"headroom"`
	// HeadroomObservedAt is when Headroom was observed; null if never.
	HeadroomObservedAt *time.Time `json:"headroom_observed_at"`
	// SchedulerAutoscaling compares the namespace's saved scheduler settings
	// with the scheduler HPAs the controller observed (ADR-0036).
	SchedulerAutoscaling SchedulerAutoscalingView `json:"scheduler_autoscaling"`
}

// Scheduler autoscaling states (ADR-0036).
const (
	SchedulerAutoscalingNotObserved = "not_observed"
	SchedulerAutoscalingInSync      = "in_sync"
	SchedulerAutoscalingPlanOnly    = "plan_only"
	SchedulerAutoscalingApplying    = "applying"
	SchedulerAutoscalingDrift       = "drift"
)

// schedulerAutoscalingSeverity orders the states for the top-level summary.
var schedulerAutoscalingSeverity = map[string]int{
	SchedulerAutoscalingInSync: 1, SchedulerAutoscalingPlanOnly: 2, SchedulerAutoscalingApplying: 3, SchedulerAutoscalingDrift: 4,
}

// SchedulerAutoscalingDesired is the namespace's saved scheduler policy.
type SchedulerAutoscalingDesired struct {
	MinReplicas                   int `json:"min_replicas"`
	MaxReplicas                   int `json:"max_replicas"`
	TargetCPUPercent              int `json:"target_cpu_percent"`
	ScaleDownStabilizationSeconds int `json:"scale_down_stabilization_seconds"`
	// ConfigRevision is the platform_config revision; 0 = never saved (the
	// values are the defaults, and the controller applies nothing).
	ConfigRevision int64 `json:"config_revision"`
}

// SchedulerAutoscalingObserved is one scheduler HPA as last reported.
type SchedulerAutoscalingObserved struct {
	MinReplicas int `json:"min_replicas"`
	MaxReplicas int `json:"max_replicas"`
	// TargetCPUPercent is null when the HPA has no CPU utilization metric.
	TargetCPUPercent *int `json:"target_cpu_percent"`
	// ScaleDownStabilizationSeconds is the effective window (Kubernetes' 300
	// when the HPA sets none); null from a controller that predates ADR-0036.
	ScaleDownStabilizationSeconds *int   `json:"scale_down_stabilization_seconds"`
	CurrentReplicas               int    `json:"current_replicas"`
	DesiredReplicas               int    `json:"desired_replicas"`
	HPAName                       string `json:"hpa_name"`
	Deployment                    string `json:"deployment"`
}

// SchedulerAutoscalingTarget is one scheduler HPA's comparison.
type SchedulerAutoscalingTarget struct {
	Observed SchedulerAutoscalingObserved `json:"observed"`
	State    string                       `json:"state"`
	// Managed: the HPA carries the controller-ownership annotation, so apply
	// mode may change its settings.
	Managed bool `json:"managed"`
	// Reason is the controller's one-line account of its last reconcile.
	Reason        string     `json:"reason"`
	LastAppliedAt *time.Time `json:"last_applied_at"`
	LastError     *string    `json:"last_error"`
}

// SchedulerAutoscalingView is the console's scheduler autoscaling panel. The
// top-level observed/state/last_applied_at/last_error describe the scheduler
// HPA in the worst state (drift, then applying, plan_only, in_sync; ties by
// name); targets lists every scheduler HPA.
type SchedulerAutoscalingView struct {
	Desired       SchedulerAutoscalingDesired   `json:"desired"`
	Observed      *SchedulerAutoscalingObserved `json:"observed"`
	State         string                        `json:"state"`
	LastAppliedAt *time.Time                    `json:"last_applied_at"`
	LastError     *string                       `json:"last_error"`
	Targets       []SchedulerAutoscalingTarget  `json:"targets"`
}

// schedulerHPAState is a scheduler HPA's controller memory
// (cluster_scaling_state, target hpa/<name>).
type schedulerHPAState struct {
	Status        SchedulerHPAStatus
	LastAppliedAt *time.Time
}

// hpaSummary is the part of a reported HorizontalPodAutoscaler summary the
// comparison reads (cluster-controller observe.go writes it).
type hpaSummary struct {
	MinReplicas                   *int   `json:"min_replicas"`
	MaxReplicas                   *int   `json:"max_replicas"`
	TargetCPUPercent              *int   `json:"target_cpu_percent"`
	ScaleDownStabilizationSeconds *int   `json:"scale_down_stabilization_seconds"`
	ScaleTarget                   string `json:"scale_target"`
	Managed                       *bool  `json:"managed"`
}

// schedulerAutoscaling builds the view: desired from the saved policy,
// observed from every HPA in the control-plane namespace whose scale target is
// a reported scheduler Deployment, and per HPA a state:
//
//	in_sync      observed equals desired
//	plan_only    they differ and the controller runs in plan mode
//	applying     they differ in apply mode, and the controller has not yet
//	             reconciled this config revision or is moving the HPA in
//	             bounded steps
//	drift        they differ in apply mode after a reconcile of this revision
//	             that could not converge (last_error says why)
//	not_observed (top level only) no scheduler HPA is reported
func schedulerAutoscaling(c *ClusterView, cfg platform.Config, states map[string]schedulerHPAState) SchedulerAutoscalingView {
	d := cfg.Scheduler.HPASettings()
	v := SchedulerAutoscalingView{
		Desired: SchedulerAutoscalingDesired{MinReplicas: int(d.MinReplicas), MaxReplicas: int(d.MaxReplicas),
			TargetCPUPercent: int(d.TargetCPUPercent), ScaleDownStabilizationSeconds: int(d.ScaleDownStabilizationSeconds),
			ConfigRevision: cfg.Revision},
		State:   SchedulerAutoscalingNotObserved,
		Targets: []SchedulerAutoscalingTarget{},
	}
	apply := c.Status != nil && c.Status.EffectiveMode == "apply"
	schedulers := map[string]bool{}
	for _, o := range c.Observations {
		if o.Kind == "SchedulerDeployment" && o.Namespace == c.ControlPlaneNamespace {
			schedulers[o.Name] = true
		}
	}
	for _, o := range c.Observations {
		if o.Kind != "HorizontalPodAutoscaler" || o.Namespace != c.ControlPlaneNamespace {
			continue
		}
		var sum hpaSummary
		if json.Unmarshal(o.Summary, &sum) != nil {
			continue
		}
		kind, name, _ := strings.Cut(sum.ScaleTarget, "/")
		if kind != "Deployment" || !schedulers[name] {
			continue
		}
		obs := SchedulerAutoscalingObserved{MinReplicas: 1, TargetCPUPercent: sum.TargetCPUPercent,
			ScaleDownStabilizationSeconds: sum.ScaleDownStabilizationSeconds, HPAName: o.Name, Deployment: name}
		if sum.MinReplicas != nil {
			obs.MinReplicas = *sum.MinReplicas
		}
		if sum.MaxReplicas != nil {
			obs.MaxReplicas = *sum.MaxReplicas
		}
		// The HPA's status, as the observation's columns carry it.
		if o.ReadyReplicas != nil {
			obs.CurrentReplicas = int(*o.ReadyReplicas)
		}
		if o.SpecReplicas != nil {
			obs.DesiredReplicas = int(*o.SpecReplicas)
		}
		t := SchedulerAutoscalingTarget{Observed: obs, Managed: sum.Managed != nil && *sum.Managed}
		st, reported := states[o.Namespace+"/"+o.Name]
		if reported {
			t.Reason, t.LastAppliedAt = st.Status.Reason, st.LastAppliedAt
		}
		inSync := obs.MinReplicas == int(d.MinReplicas) && obs.MaxReplicas == int(d.MaxReplicas) &&
			obs.TargetCPUPercent != nil && *obs.TargetCPUPercent == int(d.TargetCPUPercent) &&
			obs.ScaleDownStabilizationSeconds != nil && *obs.ScaleDownStabilizationSeconds == int(d.ScaleDownStabilizationSeconds)
		switch {
		case inSync:
			t.State = SchedulerAutoscalingInSync
		case !apply:
			t.State = SchedulerAutoscalingPlanOnly
		case !reported:
			// Observed by a controller that never reports scheduler
			// autoscaling: it predates ADR-0036 and will not converge it.
			t.State = SchedulerAutoscalingDrift
			msg := "the controller reports this HPA but not its scheduler autoscaling; upgrade the cluster controller to apply the saved settings"
			t.LastError = &msg
		case st.Status.ConfigRevision < cfg.Revision || st.Status.Result == SchedulerHPAApplying:
			t.State = SchedulerAutoscalingApplying
		default:
			t.State = SchedulerAutoscalingDrift
			msg := st.Status.Error
			if msg == "" {
				msg = st.Status.Reason
			}
			if msg == "" {
				msg = "differs from the saved scheduler settings"
			}
			t.LastError = &msg
		}
		v.Targets = append(v.Targets, t)
	}
	sort.Slice(v.Targets, func(i, j int) bool { return v.Targets[i].Observed.HPAName < v.Targets[j].Observed.HPAName })
	for i := range v.Targets {
		t := &v.Targets[i]
		if v.Observed == nil || schedulerAutoscalingSeverity[t.State] > schedulerAutoscalingSeverity[v.State] {
			obs := t.Observed
			v.Observed, v.State, v.LastAppliedAt, v.LastError = &obs, t.State, t.LastAppliedAt, t.LastError
		}
	}
	return v
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
		c.Labels, c.Headroom = map[string]string{}, map[string]ClusterHeadroom{}
		out = append(out, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate clusters: %w", err)
	}
	if len(out) == 0 {
		return []ClusterView{}, nil
	}
	// The saved scheduler policy every registered cluster of the namespace is
	// compared with (ADR-0036).
	cfg, err := GatewayPlatformGet(ctx, q, ns)
	if err != nil {
		return nil, fmt.Errorf("load platform config: %w", err)
	}
	for i := range out {
		states, err := fillClusterView(ctx, q, &out[i])
		if err != nil {
			return nil, err
		}
		out[i].SchedulerAutoscaling = schedulerAutoscaling(&out[i], cfg, states)
	}
	return out, nil
}

// fillClusterView loads one cluster's bindings and everything observed, and
// returns the scheduler HPAs' controller memory, keyed "<namespace>/<hpa>".
func fillClusterView(ctx context.Context, q GWQuerier, c *ClusterView) (map[string]schedulerHPAState, error) {
	rows, err := q.Query(ctx, `
		SELECT kubernetes_namespace, worker_deployment, pool, max_replicas, max_scale_down_step,
		       scale_up_cooldown_seconds, scale_down_cooldown_seconds
		  FROM cluster_namespace_bindings WHERE cluster_id = $1 ORDER BY kubernetes_namespace, worker_deployment`, c.ID)
	if err != nil {
		return nil, fmt.Errorf("list bindings: %w", err)
	}
	for rows.Next() {
		var b ClusterBindingSpec
		if err := rows.Scan(&b.KubernetesNamespace, &b.WorkerDeployment, &b.Pool, &b.MaxReplicas, &b.MaxScaleDownStep,
			&b.ScaleUpCooldownSeconds, &b.ScaleDownCooldownSeconds); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan binding: %w", err)
		}
		c.Bindings = append(c.Bindings, b)
	}
	rows.Close()

	var st ClusterStatusView
	var labels, headroom []byte
	err = q.QueryRow(ctx, `
		SELECT observed_revision, observed_cluster_uid, kubernetes_version, controller_identity, effective_mode,
		       effective_mode_reason, last_report_at, last_error, last_error_at, labels, headroom, headroom_observed_at
		  FROM cluster_status WHERE cluster_id = $1`, c.ID).Scan(
		&st.ObservedRevision, &st.ObservedClusterUID, &st.KubernetesVersion, &st.ControllerIdentity,
		&st.EffectiveMode, &st.EffectiveModeReason, &st.LastReportAt, &st.LastError, &st.LastErrorAt,
		&labels, &headroom, &c.HeadroomObservedAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, fmt.Errorf("load cluster status: %w", err)
	default:
		st.IdentityVerified = c.ExpectedClusterUID != "" && st.ObservedClusterUID == c.ExpectedClusterUID
		c.Status = &st
		// Written by the resource manager from a validated report; a document
		// that does not decode is shown as empty rather than failing the page.
		if json.Unmarshal(labels, &c.Labels) != nil || c.Labels == nil {
			c.Labels = map[string]string{}
		}
		if json.Unmarshal(headroom, &c.Headroom) != nil || c.Headroom == nil {
			c.Headroom = map[string]ClusterHeadroom{}
		}
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
		return nil, fmt.Errorf("list observations: %w", err)
	}
	for rows.Next() {
		var o ObservationView
		var summary []byte
		if err := rows.Scan(&o.Kind, &o.Namespace, &o.Name, &o.Generation, &o.ObservedGeneration, &o.SpecReplicas,
			&o.ReadyReplicas, &o.Ready, &summary, &o.CreatedAt, &o.ObservedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan observation: %w", err)
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
		return nil, fmt.Errorf("list scaling state: %w", err)
	}
	states := map[string]schedulerHPAState{}
	for rows.Next() {
		var s ScalingStateView
		var decision []byte
		if err := rows.Scan(&s.KubernetesNamespace, &s.WorkerDeployment, &s.LastScaleAt, &s.LastScaleDirection,
			&s.IdleSince, &s.ObservedPlatformRevision, &decision, &s.UpdatedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan scaling state: %w", err)
		}
		if hpa, ok := strings.CutPrefix(s.WorkerDeployment, SchedulerHPATargetPrefix); ok {
			// A scheduler HPA's memory: shown under scheduler_autoscaling,
			// not as a worker Deployment.
			st := schedulerHPAState{LastAppliedAt: s.LastScaleAt}
			_ = json.Unmarshal(decision, &st.Status)
			states[s.KubernetesNamespace+"/"+hpa] = st
			continue
		}
		s.LastDecision = decision
		c.Scaling = append(c.Scaling, s)
	}
	rows.Close()

	rows, err = q.Query(ctx, `
		SELECT id, kubernetes_namespace, worker_deployment, action, mode, applied, from_replicas, to_replicas,
		       reason, actor, detail, occurred_at
		  FROM cluster_scaling_events WHERE cluster_id = $1 ORDER BY id DESC LIMIT 50`, c.ID)
	if err != nil {
		return nil, fmt.Errorf("list scaling events: %w", err)
	}
	for rows.Next() {
		var e ScalingEventView
		var detail []byte
		if err := rows.Scan(&e.ID, &e.KubernetesNamespace, &e.WorkerDeployment, &e.Action, &e.Mode, &e.Applied,
			&e.FromReplicas, &e.ToReplicas, &e.Reason, &e.Actor, &detail, &e.OccurredAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan scaling event: %w", err)
		}
		e.Detail = detail
		c.Events = append(c.Events, e)
	}
	rows.Close()

	rows, err = q.Query(ctx, `
		SELECT target, message, actor, occurred_at FROM cluster_reconcile_errors
		 WHERE cluster_id = $1 ORDER BY id DESC LIMIT 20`, c.ID)
	if err != nil {
		return nil, fmt.Errorf("list reconcile errors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e ReconcileErrorView
		if err := rows.Scan(&e.Target, &e.Message, &e.Actor, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("scan reconcile error: %w", err)
		}
		c.Errors = append(c.Errors, e)
	}
	return states, rows.Err()
}
