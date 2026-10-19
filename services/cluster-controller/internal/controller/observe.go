// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchedulerSelector finds scheduler Deployments in the control-plane
// namespace. deploy/k8s labels them this way.
const SchedulerSelector = "app.kubernetes.io/part-of=kgf-nexus,app.kubernetes.io/component=scheduler"

// The kubernetes executor's task Job metadata (agent/internal/executor/
// kubernetes_spec.go): every Job it creates carries the label, and the
// annotations hold the full ids (a label value is hashed when too long).
const (
	DefaultJobLabelSelector = "nexus.dev/attempt"
	jobAnnAttemptID         = "nexus.dev/attempt-id"
	jobAnnRunID             = "nexus.dev/run-id"
	// Read as a fallback, for Jobs created by hand with the selector this
	// controller used before it matched the executor.
	legacyJobLabelAttemptID = "nexus.io/attempt-id"
	legacyJobLabelRunID     = "nexus.io/run-id"
)

type observation struct {
	objects       []*rmv1.ObservedObject
	completeKinds []string
	errs          []error
	// nodes is the full Node list when it succeeded, for the cluster's
	// placement labels (ADR-0031); nil when the list failed.
	nodes []corev1.Node
}

// observe collects the snapshot. Each kind is complete or not independently:
// one forbidden list must not blank out every other kind in the console.
func (c *Controller) observe(ctx context.Context, a *rmv1.ClusterAssignment) observation {
	var o observation
	cp := a.GetControlPlaneNamespace()

	// Worker Deployments are fetched by name from the bindings, not listed:
	// the controller only needs, and is only granted, the ones it scales.
	workerOK := true
	for _, b := range a.GetBindings() {
		kctx, cancel := c.rpcCtx(ctx)
		d, err := c.kube.AppsV1().Deployments(b.GetKubernetesNamespace()).Get(kctx, b.GetWorkerDeployment(), metav1.GetOptions{})
		cancel()
		switch {
		case apierrors.IsNotFound(err):
			// Absent is an observation, not a failure; the pool reconcile reports it.
		case err != nil:
			workerOK = false
			o.errs = append(o.errs, fmt.Errorf("observe worker deployment %s/%s: %w", b.GetKubernetesNamespace(), b.GetWorkerDeployment(), err))
		default:
			o.objects = append(o.objects, deploymentObject("WorkerDeployment", d))
		}
	}
	if workerOK {
		o.completeKinds = append(o.completeKinds, "WorkerDeployment")
	}

	kctx, cancel := c.rpcCtx(ctx)
	deps, err := c.kube.AppsV1().Deployments(cp).List(kctx, metav1.ListOptions{LabelSelector: SchedulerSelector})
	cancel()
	if err != nil {
		o.errs = append(o.errs, fmt.Errorf("observe scheduler deployments in %s: %w", cp, err))
	} else {
		o.completeKinds = append(o.completeKinds, "SchedulerDeployment")
		for i := range deps.Items {
			o.objects = append(o.objects, deploymentObject("SchedulerDeployment", &deps.Items[i]))
		}
	}

	kctx, cancel = c.rpcCtx(ctx)
	hpas, err := c.kube.AutoscalingV2().HorizontalPodAutoscalers(cp).List(kctx, metav1.ListOptions{})
	cancel()
	if err != nil {
		o.errs = append(o.errs, fmt.Errorf("observe HPAs in %s: %w", cp, err))
	} else {
		o.completeKinds = append(o.completeKinds, "HorizontalPodAutoscaler")
		for i := range hpas.Items {
			o.objects = append(o.objects, hpaObject(&hpas.Items[i]))
		}
	}

	kctx, cancel = c.rpcCtx(ctx)
	nodes, err := c.kube.CoreV1().Nodes().List(kctx, metav1.ListOptions{})
	cancel()
	if err != nil {
		o.errs = append(o.errs, fmt.Errorf("observe nodes: %w", err))
	} else {
		o.completeKinds = append(o.completeKinds, "Node")
		o.nodes = nodes.Items
		for i := range nodes.Items {
			o.objects = append(o.objects, nodeObject(&nodes.Items[i]))
		}
	}

	// Task Jobs live where the kubernetes executor creates them -- its
	// NEXUS_K8S_NAMESPACE, which is not necessarily a worker namespace.
	jobsOK := true
	var jobs []batchv1.Job
	for _, ns := range c.taskNamespaces(a) {
		kctx, cancel := c.rpcCtx(ctx)
		list, err := c.kube.BatchV1().Jobs(ns).List(kctx, metav1.ListOptions{LabelSelector: c.cfg.JobLabelSelector})
		cancel()
		if err != nil {
			jobsOK = false
			o.errs = append(o.errs, fmt.Errorf("observe task jobs in %s: %w", ns, err))
			continue
		}
		jobs = append(jobs, list.Items...)
	}
	if jobsOK {
		o.completeKinds = append(o.completeKinds, "Job")
		// Newest first, so a truncated report keeps the Jobs an operator is
		// most likely looking for.
		sort.Slice(jobs, func(i, j int) bool { return jobs[j].CreationTimestamp.Before(&jobs[i].CreationTimestamp) })
		if len(jobs) > c.cfg.MaxJobsReported {
			c.log.InfoContext(ctx, "task job report truncated", "total", len(jobs), "reported", c.cfg.MaxJobsReported)
			jobs = jobs[:c.cfg.MaxJobsReported]
		}
		for i := range jobs {
			o.objects = append(o.objects, jobObject(&jobs[i]))
		}
	}
	return o
}

func summary(v any) string {
	b, err := json.Marshal(v)
	if err != nil || len(b) > 8<<10 {
		return `{"truncated":true}`
	}
	return string(b)
}

func created(m metav1.ObjectMeta) int64 {
	if m.CreationTimestamp.IsZero() {
		return 0
	}
	return m.CreationTimestamp.UnixMilli()
}

func deploymentObject(kind string, d *appsv1.Deployment) *rmv1.ObservedObject {
	spec := int32(1)
	if d.Spec.Replicas != nil {
		spec = *d.Spec.Replicas
	}
	var conds []map[string]string
	for _, c := range d.Status.Conditions {
		conds = append(conds, map[string]string{"type": string(c.Type), "status": string(c.Status), "reason": c.Reason})
	}
	var images []string
	for _, ct := range d.Spec.Template.Spec.Containers {
		images = append(images, ct.Image)
	}
	return &rmv1.ObservedObject{
		Kind: kind, Namespace: d.Namespace, Name: d.Name, Uid: string(d.UID),
		Generation: d.Generation, ObservedGeneration: d.Status.ObservedGeneration,
		SpecReplicas: spec, ReadyReplicas: d.Status.ReadyReplicas,
		// Ready means the controller has seen the latest spec AND every desired
		// replica is available -- not merely that some pods are up.
		Ready:           d.Status.ObservedGeneration >= d.Generation && d.Status.AvailableReplicas == spec && d.Status.UpdatedReplicas == spec,
		ResourceVersion: d.ResourceVersion, CreatedAtUnixMs: created(d.ObjectMeta),
		SummaryJson: summary(map[string]any{
			"replicas": d.Status.Replicas, "updated": d.Status.UpdatedReplicas, "available": d.Status.AvailableReplicas,
			"unavailable": d.Status.UnavailableReplicas, "conditions": conds, "images": images,
		}),
	}
}

func hpaObject(h *autoscalingv2.HorizontalPodAutoscaler) *rmv1.ObservedObject {
	min := int32(1)
	if h.Spec.MinReplicas != nil {
		min = *h.Spec.MinReplicas
	}
	var targetCPU, currentCPU *int32
	for _, m := range h.Spec.Metrics {
		if m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil && m.Resource.Name == corev1.ResourceCPU {
			targetCPU = m.Resource.Target.AverageUtilization
		}
	}
	for _, m := range h.Status.CurrentMetrics {
		if m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil && m.Resource.Name == corev1.ResourceCPU {
			currentCPU = m.Resource.Current.AverageUtilization
		}
	}
	able := false
	for _, c := range h.Status.Conditions {
		if c.Type == autoscalingv2.ScalingActive && c.Status == corev1.ConditionTrue {
			able = true
		}
	}
	var observedGen int64
	if h.Status.ObservedGeneration != nil {
		observedGen = *h.Status.ObservedGeneration
	}
	return &rmv1.ObservedObject{
		Kind: "HorizontalPodAutoscaler", Namespace: h.Namespace, Name: h.Name, Uid: string(h.UID),
		Generation: h.Generation, ObservedGeneration: observedGen,
		SpecReplicas: h.Status.DesiredReplicas, ReadyReplicas: h.Status.CurrentReplicas,
		// ScalingActive=True is the HPA's own statement that it can read its
		// metric; without it the HPA is present but not actually scaling.
		Ready: able, ResourceVersion: h.ResourceVersion, CreatedAtUnixMs: created(h.ObjectMeta),
		SummaryJson: summary(map[string]any{
			"min_replicas": min, "max_replicas": h.Spec.MaxReplicas, "target_cpu_percent": targetCPU,
			"current_cpu_percent": currentCPU, "scale_target": h.Spec.ScaleTargetRef.Kind + "/" + h.Spec.ScaleTargetRef.Name,
			"scaling_active": able,
		}),
	}
}

// nodeLabels are the only labels reported: enough to group nodes by pool,
// size and zone, without copying arbitrary operator labels into the database.
var nodeLabels = []string{
	"kubernetes.azure.com/agentpool", "eks.amazonaws.com/nodegroup", "node.kubernetes.io/instance-type",
	"topology.kubernetes.io/zone", "kubernetes.io/os", "kubernetes.azure.com/mode",
}

func nodeObject(n *corev1.Node) *rmv1.ObservedObject {
	ready := false
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	labels := map[string]string{}
	for _, k := range nodeLabels {
		if v, ok := n.Labels[k]; ok {
			labels[k] = v
		}
	}
	res := func(l corev1.ResourceList) map[string]string {
		out := map[string]string{}
		for _, k := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, corev1.ResourcePods, "nvidia.com/gpu", corev1.ResourceEphemeralStorage} {
			if q, ok := l[k]; ok {
				out[string(k)] = q.String()
			}
		}
		return out
	}
	return &rmv1.ObservedObject{
		Kind: "Node", Name: n.Name, Uid: string(n.UID), Generation: n.Generation, ObservedGeneration: -1,
		SpecReplicas: -1, ReadyReplicas: -1, Ready: ready && !n.Spec.Unschedulable,
		ResourceVersion: n.ResourceVersion, CreatedAtUnixMs: created(n.ObjectMeta),
		SummaryJson: summary(map[string]any{
			"ready": ready, "unschedulable": n.Spec.Unschedulable, "labels": labels,
			"allocatable": res(n.Status.Allocatable), "capacity": res(n.Status.Capacity),
			"kubelet_version": n.Status.NodeInfo.KubeletVersion,
		}),
	}
}

func jobObject(j *batchv1.Job) *rmv1.ObservedObject {
	complete, failed := false, false
	for _, c := range j.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			complete = true
		case batchv1.JobFailed:
			failed = true
		}
	}
	ts := func(t *metav1.Time) string {
		if t == nil {
			return ""
		}
		return t.UTC().Format("2006-01-02T15:04:05Z")
	}
	state := "active"
	switch {
	case complete:
		state = "complete"
	case failed:
		state = "failed"
	case j.Status.Active == 0:
		state = "pending"
	}
	return &rmv1.ObservedObject{
		Kind: "Job", Namespace: j.Namespace, Name: j.Name, Uid: string(j.UID),
		// Jobs have no observedGeneration in status; -1 says "not reported"
		// rather than inventing a 0 that reads as "never observed".
		Generation: j.Generation, ObservedGeneration: -1, SpecReplicas: -1, ReadyReplicas: j.Status.Active,
		Ready: complete, ResourceVersion: j.ResourceVersion, CreatedAtUnixMs: created(j.ObjectMeta),
		SummaryJson: summary(map[string]any{
			"state": state, "active": j.Status.Active, "succeeded": j.Status.Succeeded, "failed": j.Status.Failed,
			"start_time": ts(j.Status.StartTime), "completion_time": ts(j.Status.CompletionTime),
			// The join back to Nexus: which attempt this Job runs.
			"attempt_id": jobMeta(j, jobAnnAttemptID, legacyJobLabelAttemptID),
			"run_id":     jobMeta(j, jobAnnRunID, legacyJobLabelRunID),
		}),
	}
}

// jobMeta is the executor's annotation, else the legacy label.
func jobMeta(j *batchv1.Job, annotation, legacyLabel string) string {
	if v := j.Annotations[annotation]; v != "" {
		return v
	}
	return j.Labels[legacyLabel]
}
