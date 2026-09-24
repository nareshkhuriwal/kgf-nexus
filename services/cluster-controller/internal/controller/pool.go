// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"

	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// MinDeletionCost is the pod-deletion-cost given to a drained pod just before
// its Deployment is scaled in. The ReplicaSet controller removes the
// lowest-cost pod among pods that are otherwise equally ranked (all scheduled,
// Running and Ready -- which scale-in requires), so this is how "remove THIS
// drained worker, not a busy one" is expressed to Kubernetes.
const MinDeletionCost = math.MinInt32

type podView struct {
	pod         *corev1.Pod
	ready       bool
	terminating bool
	workers     *rmv1.PodWorkers
	cost        int64
}

// drained: every worker registered from the pod is DRAINING (none ACTIVE or
// QUARANTINED). It still may hold leases -- that is what removal re-checks.
func (p podView) drained() bool {
	w := p.workers
	return w != nil && w.GetDraining() > 0 && w.GetActive() == 0 && w.GetQuarantined() == 0
}

// Step is one cycle's decision for one worker Deployment.
type Step struct {
	From, To int32
	Add      int
	Remove   []string // drained, lease-free pods to remove now
	Drain    []string // pods to drain now; removed in a later cycle
	Holds    []string // why something the plan wanted did not happen
	Decision platform.Decision
	Idle     bool
}

// Action is the event name for the step.
func (s Step) Action() string {
	switch {
	case s.To > s.From:
		return "scale_out"
	case len(s.Remove) > 0 && s.To < s.From:
		return "scale_in"
	case len(s.Drain) > 0:
		return "drain"
	case len(s.Holds) > 0:
		return "hold"
	}
	return ""
}

// Reason is a one-line operator explanation.
func (s Step) Reason() string {
	parts := []string{s.Decision.Reason}
	if len(s.Drain) > 0 {
		parts = append(parts, "draining "+strings.Join(s.Drain, ", ")+" before removal")
	}
	if len(s.Remove) > 0 {
		parts = append(parts, "removing drained lease-free "+strings.Join(s.Remove, ", "))
	}
	parts = append(parts, s.Holds...)
	return strings.Join(parts, "; ")
}

func bindingPolicy(b *rmv1.PoolBinding) (platform.Pool, platform.PodProfile) {
	return platform.Pool{
		Name: b.GetPool(), Queue: b.GetQueue(), MinWorkers: int(b.GetMinWorkers()), MaxWorkers: int(b.GetMaxWorkers()),
		TasksPerWorker: int(b.GetTasksPerWorker()), IdleSeconds: int(b.GetIdleSeconds()),
		MaxScaleUpStep: int(b.GetMaxScaleUpStep()), Enabled: b.GetEnabled(),
	}, platform.PodProfile{
		CPU: b.GetProfileCpuMillis(), Memory: b.GetProfileMemoryMib(), GPU: int(b.GetProfileGpuCount()),
	}
}

func podReady(p *corev1.Pod) bool {
	if p.Spec.NodeName == "" || p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func deletionCost(p *corev1.Pod) int64 {
	v, ok := p.Annotations[corev1.PodDeletionCost]
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		return 0
	}
	return n
}

// stable reports whether the ReplicaSet's victim choice is predictable. Only
// when every pod is scheduled, Running and Ready, with no rollout and nothing
// terminating, is pod-deletion-cost the deciding criterion -- otherwise the
// ReplicaSet prefers deleting a not-ready or pending pod, which could be a busy
// worker that merely failed one readiness probe.
func stable(dep *appsv1.Deployment, current int32, pods []podView) (bool, string) {
	switch {
	case dep.Status.ObservedGeneration < dep.Generation:
		return false, "deployment spec change not yet observed by its controller"
	case dep.Status.Replicas != current || dep.Status.UpdatedReplicas != current:
		return false, fmt.Sprintf("rollout or scale in progress (%d pods, %d updated, %d desired)", dep.Status.Replicas, dep.Status.UpdatedReplicas, current)
	}
	live := 0
	for _, p := range pods {
		if p.terminating {
			return false, "pod " + p.pod.Name + " is terminating"
		}
		if !p.ready {
			return false, "pod " + p.pod.Name + " is not Running and Ready"
		}
		live++
	}
	if int32(live) != current {
		return false, fmt.Sprintf("%d live pods for %d desired replicas", live, current)
	}
	return true, ""
}

// reconcilePool runs one cycle for one binding.
func (c *Controller) reconcilePool(ctx context.Context, a *rmv1.ClusterAssignment, b *rmv1.PoolBinding, mode Mode, uid string) error {
	ns, name := b.GetKubernetesNamespace(), b.GetWorkerDeployment()
	log := c.log.With("namespace", b.GetNamespace(), "pool", b.GetPool(), "deployment", ns+"/"+name, "mode", mode.String())
	if b.GetConfigError() != "" {
		return fmt.Errorf("binding not reconcilable: %s", b.GetConfigError())
	}

	kctx, cancel := c.rpcCtx(ctx)
	dep, err := c.kube.AppsV1().Deployments(ns).Get(kctx, name, metav1.GetOptions{})
	cancel()
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("worker deployment %s/%s does not exist", ns, name)
	}
	if err != nil {
		return fmt.Errorf("get worker deployment: %w", err)
	}
	sel, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
	if err != nil {
		return fmt.Errorf("worker deployment selector: %w", err)
	}
	kctx, cancel = c.rpcCtx(ctx)
	podList, err := c.kube.CoreV1().Pods(ns).List(kctx, metav1.ListOptions{LabelSelector: sel.String()})
	cancel()
	if err != nil {
		return fmt.Errorf("list worker pods: %w", err)
	}

	hostnames := make([]string, 0, len(podList.Items))
	for _, p := range podList.Items {
		hostnames = append(hostnames, p.Name)
	}
	rctx, cancel := c.rpcCtx(ctx)
	ps, err := c.rm.GetPoolState(rctx, &rmv1.GetPoolStateRequest{
		ClusterId: a.GetClusterId(), Namespace: b.GetNamespace(), Pool: b.GetPool(), Hostnames: hostnames,
	})
	cancel()
	if err != nil {
		// No demand, no lease view: change nothing. Never treat an unanswered
		// question as "zero demand" -- that is how an autoscaler scales a busy
		// pool to its minimum during a resource-manager restart.
		return fmt.Errorf("get pool state: %w", err)
	}
	byHost := map[string]*rmv1.PodWorkers{}
	for _, w := range ps.GetPods() {
		byHost[w.GetHostname()] = w
	}
	pods := make([]podView, 0, len(podList.Items))
	for i := range podList.Items {
		p := &podList.Items[i]
		pods = append(pods, podView{pod: p, ready: podReady(p), terminating: p.DeletionTimestamp != nil,
			workers: byHost[p.Name], cost: deletionCost(p)})
	}

	now := c.cfg.Now()
	current := int32(1)
	if dep.Spec.Replicas != nil {
		current = *dep.Spec.Replicas
	}
	st, idleUpdate, idleSince, err := c.plan(ctx, a, b, dep, current, pods, ps.GetDemand(), now)
	if err != nil {
		return err
	}

	applied := false
	if mode.Apply {
		if err := c.apply(ctx, a, b, dep, &st, pods, uid); err != nil {
			c.record(ctx, a, b, st, mode, false, idleUpdate, idleSince, "refused", err.Error())
			return err
		}
		applied = true
	}
	c.record(ctx, a, b, st, mode, applied, idleUpdate, idleSince, "", "")
	if st.Action() != "" {
		log.InfoContext(ctx, "scaling decision", "action", st.Action(), "from", st.From, "to", st.To,
			"drain", st.Drain, "remove", st.Remove, "reason", st.Reason(), "applied", applied)
	}
	return nil
}

// plan computes the step. It reads the cluster (for quota) and the resource
// manager (for fresh removability) but changes nothing.
func (c *Controller) plan(ctx context.Context, a *rmv1.ClusterAssignment, b *rmv1.PoolBinding, dep *appsv1.Deployment,
	current int32, pods []podView, d *rmv1.Demand, now time.Time,
) (Step, rmv1.RecordScalingRequest_IdleUpdate, time.Time, error) {
	// Idle is tracked across cycles (and restarts, via the resource manager)
	// because platform.Plan only allows scale-in after a SUSTAINED idle period,
	// and one snapshot cannot prove that.
	idle := d.GetPendingTasks() == 0 && d.GetRunningTasks() == 0
	idleUpdate := rmv1.RecordScalingRequest_IDLE_UPDATE_UNSPECIFIED
	idleSince := time.Time{}
	idleSeconds := 0
	if idle {
		if b.GetIdleSinceUnixMs() > 0 {
			idleSince = time.UnixMilli(b.GetIdleSinceUnixMs())
		} else {
			idleSince = now
			idleUpdate = rmv1.RecordScalingRequest_IDLE_UPDATE_SET
		}
		idleSeconds = int(now.Sub(idleSince) / time.Second)
	} else if b.GetIdleSinceUnixMs() > 0 {
		idleUpdate = rmv1.RecordScalingRequest_IDLE_UPDATE_CLEAR
	}

	var drained []podView
	for _, p := range pods {
		if !p.terminating && p.drained() {
			drained = append(drained, p)
		}
	}
	// Drained pods are on their way out and take no work, so they are not
	// capacity. They still occupy replicas (and the max bound) until removed.
	active := current - int32(len(drained))
	if active < 0 {
		active = 0
	}
	pool, profile := bindingPolicy(b)
	decision := platform.Plan(pool, profile, platform.Demand{
		Pending: int(d.GetPendingTasks()), Running: int(d.GetRunningTasks()), CPU: d.GetCpuMillis(),
		Memory: d.GetMemoryMib(), GPU: int(d.GetGpuCount()), Unschedulable: int(d.GetUnschedulableTasks()),
		CurrentWorkers: int(active), IdleSeconds: idleSeconds,
	})
	st := Step{From: current, To: current, Decision: decision, Idle: idle}
	if !pool.Enabled {
		return st, idleUpdate, idleSince, nil
	}

	isStable, unstableWhy := stable(dep, current, pods)
	var lastScale time.Time
	if b.GetLastScaleAtUnixMs() > 0 {
		lastScale = time.UnixMilli(b.GetLastScaleAtUnixMs())
	}
	step := int(max32(b.GetMaxScaleDownStep(), 1))

	// 1. Remove pods that finished draining. Not cooldown-gated: a drained pod
	// accepts no work, so keeping it only costs money. The removability check
	// is FRESH, made now, never inferred from the pool state read above.
	if len(drained) > 0 {
		if !isStable {
			st.Holds = append(st.Holds, "removal of drained pods held: "+unstableWhy)
		} else {
			sort.Slice(drained, func(i, j int) bool { return drained[i].pod.Name < drained[j].pod.Name })
			for _, p := range drained {
				if len(st.Remove) >= step {
					break
				}
				rctx, cancel := c.rpcCtx(ctx)
				chk, err := c.rm.CheckPodRemovable(rctx, &rmv1.CheckPodRemovableRequest{
					ClusterId: a.GetClusterId(), Namespace: b.GetNamespace(), Hostname: p.pod.Name,
				})
				cancel()
				if err != nil {
					return st, idleUpdate, idleSince, fmt.Errorf("check pod %s removable: %w", p.pod.Name, err)
				}
				if !chk.GetRemovable() {
					st.Holds = append(st.Holds, fmt.Sprintf("pod %s kept: %s", p.pod.Name, chk.GetReason()))
					continue
				}
				st.Remove = append(st.Remove, p.pod.Name)
			}
		}
	}
	st.To = current - int32(len(st.Remove))

	switch {
	case decision.Desired > int(active):
		// 2. Scale out. Skipped in the same cycle as a removal: a combined
		// "remove one, add one" nets to an unchanged replica count, and the
		// ReplicaSet would then delete nothing.
		if len(st.Remove) > 0 {
			st.Holds = append(st.Holds, "scale-out deferred one cycle until drained pods are removed")
			break
		}
		if !lastScale.IsZero() && b.GetLastScaleDirection() == "out" && now.Sub(lastScale) < time.Duration(b.GetScaleUpCooldownSeconds())*time.Second {
			st.Holds = append(st.Holds, fmt.Sprintf("scale-out cooldown: last scale-out %s ago", now.Sub(lastScale).Round(time.Second)))
			break
		}
		add := decision.Desired - int(active)
		if room := int(b.GetMaxWorkers()) - int(current); add > room {
			// Drained pods still hold replicas; the bound is on pods, not on
			// active workers, because pods are what cost money.
			add = room
			st.Holds = append(st.Holds, fmt.Sprintf("capped at max %d replicas", b.GetMaxWorkers()))
		}
		if add > 0 {
			kctx, cancel := c.rpcCtx(ctx)
			quotas, err := c.kube.CoreV1().ResourceQuotas(dep.Namespace).List(kctx, metav1.ListOptions{})
			cancel()
			if err != nil {
				return st, idleUpdate, idleSince, fmt.Errorf("list resource quotas: %w", err)
			}
			if h, why := QuotaHeadroom(quotas.Items, dep.Spec.Template.Spec); h < add {
				add = h
				st.Holds = append(st.Holds, "namespace quota: "+why)
			}
		}
		if add > 0 {
			st.Add = add
			st.To = current + int32(add)
		}
	case decision.Desired < int(active) && len(drained) == 0:
		// 3. Choose workers to drain. One batch at a time: nothing new is
		// drained while an earlier batch is still on its way out.
		if !lastScale.IsZero() && now.Sub(lastScale) < time.Duration(b.GetScaleDownCooldownSeconds())*time.Second {
			st.Holds = append(st.Holds, fmt.Sprintf("scale-in cooldown: last scale %s ago", now.Sub(lastScale).Round(time.Second)))
			break
		}
		if !isStable {
			st.Holds = append(st.Holds, "scale-in held: "+unstableWhy)
			break
		}
		k := int(active) - decision.Desired
		if k > step {
			k = step
		}
		var candidates []podView
		for _, p := range pods {
			// Only registered, ACTIVE workers are drained. An unregistered pod
			// may simply be starting; draining is how the controller claims a
			// pod for removal, and it cannot claim what it cannot see.
			if !p.terminating && p.workers != nil && p.workers.GetActive() > 0 {
				candidates = append(candidates, p)
			}
		}
		// Least work first; among equals, the newest pod.
		sort.SliceStable(candidates, func(i, j int) bool {
			wi, wj := candidates[i].workers, candidates[j].workers
			if wi.GetLeaseCount() != wj.GetLeaseCount() {
				return wi.GetLeaseCount() < wj.GetLeaseCount()
			}
			if wi.GetReservationCount() != wj.GetReservationCount() {
				return wi.GetReservationCount() < wj.GetReservationCount()
			}
			return candidates[j].pod.CreationTimestamp.Before(&candidates[i].pod.CreationTimestamp)
		})
		if len(candidates) < k {
			k = len(candidates)
		}
		for _, p := range candidates[:k] {
			st.Drain = append(st.Drain, p.pod.Name)
		}
	}
	return st, idleUpdate, idleSince, nil
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// apply carries out a step: drains through the resource manager, then (for a
// removal) marks the victims and changes the replica count with an optimistic
// concurrency precondition.
func (c *Controller) apply(ctx context.Context, a *rmv1.ClusterAssignment, b *rmv1.PoolBinding, dep *appsv1.Deployment,
	st *Step, pods []podView, uid string,
) error {
	for _, host := range st.Drain {
		rctx, cancel := c.rpcCtx(ctx)
		_, err := c.rm.DrainPod(rctx, &rmv1.DrainPodRequest{
			ClusterId: a.GetClusterId(), Namespace: b.GetNamespace(), Hostname: host, Actor: c.cfg.Identity,
			Reason: st.Decision.Reason, ObservedClusterUid: uid,
			KubernetesNamespace: b.GetKubernetesNamespace(), WorkerDeployment: b.GetWorkerDeployment(),
		})
		cancel()
		if err != nil {
			return fmt.Errorf("drain pod %s: %w", host, err)
		}
	}
	if st.To == st.From {
		return nil
	}

	if len(st.Remove) > 0 {
		remove := map[string]bool{}
		for _, h := range st.Remove {
			remove[h] = true
		}
		// Any pod we are NOT removing must be more expensive to delete than the
		// victims, or the ReplicaSet could pick it -- and it may be busy. A stale
		// mark is possible (a previous scale failed on Conflict after marking,
		// and the pod has since been re-registered), so it is cleared, and the
		// scale proceeds only if clearing succeeded.
		clear := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, corev1.PodDeletionCost))
		for _, p := range pods {
			if remove[p.pod.Name] || p.terminating || p.cost > MinDeletionCost {
				continue
			}
			kctx, cancel := c.rpcCtx(ctx)
			_, err := c.kube.CoreV1().Pods(dep.Namespace).Patch(kctx, p.pod.Name, types.StrategicMergePatchType, clear, metav1.PatchOptions{})
			cancel()
			if err != nil {
				return fmt.Errorf("pod %s carries a stale removal mark that could not be cleared; refusing scale-in: %w", p.pod.Name, err)
			}
			c.log.WarnContext(ctx, "cleared a stale removal mark from a pod that is not being removed", "pod", p.pod.Name)
		}
		patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, corev1.PodDeletionCost, strconv.Itoa(MinDeletionCost)))
		for _, h := range st.Remove {
			kctx, cancel := c.rpcCtx(ctx)
			_, err := c.kube.CoreV1().Pods(dep.Namespace).Patch(kctx, h, types.StrategicMergePatchType, patch, metav1.PatchOptions{})
			cancel()
			if err != nil {
				return fmt.Errorf("mark pod %s for removal: %w", h, err)
			}
		}
	}

	// resourceVersion makes this a compare-and-swap: if anything changed the
	// Deployment since it was read (a human, a rollout, a demoted leader that
	// has not noticed yet), the update fails with Conflict and the next cycle
	// re-plans from fresh state instead of overwriting it.
	scale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: dep.Namespace, ResourceVersion: dep.ResourceVersion},
		Spec:       autoscalingv1.ScaleSpec{Replicas: st.To},
	}
	kctx, cancel := c.rpcCtx(ctx)
	_, err := c.kube.AppsV1().Deployments(dep.Namespace).UpdateScale(kctx, dep.Name, scale, metav1.UpdateOptions{})
	cancel()
	if apierrors.IsConflict(err) {
		return fmt.Errorf("deployment changed since it was read; re-planning next cycle: %w", err)
	}
	if err != nil {
		return fmt.Errorf("scale deployment to %d: %w", st.To, err)
	}
	return nil
}

// record persists the step. Idle/cooldown state is written every cycle; an
// EVENT only when the decision changed, so a steady plan is one audit row, not
// one every fifteen seconds.
func (c *Controller) record(ctx context.Context, a *rmv1.ClusterAssignment, b *rmv1.PoolBinding, st Step, mode Mode,
	applied bool, idleUpdate rmv1.RecordScalingRequest_IdleUpdate, idleSince time.Time, overrideAction, overrideReason string,
) {
	action, reason := st.Action(), st.Reason()
	if overrideAction != "" {
		action, reason = overrideAction, overrideReason+" (planned: "+st.Reason()+")"
	}
	eventAction := action
	if !mode.Apply && action != "" && action != "hold" && action != "refused" {
		// In plan mode nothing happened; the event says what WOULD have.
		eventAction = "plan"
	}
	// Drains are recorded by the resource manager inside the drain
	// transaction; repeating them here would double-count.
	if mode.Apply && action == "drain" && overrideAction == "" {
		eventAction = ""
	}
	// Applied changes are always recorded; a repeated plan or hold is not.
	mutated := applied && overrideAction == "" && (st.To != st.From || len(st.Drain) > 0)

	key := b.GetKubernetesNamespace() + "/" + b.GetWorkerDeployment()
	sig := fmt.Sprintf("%s|%s|%d|%d|%v|%v|%s", mode, action, st.From, st.To, st.Remove, st.Drain, reason)
	c.mu.Lock()
	if c.lastDecision[key] == sig && !mutated {
		eventAction = ""
	}
	c.lastDecision[key] = sig
	c.mu.Unlock()

	decisionJSON, _ := json.Marshal(map[string]any{
		"mode": mode.String(), "action": action, "from": st.From, "to": st.To, "drain": st.Drain,
		"remove": st.Remove, "holds": st.Holds, "decision": st.Decision, "idle": st.Idle,
	})
	req := &rmv1.RecordScalingRequest{
		ClusterId: a.GetClusterId(), KubernetesNamespace: b.GetKubernetesNamespace(), WorkerDeployment: b.GetWorkerDeployment(),
		Action: eventAction, Mode: mode.String(), Applied: mutated,
		FromReplicas: st.From, ToReplicas: st.To, Reason: truncate(reason, 1024), Actor: c.cfg.Identity,
		DetailJson: string(decisionJSON), IdleUpdate: idleUpdate, ObservedPlatformRevision: b.GetPlatformRevision(),
		DecisionJson: string(decisionJSON),
	}
	if idleUpdate == rmv1.RecordScalingRequest_IDLE_UPDATE_SET {
		req.IdleSinceUnixMs = idleSince.UnixMilli()
	}
	if mutated {
		// Starting a drain counts as a scale-in for cooldown purposes: without
		// it the next cycle could pick a second batch before the first is gone.
		req.ScaledDirection = "in"
		if st.To > st.From {
			req.ScaledDirection = "out"
		}
	}
	rctx, cancel := c.rpcCtx(ctx)
	defer cancel()
	if _, err := c.rm.RecordScaling(rctx, req); err != nil {
		c.log.WarnContext(ctx, "could not record scaling state", "deployment", key, "error", err)
		// Forget the signature so the event is retried next cycle rather than
		// silently dropped. A lost idle write only delays scale-in (the next
		// cycle restarts the idle clock), which is the safe direction.
		c.mu.Lock()
		delete(c.lastDecision, key)
		c.mu.Unlock()
	}
}
