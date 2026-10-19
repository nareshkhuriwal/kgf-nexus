// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Scheduler autoscaling (ADR-0036).
//
// The scheduler Deployment is scaled by its HorizontalPodAutoscaler, and the
// controller never writes the scheduler's replica count. What it may change,
// in apply mode, is four settings ON that HPA -- minReplicas, maxReplicas, the
// CPU utilization target and behavior.scaleDown.stabilizationWindowSeconds --
// moving them to the namespace's saved scheduler policy. Each HPA still has
// exactly one writer per field: the HPA controller owns the replica count, and
// the cluster controller owns the four settings only on an HPA that opts in
// with platform.HPAOwnerAnnotation. Everything else on every HPA is left alone.
//
// Every patch is
//   - pinned to the HPA's UID and to the ownership annotation (JSON-patch
//     test operations, checked by the API server atomically with the write);
//   - a compare-and-swap on the resourceVersion it was computed from, so a
//     concurrent writer wins and the patch is re-planned from a fresh read;
//   - one bounded step (platform.NextHPAStep), at most one per HPA per
//     SchedulerPatchInterval, with the last patch time persisted in the
//     resource manager so a restart or a leader change does not reset it;
//   - recorded as a cluster_scaling_events row, and a refusal as a reconcile
//     error.

// maxHPAPatchAttempts bounds the Conflict retries within one cycle. Each retry
// re-reads the HPA; the HPA controller writes its status every sync, which is
// the usual reason the first attempt's resourceVersion is already stale.
const maxHPAPatchAttempts = 3

// fieldManager attributes the controller's writes in managedFields, so
// `kubectl get hpa -o yaml --show-managed-fields` names who set what.
const fieldManager = "nexus-cluster-controller"

// errNotApplicable marks an HPA whose saved settings cannot be applied without
// a human (not opted in, no CPU utilization metric). It is reported as drift,
// not as a failed cycle: nothing is broken, the cluster is configured that way.
var errNotApplicable = errors.New("not applicable")

func settingsFromPB(m *rmv1.SchedulerAutoscalingSettings) platform.HPASettings {
	return platform.HPASettings{MinReplicas: m.GetMinReplicas(), MaxReplicas: m.GetMaxReplicas(),
		TargetCPUPercent: m.GetTargetCpuPercent(), ScaleDownStabilizationSeconds: m.GetScaleDownStabilizationSeconds()}
}

func settingsPB(s platform.HPASettings) *rmv1.SchedulerAutoscalingSettings {
	return &rmv1.SchedulerAutoscalingSettings{MinReplicas: s.MinReplicas, MaxReplicas: s.MaxReplicas,
		TargetCpuPercent: s.TargetCPUPercent, ScaleDownStabilizationSeconds: s.ScaleDownStabilizationSeconds}
}

// hpaSettings reads the managed settings from an HPA, with Kubernetes'
// defaults where a field is unset, and the index of its CPU utilization metric
// (-1 when it has none).
func hpaSettings(h *autoscalingv2.HorizontalPodAutoscaler) (platform.HPASettings, int) {
	s := platform.HPASettings{MinReplicas: 1, MaxReplicas: h.Spec.MaxReplicas,
		ScaleDownStabilizationSeconds: platform.DefaultScaleDownStabilizationSeconds}
	if h.Spec.MinReplicas != nil {
		s.MinReplicas = *h.Spec.MinReplicas
	}
	cpu := -1
	for i, m := range h.Spec.Metrics {
		if m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil && m.Resource.Name == corev1.ResourceCPU &&
			m.Resource.Target.Type == autoscalingv2.UtilizationMetricType && m.Resource.Target.AverageUtilization != nil {
			s.TargetCPUPercent, cpu = *m.Resource.Target.AverageUtilization, i
			break
		}
	}
	if b := h.Spec.Behavior; b != nil && b.ScaleDown != nil && b.ScaleDown.StabilizationWindowSeconds != nil {
		s.ScaleDownStabilizationSeconds = *b.ScaleDown.StabilizationWindowSeconds
	}
	return s, cpu
}

func hpaOwned(h *autoscalingv2.HorizontalPodAutoscaler) bool {
	return h.Annotations[platform.HPAOwnerAnnotation] == platform.HPAOwnerValue
}

// schedulerHPAs are the HPAs in the control-plane namespace whose scale target
// is one of the observed scheduler Deployments, by name.
func schedulerHPAs(schedulers []string, hpas []autoscalingv2.HorizontalPodAutoscaler) []*autoscalingv2.HorizontalPodAutoscaler {
	isScheduler := map[string]bool{}
	for _, n := range schedulers {
		isScheduler[n] = true
	}
	var out []*autoscalingv2.HorizontalPodAutoscaler
	for i := range hpas {
		ref := hpas[i].Spec.ScaleTargetRef
		group, _, _ := strings.Cut(ref.APIVersion, "/")
		if ref.Kind == "Deployment" && group == "apps" && isScheduler[ref.Name] {
			out = append(out, &hpas[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// reconcileSchedulers brings every scheduler HPA toward the saved settings and
// records the outcome. It returns the errors that fail the cycle: API refusals,
// not HPAs that are simply not Nexus's to change.
func (c *Controller) reconcileSchedulers(ctx context.Context, a *rmv1.ClusterAssignment, mode Mode, obs *observation) []error {
	sa := a.GetSchedulerAutoscaling()
	if sa == nil {
		// A resource manager that predates ADR-0036 sends no settings: there
		// is nothing to compare with, so nothing is planned or changed.
		return nil
	}
	if !obs.hpasListed || !obs.schedulersListed {
		// Which HPAs scale a scheduler is unknown without both lists; the
		// failed list is already a cycle error. Change and record nothing.
		return nil
	}
	desired := settingsFromPB(sa.GetDesired())
	persisted := map[string]time.Time{}
	for _, m := range sa.GetHpas() {
		if ms := m.GetLastAppliedAtUnixMs(); ms > 0 {
			persisted[m.GetKubernetesNamespace()+"/"+m.GetHpaName()] = time.UnixMilli(ms)
		}
	}
	var (
		reports []*rmv1.SchedulerHPAReconcile
		errs    []error
	)
	for _, h := range schedulerHPAs(obs.schedulerNames, obs.hpas) {
		key := h.Namespace + "/" + h.Name
		last := persisted[key]
		c.mu.Lock()
		if t := c.hpaApplied[key]; t.After(last) {
			// A patch this process made whose record did not reach the
			// resource manager still counts against the patch interval.
			last = t
		}
		c.mu.Unlock()
		rep, updated, err := c.reconcileHPA(ctx, mode, h, desired, sa.GetConfigRevision(), last)
		if updated != nil {
			obs.replace(hpaObject(updated))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("scheduler HPA %s: %w", key, err))
			c.reportError(ctx, a.GetClusterId(), "hpa "+key, err)
		}
		c.metrics.hpa(rep, mode)
		reports = append(reports, rep)
	}
	if len(reports) == 0 && len(sa.GetHpas()) == 0 {
		return errs
	}
	rctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	if _, err := c.rm.RecordSchedulerAutoscaling(rctx, &rmv1.RecordSchedulerAutoscalingRequest{
		ClusterId: a.GetClusterId(), Actor: c.cfg.Identity, Mode: mode.String(), Complete: true, Targets: reports,
	}); err != nil {
		c.log.WarnContext(ctx, "could not record scheduler autoscaling", "error", err)
		// Forget the signatures so the events are retried next cycle rather
		// than silently dropped.
		c.mu.Lock()
		for _, r := range reports {
			delete(c.lastDecision, hpaDecisionKey(r.GetKubernetesNamespace(), r.GetHpaName()))
		}
		c.mu.Unlock()
	}
	return errs
}

func hpaDecisionKey(ns, name string) string { return "hpa/" + ns + "/" + name }

// decide records sig as the latest decision for key and reports whether it
// differs from the previous one -- the event-deduplication rule the pools use:
// a decision repeated every cycle is one audit row, not one per cycle.
func (c *Controller) decide(key, sig string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	changed := c.lastDecision[key] != sig
	c.lastDecision[key] = sig
	return changed
}

// reconcileHPA runs one cycle for one scheduler HPA. updated is the HPA as
// the API server returned it after a patch (nil when nothing was patched).
func (c *Controller) reconcileHPA(ctx context.Context, mode Mode, h *autoscalingv2.HorizontalPodAutoscaler,
	desired platform.HPASettings, revision int64, lastApplied time.Time,
) (rep *rmv1.SchedulerHPAReconcile, updated *autoscalingv2.HorizontalPodAutoscaler, err error) {
	cur, cpu := hpaSettings(h)
	owned := hpaOwned(h)
	key := hpaDecisionKey(h.Namespace, h.Name)
	log := c.log.With("hpa", h.Namespace+"/"+h.Name, "mode", mode.String())
	rep = &rmv1.SchedulerHPAReconcile{
		KubernetesNamespace: h.Namespace, HpaName: h.Name, Deployment: h.Spec.ScaleTargetRef.Name, HpaUid: string(h.UID),
		Managed: owned, ConfigRevision: revision, Observed: settingsPB(cur),
	}
	diff := cur.Diff(desired)
	switch {
	case diff == "":
		rep.Result, rep.Reason = "in_sync", "matches the saved scheduler settings"
		c.decide(key, "in_sync")
		return rep, nil, nil
	case revision == 0:
		// Defaults nobody chose are not a reason to resize anything.
		rep.Result = "not_configured"
		rep.Reason = "scheduler settings were never saved for this namespace; the HPA is left as it is"
		rep.Error = rep.Reason
		c.decide(key, "not_configured")
		return rep, nil, nil
	}

	var blocked string
	switch {
	case !owned:
		blocked = fmt.Sprintf("not controller-owned: annotate it %s=%s to let apply mode keep it at the saved settings, or change it where it is managed (Helm values, GitOps)",
			platform.HPAOwnerAnnotation, platform.HPAOwnerValue)
	case cpu < 0:
		blocked = "it has no CPU utilization metric; applying the saved CPU target would change what the HPA scales on, which is not Nexus's to decide"
	}
	rep.From, rep.To = settingsPB(cur), settingsPB(desired)
	detail := func(extra map[string]any) string {
		m := map[string]any{"hpa": h.Name, "deployment": h.Spec.ScaleTargetRef.Name, "uid": string(h.UID),
			"from": cur, "desired": desired, "config_revision": revision}
		for k, v := range extra {
			m[k] = v
		}
		b, _ := json.Marshal(m)
		return string(b)
	}

	if !mode.Apply {
		rep.Result = "plan"
		rep.Reason = "plan: would set " + diff
		if step := platform.NextHPAStep(cur, desired); step != desired {
			rep.Reason += " in bounded steps, first " + cur.Diff(step)
		}
		if blocked != "" {
			rep.Reason += "; apply mode would not change it: " + blocked
		}
		rep.Reason += fmt.Sprintf(" (platform config revision %d)", revision)
		if c.decide(key, fmt.Sprintf("plan|%+v|%+v|%s", cur, desired, blocked)) {
			rep.Action, rep.DetailJson = "hpa_plan", detail(nil)
		}
		return rep, nil, nil
	}

	if blocked != "" {
		rep.Result, rep.Reason, rep.Error = "blocked", "differs from the saved settings ("+diff+")", blocked
		if c.decide(key, fmt.Sprintf("blocked|%+v|%+v|%s", cur, desired, blocked)) {
			rep.Action, rep.DetailJson = "hpa_refused", detail(map[string]any{"refused": blocked})
			log.InfoContext(ctx, "scheduler HPA differs from the saved settings but is not changed", "diff", diff, "why", blocked)
		}
		return rep, nil, nil
	}

	now := c.cfg.Now()
	if !lastApplied.IsZero() && now.Sub(lastApplied) < c.cfg.SchedulerPatchInterval {
		wait := c.cfg.SchedulerPatchInterval - now.Sub(lastApplied)
		rep.Result = "applying"
		rep.Reason = fmt.Sprintf("next bounded step (%s) in %s", cur.Diff(platform.NextHPAStep(cur, desired)), wait.Round(time.Second))
		return rep, nil, nil
	}

	updated, from, to, err := c.patchHPA(ctx, h, desired)
	switch {
	case errors.Is(err, errNotApplicable):
		rep.Result, rep.Reason, rep.Error = "blocked", "differs from the saved settings ("+diff+")", err.Error()
		if c.decide(key, "blocked|"+err.Error()) {
			rep.Action, rep.DetailJson = "hpa_refused", detail(map[string]any{"refused": err.Error()})
		}
		return rep, nil, nil
	case err != nil:
		rep.Result, rep.Reason, rep.Error = "blocked", "patch refused; nothing was changed", err.Error()
		if c.decide(key, "refused|"+err.Error()) {
			rep.Action, rep.DetailJson = "hpa_refused", detail(map[string]any{"error": err.Error()})
		}
		log.WarnContext(ctx, "scheduler HPA patch refused", "error", err)
		return rep, nil, err
	case updated == nil:
		// A fresh read found it already at the saved settings: someone else
		// applied them between the list and the patch.
		rep.Result, rep.Reason, rep.Observed = "in_sync", "matches the saved scheduler settings", settingsPB(from)
		c.decide(key, "in_sync")
		return rep, nil, nil
	}

	c.mu.Lock()
	c.hpaApplied[h.Namespace+"/"+h.Name] = now
	c.mu.Unlock()
	after, _ := hpaSettings(updated)
	rep.Observed, rep.From, rep.To = settingsPB(after), settingsPB(from), settingsPB(to)
	rep.Action, rep.Applied, rep.HpaUid = "hpa_update", true, string(updated.UID)
	rep.Reason = "set " + from.Diff(to)
	if to == desired {
		rep.Result = "in_sync"
	} else {
		rep.Result = "applying"
		rep.Reason += "; then " + to.Diff(desired) + " in further bounded steps"
	}
	rep.Reason += fmt.Sprintf(" (platform config revision %d)", revision)
	rep.DetailJson = detail(map[string]any{"from": from, "to": to, "bounded": to != desired, "resource_version": updated.ResourceVersion})
	c.decide(key, "applied|"+rep.GetReason())
	log.InfoContext(ctx, "scheduler HPA patched toward the saved settings", "change", from.Diff(to), "remaining", to.Diff(desired))
	return rep, updated, nil
}

// patchHPA patches h one bounded step toward desired. A Conflict is retried on
// a fresh read of the SAME object: a different UID means the HPA was replaced,
// and the replacement is planned from scratch next cycle. Returns updated=nil
// with no error when the fresh read already matches desired.
func (c *Controller) patchHPA(ctx context.Context, h *autoscalingv2.HorizontalPodAutoscaler, desired platform.HPASettings,
) (updated *autoscalingv2.HorizontalPodAutoscaler, from, to platform.HPASettings, err error) {
	uid, cur := h.UID, h
	api := c.kube.AutoscalingV2().HorizontalPodAutoscalers(h.Namespace)
	for attempt := 1; ; attempt++ {
		var cpu int
		from, cpu = hpaSettings(cur)
		if from == desired {
			return nil, from, from, nil
		}
		switch {
		case !hpaOwned(cur):
			return nil, from, from, fmt.Errorf("%w: the ownership annotation was removed", errNotApplicable)
		case cpu < 0:
			return nil, from, from, fmt.Errorf("%w: it has no CPU utilization metric", errNotApplicable)
		}
		to = platform.NextHPAStep(from, desired)
		patch, perr := hpaPatch(cur, cpu, from, to)
		if perr != nil {
			return nil, from, to, perr
		}
		kctx, cancel := c.rpcCtx(ctx)
		updated, err = api.Patch(kctx, cur.Name, types.JSONPatchType, patch, metav1.PatchOptions{FieldManager: fieldManager})
		cancel()
		switch {
		case err == nil:
			return updated, from, to, nil
		case apierrors.IsForbidden(err):
			return nil, from, to, fmt.Errorf("patch forbidden; the apply RBAC must grant horizontalpodautoscalers patch in %s "+
				"(deploy/k8s/cluster-controller/apply, or the chart's clusterController.apply): %w", cur.Namespace, err)
		case !apierrors.IsConflict(err):
			return nil, from, to, fmt.Errorf("patch: %w", err)
		case attempt == maxHPAPatchAttempts:
			return nil, from, to, fmt.Errorf("changed under every one of %d attempts (another writer is active); re-planning next cycle: %w",
				maxHPAPatchAttempts, err)
		}
		kctx, cancel = c.rpcCtx(ctx)
		fresh, gerr := api.Get(kctx, cur.Name, metav1.GetOptions{})
		cancel()
		if gerr != nil {
			return nil, from, to, fmt.Errorf("re-read after a conflict: %w", gerr)
		}
		if fresh.UID != uid {
			return nil, from, to, fmt.Errorf("replaced while being patched (uid %s, observed %s); re-planning next cycle", fresh.UID, uid)
		}
		cur = fresh
	}
}

type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// jsonPointerEscape escapes one JSON-pointer reference token (RFC 6901).
func jsonPointerEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// hpaPatch is the JSON patch (RFC 6902) moving h's managed settings from
// `from` to `to`, touching only the fields that change. Its test operations
// make the API server refuse it unless the object is still the one planned
// for (same UID, still opted in, the CPU metric still at the same index), and
// its resourceVersion makes it a compare-and-swap: the API server answers 409
// Conflict when the object changed since it was read.
func hpaPatch(h *autoscalingv2.HorizontalPodAutoscaler, cpu int, from, to platform.HPASettings) ([]byte, error) {
	if h.UID == "" || h.ResourceVersion == "" {
		return nil, errors.New("HPA has no uid or resourceVersion; refusing an unconditional patch")
	}
	ops := []jsonPatchOp{
		{Op: "test", Path: "/metadata/uid", Value: string(h.UID)},
		{Op: "test", Path: "/metadata/annotations/" + jsonPointerEscape(platform.HPAOwnerAnnotation), Value: platform.HPAOwnerValue},
		{Op: "replace", Path: "/metadata/resourceVersion", Value: h.ResourceVersion},
	}
	if to.MinReplicas != from.MinReplicas {
		// add, not replace: minReplicas is optional, and add sets an existing
		// member as well.
		ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/minReplicas", Value: to.MinReplicas})
	}
	if to.MaxReplicas != from.MaxReplicas {
		ops = append(ops, jsonPatchOp{Op: "replace", Path: "/spec/maxReplicas", Value: to.MaxReplicas})
	}
	if to.TargetCPUPercent != from.TargetCPUPercent {
		base := fmt.Sprintf("/spec/metrics/%d/resource", cpu)
		ops = append(ops,
			jsonPatchOp{Op: "test", Path: base + "/name", Value: string(corev1.ResourceCPU)},
			jsonPatchOp{Op: "test", Path: base + "/target/type", Value: string(autoscalingv2.UtilizationMetricType)},
			jsonPatchOp{Op: "replace", Path: base + "/target/averageUtilization", Value: to.TargetCPUPercent})
	}
	if to.ScaleDownStabilizationSeconds != from.ScaleDownStabilizationSeconds {
		w := to.ScaleDownStabilizationSeconds
		switch b := h.Spec.Behavior; {
		case b == nil:
			ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/behavior",
				Value: map[string]any{"scaleDown": map[string]any{"stabilizationWindowSeconds": w}}})
		case b.ScaleDown == nil:
			ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/behavior/scaleDown",
				Value: map[string]any{"stabilizationWindowSeconds": w}})
		default:
			ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/behavior/scaleDown/stabilizationWindowSeconds", Value: w})
		}
	}
	return json.Marshal(ops)
}
