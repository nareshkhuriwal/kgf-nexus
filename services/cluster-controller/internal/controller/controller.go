// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package controller is the in-cluster worker-scaling controller
// (docs/CLUSTER-SCALING.md).
//
// Deliberately narrow. It does exactly three things per cycle:
//
//  1. OBSERVE: report scheduler/worker Deployments, Nodes, task Jobs and the
//     scheduler HPA -- with generation, observedGeneration and timestamps -- to
//     the resource manager, which owns the observed-cluster tables.
//  2. PLAN: turn queued demand into a bounded desired worker count with the
//     same platform.Plan the console previews, and compare each scheduler
//     HPA's settings with the namespace's saved scheduler policy.
//  3. APPLY (only when explicitly enabled per cluster AND in this process):
//     scale worker Deployments out, and scale them in only after draining the
//     chosen workers and confirming they hold no leases; move an opted-in
//     scheduler HPA's four settings toward the saved policy (ADR-0036).
//
// It does NOT scale the scheduler (its HPA does, on CPU; the controller only
// sets that HPA's bounds and target), and it does NOT scale nodes (the AKS
// cluster autoscaler does). Two controllers writing one target fight; each
// target here has exactly one writer.
package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Config is the controller's process-level configuration.
type Config struct {
	// ClusterName selects the registry entry (clusters.name).
	ClusterName string
	// Identity is this replica's name (the pod name in-cluster). Used as the
	// leader-election identity and as the actor on every recorded event.
	Identity string
	// AllowApply is the process-level half of the apply gate. The registry's
	// mode is the other half; both must say apply. A controller deployed
	// without it can never mutate the cluster, whatever the database says.
	AllowApply bool
	// Interval between reconcile cycles.
	Interval time.Duration
	// RPCTimeout bounds every call to the resource manager and the API server,
	// so one hung dependency cannot stall the loop indefinitely.
	RPCTimeout time.Duration
	// JobLabelSelector selects task Jobs to report (default: the kubernetes
	// executor's nexus.dev/attempt label). Jobs are reported, never created or
	// deleted here.
	JobLabelSelector string
	// MaxJobsReported caps Jobs per report; the rest are counted, not listed.
	MaxJobsReported int
	// Now is injectable for deterministic cooldown tests.
	Now func() time.Time

	// ClusterLabels are the operator-declared placement labels published with
	// every report (NEXUS_CLUSTER_LABELS: cost tier, data locality ...),
	// merged over the observed region and provider (ADR-0031 §1).
	ClusterLabels map[string]string
	// TaskNamespaces are where task pods run (NEXUS_CLUSTER_TASK_NAMESPACES):
	// task Jobs are observed there and their ResourceQuota headroom is
	// published. Empty means the bound worker namespaces.
	TaskNamespaces []string
	// GPUResourceNames count as gpu_count in the headroom (default
	// nvidia.com/gpu).
	GPUResourceNames []string

	// SchedulerPatchInterval is the least time between two patches of one
	// scheduler HPA (ADR-0036): with platform.NextHPAStep's bounded step, it
	// bounds how fast a saved setting resizes the scheduler fleet, and how
	// often the controller rewrites an HPA another writer keeps reverting.
	SchedulerPatchInterval time.Duration
}

func (c *Config) defaults() {
	if c.Interval <= 0 {
		c.Interval = 15 * time.Second
	}
	if c.RPCTimeout <= 0 {
		c.RPCTimeout = 10 * time.Second
	}
	if c.JobLabelSelector == "" {
		c.JobLabelSelector = DefaultJobLabelSelector
	}
	if c.SchedulerPatchInterval <= 0 {
		c.SchedulerPatchInterval = time.Minute
	}
	if c.MaxJobsReported <= 0 {
		c.MaxJobsReported = 500
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Controller reconciles one registered cluster.
type Controller struct {
	cfg  Config
	kube kubernetes.Interface
	rm   rmv1.ClusterControllerServiceClient
	log  *slog.Logger

	mu sync.Mutex
	// lastDecision suppresses duplicate plan-mode events: a pool that wants
	// three more workers every 15 seconds for an hour is one decision, not 240.
	lastDecision map[string]string
	// hpaApplied is when this process last patched each scheduler HPA
	// ("<namespace>/<name>"), in case recording it in the resource manager
	// failed: the patch interval must hold either way.
	hpaApplied map[string]time.Time

	lastSuccess atomic.Int64
	cycles      atomic.Int64

	// metrics is optional; nil records nothing.
	metrics *Metrics
}

// SetMetrics attaches the reconcile-loop instruments. Called once, before Run.
func (c *Controller) SetMetrics(m *Metrics) { c.metrics = m }

// New builds a controller.
func New(cfg Config, kube kubernetes.Interface, rm rmv1.ClusterControllerServiceClient, log *slog.Logger) (*Controller, error) {
	if cfg.ClusterName == "" {
		return nil, errors.New("controller: cluster name is required")
	}
	if cfg.Identity == "" {
		return nil, errors.New("controller: identity is required")
	}
	cfg.defaults()
	return &Controller{cfg: cfg, kube: kube, rm: rm, log: log.With("cluster", cfg.ClusterName, "identity", cfg.Identity),
		lastDecision: map[string]string{}, hpaApplied: map[string]time.Time{}}, nil
}

// LastSuccess is when a cycle last completed without error (zero if never).
func (c *Controller) LastSuccess() time.Time {
	v := c.lastSuccess.Load()
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// Run reconciles every Interval until ctx ends. A failed cycle is logged and
// retried next interval; it never exits the loop, because the failure modes
// (API server blip, resource manager restart) are transient and the safe
// response to all of them is "change nothing and try again".
func (c *Controller) Run(ctx context.Context) error {
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		if err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
			c.log.WarnContext(ctx, "reconcile cycle failed; nothing further changed this cycle", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Mode is the mode a cycle actually ran in.
type Mode struct {
	Apply  bool
	Reason string
}

func (m Mode) String() string {
	if m.Apply {
		return "apply"
	}
	return "plan"
}

// effectiveMode narrows the registry's request by the process gate and the
// identity check. Apply against the wrong cluster is the one mistake this
// controller can make that no bound or drain protects against, so the kube-system
// UID must match what the operator registered.
func (c *Controller) effectiveMode(a *rmv1.ClusterAssignment, observedUID string) Mode {
	switch {
	case a.GetMode() != "apply":
		return Mode{Reason: "registry mode is plan (dry-run)"}
	case !c.cfg.AllowApply:
		return Mode{Reason: "registry requests apply but this controller was started without NEXUS_CONTROLLER_ALLOW_APPLY"}
	case a.GetExpectedClusterUid() == "":
		return Mode{Reason: "registry requests apply but no expected cluster UID is registered"}
	case observedUID == "":
		return Mode{Reason: "registry requests apply but the cluster UID could not be observed"}
	case observedUID != a.GetExpectedClusterUid():
		return Mode{Reason: fmt.Sprintf("registry requests apply but this is cluster %s, not the registered %s", observedUID, a.GetExpectedClusterUid())}
	}
	return Mode{Apply: true, Reason: "apply enabled in the registry and in this controller; cluster identity verified"}
}

func (c *Controller) rpcCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.cfg.RPCTimeout)
}

// RunOnce performs one observe -> plan -> (apply) -> report cycle.
func (c *Controller) RunOnce(ctx context.Context) error {
	start := time.Now()
	err := c.runOnce(ctx)
	c.metrics.cycle(start, err, c.cfg.Now())
	return err
}

func (c *Controller) runOnce(ctx context.Context) error {
	c.cycles.Add(1)
	rctx, cancel := c.rpcCtx(ctx)
	a, err := c.rm.GetAssignment(rctx, &rmv1.GetAssignmentRequest{ClusterName: c.cfg.ClusterName})
	cancel()
	if err != nil {
		// Without the assignment there is no policy, and without policy the only
		// safe action is none. This is also the resource-manager-down path.
		return fmt.Errorf("get assignment: %w", err)
	}

	uid, version, idErr := c.clusterIdentity(ctx)
	mode := c.effectiveMode(a, uid)

	obs := c.observe(ctx, a)
	var cycleErrs []error
	if idErr != nil {
		cycleErrs = append(cycleErrs, idErr)
	}
	cycleErrs = append(cycleErrs, obs.errs...)

	// Scheduler HPAs before the pools: a patched HPA replaces its listed
	// observation, so this cycle's report already shows the result.
	cycleErrs = append(cycleErrs, c.reconcileSchedulers(ctx, a, mode, &obs)...)

	// Bindings are reconciled in a stable order so logs and events read the
	// same way every cycle.
	bindings := append([]*rmv1.PoolBinding(nil), a.GetBindings()...)
	sort.Slice(bindings, func(i, j int) bool {
		return bindings[i].GetKubernetesNamespace()+"/"+bindings[i].GetWorkerDeployment() <
			bindings[j].GetKubernetesNamespace()+"/"+bindings[j].GetWorkerDeployment()
	})
	for _, b := range bindings {
		if err := c.reconcilePool(ctx, a, b, mode, uid); err != nil {
			target := b.GetKubernetesNamespace() + "/" + b.GetWorkerDeployment()
			cycleErrs = append(cycleErrs, fmt.Errorf("%s: %w", target, err))
			c.metrics.poolError(b.GetPool())
			c.reportError(ctx, a.GetClusterId(), target, err)
		}
	}

	observed := int64(0)
	if len(cycleErrs) == 0 {
		// Only a clean cycle acknowledges the desired revision: "observed
		// revision N" must mean every binding of N was reconciled.
		observed = a.GetDesiredRevision()
	}
	lastErr := ""
	if len(cycleErrs) > 0 {
		lastErr = errors.Join(cycleErrs...).Error()
	}
	// Placement facts (ADR-0031). Labels only from a complete Node list, and
	// headroom only when every quota list succeeded: a partial view must
	// never replace the stored one.
	facts := &rmv1.ReportObservationsRequest{}
	if obs.nodes != nil {
		facts.Labels, facts.LabelsReported = clusterLabels(c.cfg.ClusterLabels, obs.nodes), true
	}
	if hr, ok, herr := c.headroom(ctx, a); herr != nil {
		cycleErrs = append(cycleErrs, herr)
	} else if ok {
		facts.Headroom, facts.HeadroomReported = hr, true
	}
	rctx, cancel = c.rpcCtx(ctx)
	_, err = c.rm.ReportObservations(rctx, &rmv1.ReportObservationsRequest{
		Labels: facts.Labels, LabelsReported: facts.LabelsReported,
		Headroom: facts.Headroom, HeadroomReported: facts.HeadroomReported,
		ClusterId: a.GetClusterId(), ControllerIdentity: c.cfg.Identity, ObservedClusterUid: uid,
		KubernetesVersion: version, ObservedRevision: observed, EffectiveMode: mode.String(),
		EffectiveModeReason: mode.Reason, CompleteKinds: obs.completeKinds, Objects: obs.objects,
		LastError: truncate(lastErr, 4096),
	})
	cancel()
	if err != nil {
		cycleErrs = append(cycleErrs, fmt.Errorf("report observations: %w", err))
	}
	if len(cycleErrs) > 0 {
		return errors.Join(cycleErrs...)
	}
	c.lastSuccess.Store(c.cfg.Now().UnixMilli())
	return nil
}

// clusterIdentity is the kube-system namespace UID -- stable for the life of a
// cluster and different for every cluster, which is what makes it a usable
// fingerprint without any cloud API.
func (c *Controller) clusterIdentity(ctx context.Context) (uid, version string, err error) {
	kctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	ns, err := c.kube.CoreV1().Namespaces().Get(kctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("observe cluster identity: %w", err)
	}
	if v, verr := c.kube.Discovery().ServerVersion(); verr == nil && v != nil {
		version = v.GitVersion
	}
	return string(ns.UID), version, nil
}

func (c *Controller) reportError(ctx context.Context, clusterID, target string, err error) {
	rctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	if _, rerr := c.rm.ReportReconcileError(rctx, &rmv1.ReportReconcileErrorRequest{
		ClusterId: clusterID, Target: target, Message: truncate(err.Error(), 4096), Actor: c.cfg.Identity,
	}); rerr != nil {
		c.log.WarnContext(ctx, "could not record reconcile error", "target", target, "error", err, "report_error", rerr)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
