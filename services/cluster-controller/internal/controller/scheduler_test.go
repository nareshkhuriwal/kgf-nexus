// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/metrics"
	"github.com/khuriwalgroup/kgf-nexus/internal/metrics/metricstest"
	"github.com/khuriwalgroup/kgf-nexus/internal/platform"
	rmv1 "github.com/khuriwalgroup/kgf-nexus/services/resource-manager/gen/nexus/rm/v1"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

const (
	cpNS     = "kgf-nexus"
	schedHPA = "scheduler"
	hpaUID   = "hpa-uid-1"
)

var hpasGVR = schema.GroupVersionResource{Group: "autoscaling", Version: "v2", Resource: "horizontalpodautoscalers"}

func i32(v int32) *int32 { return &v }

// schedulerHPA is a chart-shaped scheduler HPA: min 2, max 6, CPU 65% (with a
// memory metric beside it), scale-down window 300 s, a scale-up policy, and
// the ownership opt-in.
func schedulerHPA(owned bool) *autoscalingv2.HorizontalPodAutoscaler {
	h := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: schedHPA, Namespace: cpNS, UID: types.UID(hpaUID), ResourceVersion: "10", Generation: 1,
			Labels: map[string]string{"app.kubernetes.io/part-of": "kgf-nexus", "app.kubernetes.io/component": "scheduler", "keep": "me"}},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "scheduler"},
			MinReplicas:    i32(2), MaxReplicas: 6,
			Metrics: []autoscalingv2.MetricSpec{
				{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: i32(80)}}},
				{Type: autoscalingv2.ResourceMetricSourceType, Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{Type: autoscalingv2.UtilizationMetricType, AverageUtilization: i32(65)}}},
			},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{Policies: []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 2, PeriodSeconds: 30}}},
				ScaleDown: &autoscalingv2.HPAScalingRules{StabilizationWindowSeconds: i32(300),
					Policies: []autoscalingv2.HPAScalingPolicy{{Type: autoscalingv2.PodsScalingPolicy, Value: 1, PeriodSeconds: 60}}},
			},
		},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 2, DesiredReplicas: 2},
	}
	if owned {
		h.Annotations = map[string]string{platform.HPAOwnerAnnotation: platform.HPAOwnerValue}
	}
	return h
}

// withScheduler adds the scheduler Deployment and h to the cluster, and the
// API server's handling of HPA patches.
func (fc *fakeCluster) withScheduler(h *autoscalingv2.HorizontalPodAutoscaler) *fakeCluster {
	fc.t.Helper()
	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "scheduler", Namespace: cpNS, Generation: 1, ResourceVersion: "1",
			Labels: map[string]string{"app.kubernetes.io/part-of": "kgf-nexus", "app.kubernetes.io/component": "scheduler"}},
		Spec:   appsv1.DeploymentSpec{Replicas: &two},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1, Replicas: 2, UpdatedReplicas: 2, AvailableReplicas: 2, ReadyReplicas: 2},
	}
	for _, o := range []runtime.Object{dep, h} {
		if err := fc.kube.Tracker().Add(o); err != nil {
			fc.t.Fatal(err)
		}
	}
	if !fc.hpaReactor {
		fc.hpaReactor = true
		fc.kube.PrependReactor("patch", "horizontalpodautoscalers", fc.hpaPatch)
	}
	return fc
}

// hpaPatch emulates the API server's patch of an HPA: a resourceVersion in
// the patch is a precondition (409 Conflict when stale), the JSON patch's
// test operations are applied by the same JSON-patch library, and a
// successful write moves resourceVersion and generation.
func (fc *fakeCluster) hpaPatch(action k8stesting.Action) (bool, runtime.Object, error) {
	pa := action.(k8stesting.PatchAction)
	fc.mu.Lock()
	fc.hpaPatchCalls++
	fc.hpaPatches = append(fc.hpaPatches, pa.GetPatch())
	hook := fc.beforeHPAPatch
	fc.mu.Unlock()
	if hook != nil {
		if err := hook(pa); err != nil {
			return true, nil, err
		}
	}
	obj, err := fc.kube.Tracker().Get(hpasGVR, pa.GetNamespace(), pa.GetName())
	if err != nil {
		return true, nil, err
	}
	stored := obj.(*autoscalingv2.HorizontalPodAutoscaler)
	if pa.GetPatchType() == types.JSONPatchType {
		var ops []struct {
			Op, Path string
			Value    any
		}
		if err := json.Unmarshal(pa.GetPatch(), &ops); err != nil {
			return true, nil, apierrors.NewBadRequest(err.Error())
		}
		for _, op := range ops {
			if op.Path == "/metadata/resourceVersion" && op.Value != stored.ResourceVersion {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"},
					stored.Name, fmt.Errorf("the object has been modified (resourceVersion %v, stored %s)", op.Value, stored.ResourceVersion))
			}
		}
	}
	if _, _, err := k8stesting.ObjectReaction(fc.kube.Tracker())(action); err != nil {
		return true, nil, err
	}
	obj, err = fc.kube.Tracker().Get(hpasGVR, pa.GetNamespace(), pa.GetName())
	if err != nil {
		return true, nil, err
	}
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler).DeepCopy()
	rv, _ := strconv.Atoi(stored.ResourceVersion)
	h.ResourceVersion, h.Generation = strconv.Itoa(rv+1), stored.Generation+1
	if err := fc.kube.Tracker().Update(hpasGVR, h, h.Namespace); err != nil {
		return true, nil, err
	}
	return true, h, nil
}

// touchHPA is another writer: it bumps the stored resourceVersion (as the
// HPA controller's status write does), optionally changing the object.
func (fc *fakeCluster) touchHPA(mutate func(*autoscalingv2.HorizontalPodAutoscaler)) error {
	obj, err := fc.kube.Tracker().Get(hpasGVR, cpNS, schedHPA)
	if err != nil {
		return err
	}
	h := obj.(*autoscalingv2.HorizontalPodAutoscaler).DeepCopy()
	rv, _ := strconv.Atoi(h.ResourceVersion)
	h.ResourceVersion = strconv.Itoa(rv + 1)
	if mutate != nil {
		mutate(h)
	}
	return fc.kube.Tracker().Update(hpasGVR, h, cpNS)
}

func (fc *fakeCluster) hpa() *autoscalingv2.HorizontalPodAutoscaler {
	fc.t.Helper()
	h, err := fc.kube.AutoscalingV2().HorizontalPodAutoscalers(cpNS).Get(context.Background(), schedHPA, metav1.GetOptions{})
	if err != nil {
		fc.t.Fatal(err)
	}
	return h
}

func (fc *fakeCluster) patchCalls() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.hpaPatchCalls
}

func withSettings(rm *fakeRM, min, max, cpu, window int32, revision int64) {
	rm.assignment.SchedulerAutoscaling = &rmv1.SchedulerAutoscaling{ConfigRevision: revision,
		Desired: &rmv1.SchedulerAutoscalingSettings{MinReplicas: min, MaxReplicas: max, TargetCpuPercent: cpu, ScaleDownStabilizationSeconds: window}}
}

func settingsOf(t *testing.T, h *autoscalingv2.HorizontalPodAutoscaler) platform.HPASettings {
	t.Helper()
	s, _ := hpaSettings(h)
	return s
}

func reportedHPA(t *testing.T, rm *fakeRM) map[string]any {
	t.Helper()
	rep := rm.reports[len(rm.reports)-1]
	for _, o := range rep.Objects {
		if o.Kind == "HorizontalPodAutoscaler" && o.Name == schedHPA {
			var m map[string]any
			if err := json.Unmarshal([]byte(o.SummaryJson), &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
	}
	t.Fatalf("scheduler HPA not in the report")
	return nil
}

func TestSchedulerHPAPlanModeReportsAndChangesNothing(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("plan")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	c := newController(t, fc, rm, true, clk)

	before := fc.mutations()
	for i := 0; i < 3; i++ {
		mustRun(t, c)
		clk.Advance(time.Minute)
	}
	if fc.mutations() != before || fc.patchCalls() != 0 {
		t.Fatalf("plan mode wrote the cluster: %v", fc.kube.Actions())
	}
	if ev := rm.hpaEvents(); !slices.Equal(ev, []string{"hpa_plan"}) {
		t.Fatalf("events = %v, want exactly one hpa_plan for an unchanged plan", ev)
	}
	tgt := rm.hpaTargets()[schedHPA]
	if tgt.Result != "plan" || tgt.Deployment != "scheduler" || !tgt.Managed || tgt.ConfigRevision != 4 ||
		tgt.Observed.MaxReplicas != 6 || !strings.Contains(tgt.Reason, "max 6→10") || !strings.Contains(tgt.Reason, "revision 4") {
		t.Fatalf("plan report %+v", tgt)
	}
	first := rm.hpaReports[0].Targets[0]
	if first.From.GetMaxReplicas() != 6 || first.To.GetMaxReplicas() != 10 || first.Applied {
		t.Fatalf("plan event %+v", first)
	}
	if settingsOf(t, fc.hpa()) != (platform.HPASettings{MinReplicas: 2, MaxReplicas: 6, TargetCPUPercent: 65, ScaleDownStabilizationSeconds: 300}) {
		t.Fatalf("HPA changed in plan mode")
	}
	if hpa := reportedHPA(t, rm); hpa["managed"] != true || hpa["scale_down_stabilization_seconds"] != float64(300) ||
		hpa["current_replicas"] != float64(2) {
		t.Fatalf("HPA observation summary %v", hpa)
	}
}

func TestSchedulerHPAApplyPatchesOnlyTheFourSettings(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	c := newController(t, fc, rm, true, clk)
	before := fc.hpa()

	mustRun(t, c)
	after := fc.hpa()
	want := platform.HPASettings{MinReplicas: 3, MaxReplicas: 10, TargetCPUPercent: 70, ScaleDownStabilizationSeconds: 600}
	if got := settingsOf(t, after); got != want {
		t.Fatalf("HPA settings %+v, want %+v", got, want)
	}
	// Everything else is exactly as it was.
	restored := after.DeepCopy()
	restored.Spec.MinReplicas, restored.Spec.MaxReplicas = before.Spec.MinReplicas, before.Spec.MaxReplicas
	restored.Spec.Metrics[1].Resource.Target.AverageUtilization = before.Spec.Metrics[1].Resource.Target.AverageUtilization
	restored.Spec.Behavior.ScaleDown.StabilizationWindowSeconds = before.Spec.Behavior.ScaleDown.StabilizationWindowSeconds
	if b, a := mustJSON(t, before.Spec), mustJSON(t, restored.Spec); b != a {
		t.Fatalf("patch touched more than the four settings:\nbefore %s\nafter  %s", b, a)
	}
	if !sameMap(after.Labels, before.Labels) || !sameMap(after.Annotations, before.Annotations) {
		t.Fatalf("metadata changed: %v %v", after.Labels, after.Annotations)
	}

	patch := string(fc.hpaPatches[0])
	for _, wantOp := range []string{`{"op":"test","path":"/metadata/uid","value":"hpa-uid-1"}`,
		`{"op":"test","path":"/metadata/annotations/nexus.khuriwalgroup.com~1autoscaling-owner","value":"cluster-controller"}`,
		`{"op":"replace","path":"/metadata/resourceVersion","value":"10"}`,
		`{"op":"test","path":"/spec/metrics/1/resource/name","value":"cpu"}`} {
		if !strings.Contains(patch, wantOp) {
			t.Errorf("patch lacks %s:\n%s", wantOp, patch)
		}
	}
	if ev := rm.hpaEvents(); !slices.Equal(ev, []string{"hpa_update"}) {
		t.Fatalf("events = %v", ev)
	}
	tgt := rm.hpaTargets()[schedHPA]
	if tgt.Result != "in_sync" || !tgt.Applied || tgt.From.GetMinReplicas() != 2 || tgt.To.GetMinReplicas() != 3 ||
		tgt.Observed.GetMaxReplicas() != 10 || !strings.Contains(tgt.Reason, "set min 2→3, max 6→10") {
		t.Fatalf("applied report %+v", tgt)
	}
	// The cycle's observation is the patched object, not the one listed.
	if hpa := reportedHPA(t, rm); hpa["max_replicas"] != float64(10) || hpa["target_cpu_percent"] != float64(70) {
		t.Fatalf("reported HPA %v is the pre-patch object", hpa)
	}
	if rm.assignment.SchedulerAutoscaling.Hpas[0].LastAppliedAtUnixMs != clk.Now().UnixMilli() {
		t.Fatalf("last applied time not persisted: %+v", rm.assignment.SchedulerAutoscaling.Hpas)
	}

	// In sync: the next cycles neither patch nor record events.
	clk.Advance(5 * time.Minute)
	mustRun(t, c)
	if fc.patchCalls() != 1 || len(rm.hpaEvents()) != 1 || rm.hpaTargets()[schedHPA].Result != "in_sync" {
		t.Fatalf("an in-sync HPA was patched again (%d) or re-recorded (%v)", fc.patchCalls(), rm.hpaEvents())
	}
}

func TestSchedulerHPABoundedStepsAndPatchIntervalSurviveRestart(t *testing.T) {
	clk := start()
	h := schedulerHPA(true)
	h.Spec.MinReplicas, h.Spec.MaxReplicas = i32(1), 20
	fc, rm := newFakeCluster(t, 1).withScheduler(h), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 1, 2, 65, 300, 9)
	c := newController(t, fc, rm, true, clk)

	mustRun(t, c)
	if got := fc.hpa().Spec.MaxReplicas; got != 10 {
		t.Fatalf("first step max = %d, want 10 (at most halved)", got)
	}
	if tgt := rm.hpaTargets()[schedHPA]; tgt.Result != "applying" || !strings.Contains(tgt.Reason, "further bounded steps") {
		t.Fatalf("after a partial step %+v", tgt)
	}

	// Inside the patch interval nothing is patched -- not even by a restarted
	// controller, because the last patch time is persisted.
	clk.Advance(30 * time.Second)
	mustRun(t, newController(t, fc, rm, true, clk))
	if fc.patchCalls() != 1 {
		t.Fatalf("patched inside the interval after a restart")
	}
	if tgt := rm.hpaTargets()[schedHPA]; tgt.Result != "applying" || !strings.Contains(tgt.Reason, "in 30s") {
		t.Fatalf("waiting report %+v", tgt)
	}

	var maxes []int32
	for i := 0; i < 5; i++ {
		clk.Advance(time.Minute)
		mustRun(t, c)
		maxes = append(maxes, fc.hpa().Spec.MaxReplicas)
	}
	if !slices.Equal(maxes, []int32{5, 3, 2, 2, 2}) {
		t.Fatalf("max over successive intervals = %v, want 5,3,2 then steady", maxes)
	}
	if ev := rm.hpaEvents(); !slices.Equal(ev, []string{"hpa_update", "hpa_update", "hpa_update", "hpa_update"}) {
		t.Fatalf("events = %v, want one hpa_update per step", ev)
	}
	if rm.hpaTargets()[schedHPA].Result != "in_sync" {
		t.Fatalf("not converged: %+v", rm.hpaTargets()[schedHPA])
	}
}

func TestSchedulerHPAConflictIsRetriedOnAFreshRead(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	// The HPA controller writes status between the list and the first patch.
	first := true
	fc.beforeHPAPatch = func(k8stesting.PatchAction) error {
		if first {
			first = false
			return fc.touchHPA(func(h *autoscalingv2.HorizontalPodAutoscaler) { h.Status.CurrentReplicas = 3 })
		}
		return nil
	}
	c := newController(t, fc, rm, true, clk)
	mustRun(t, c)
	if fc.patchCalls() != 2 {
		t.Fatalf("patch attempts = %d, want a conflict then a success", fc.patchCalls())
	}
	if !strings.Contains(string(fc.hpaPatches[1]), `"path":"/metadata/resourceVersion","value":"11"`) {
		t.Fatalf("the retry was not computed from a fresh read: %s", fc.hpaPatches[1])
	}
	got := fc.hpa()
	if settingsOf(t, got) != (platform.HPASettings{MinReplicas: 3, MaxReplicas: 10, TargetCPUPercent: 70, ScaleDownStabilizationSeconds: 600}) ||
		got.Status.CurrentReplicas != 3 {
		t.Fatalf("after retry %+v (status %+v)", got.Spec, got.Status)
	}
	if len(rm.errs) != 0 {
		t.Fatalf("a retried conflict was reported as an error: %+v", rm.errs)
	}
}

func TestSchedulerHPAPersistentConflictIsRefusedAndReported(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	fc.beforeHPAPatch = func(k8stesting.PatchAction) error { return fc.touchHPA(nil) }
	c := newController(t, fc, rm, true, clk)
	err := c.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another writer is active") {
		t.Fatalf("err = %v, want the persistent conflict to fail the cycle", err)
	}
	if fc.patchCalls() != maxHPAPatchAttempts {
		t.Fatalf("attempts = %d, want %d", fc.patchCalls(), maxHPAPatchAttempts)
	}
	if settingsOf(t, fc.hpa()).MaxReplicas != 6 {
		t.Fatalf("a conflicting patch was applied")
	}
	if len(rm.errs) != 1 || rm.errs[0].Target != "hpa kgf-nexus/scheduler" {
		t.Fatalf("reconcile error not recorded: %+v", rm.errs)
	}
	tgt := rm.hpaTargets()[schedHPA]
	if tgt.Result != "blocked" || tgt.Action != "hpa_refused" || tgt.Applied || tgt.Error == "" {
		t.Fatalf("refusal report %+v", tgt)
	}
	if rep := rm.reports[len(rm.reports)-1]; rep.ObservedRevision != 0 || !strings.Contains(rep.LastError, "scheduler HPA") {
		t.Fatalf("a failed HPA apply acknowledged the revision: %+v", rep)
	}
}

func TestSchedulerHPAReplacedWhileBeingPatchedIsNotPatched(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	fc.beforeHPAPatch = func(k8stesting.PatchAction) error {
		// Deleted and recreated (same name, new UID) between list and patch.
		return fc.touchHPA(func(h *autoscalingv2.HorizontalPodAutoscaler) { h.UID = "hpa-uid-2" })
	}
	c := newController(t, fc, rm, true, clk)
	if err := c.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "replaced while being patched") {
		t.Fatalf("err = %v", err)
	}
	if fc.patchCalls() != 1 || settingsOf(t, fc.hpa()).MaxReplicas != 6 {
		t.Fatalf("patched a replacement object it never observed (calls %d)", fc.patchCalls())
	}
}

func TestSchedulerHPANotOwnedIsNeverTouched(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(false)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	c := newController(t, fc, rm, true, clk)
	before := fc.mutations()
	for i := 0; i < 3; i++ {
		mustRun(t, c) // drift by configuration is not a failed cycle
		clk.Advance(2 * time.Minute)
	}
	if fc.patchCalls() != 0 || fc.mutations() != before {
		t.Fatalf("wrote an HPA without the ownership opt-in")
	}
	tgt := rm.hpaTargets()[schedHPA]
	if tgt.Result != "blocked" || tgt.Managed || !strings.Contains(tgt.Error, platform.HPAOwnerAnnotation) {
		t.Fatalf("report %+v", tgt)
	}
	if ev := rm.hpaEvents(); !slices.Equal(ev, []string{"hpa_refused"}) {
		t.Fatalf("events = %v, want one hpa_refused for a steady state", ev)
	}
	if len(rm.errs) != 0 {
		t.Fatalf("an unowned HPA is not a reconcile error: %+v", rm.errs)
	}
}

func TestSchedulerHPAForbiddenPatchIsRecordedAsReconcileError(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	fc.beforeHPAPatch = func(k8stesting.PatchAction) error {
		return apierrors.NewForbidden(schema.GroupResource{Group: "autoscaling", Resource: "horizontalpodautoscalers"}, schedHPA,
			errors.New(`User "system:serviceaccount:kgf-nexus:nexus-cluster-controller" cannot patch resource`))
	}
	c := newController(t, fc, rm, true, clk)
	if err := c.RunOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("err = %v", err)
	}
	if len(rm.errs) != 1 || rm.errs[0].Target != "hpa kgf-nexus/scheduler" ||
		!strings.Contains(rm.errs[0].Message, "horizontalpodautoscalers patch in kgf-nexus") {
		t.Fatalf("reconcile error %+v", rm.errs)
	}
	tgt := rm.hpaTargets()[schedHPA]
	if tgt.Result != "blocked" || tgt.Action != "hpa_refused" || !strings.Contains(tgt.Error, "forbidden") {
		t.Fatalf("report %+v", tgt)
	}
	if settingsOf(t, fc.hpa()).MaxReplicas != 6 {
		t.Fatalf("HPA changed despite the refusal")
	}
}

func TestSchedulerHPAUnsavedSettingsAndOldResourceManagerChangeNothing(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 1, 4, 65, 300, 0) // the defaults, never saved
	mustRun(t, newController(t, fc, rm, true, clk))
	if fc.patchCalls() != 0 {
		t.Fatalf("applied defaults nobody saved")
	}
	if tgt := rm.hpaTargets()[schedHPA]; tgt.Result != "not_configured" || tgt.Action != "" || !strings.Contains(tgt.Error, "never saved") {
		t.Fatalf("report %+v", tgt)
	}

	// A resource manager that predates ADR-0036: no settings, no RPC.
	rm.assignment.SchedulerAutoscaling = nil
	reports := len(rm.hpaReports)
	mustRun(t, newController(t, fc, rm, true, clk))
	if fc.patchCalls() != 0 || len(rm.hpaReports) != reports {
		t.Fatalf("acted on scheduler HPAs without settings from the resource manager")
	}
}

func TestSchedulerHPAWithoutCPUMetricIsNotChanged(t *testing.T) {
	clk := start()
	h := schedulerHPA(true)
	h.Spec.Metrics = h.Spec.Metrics[:1] // memory only
	fc, rm := newFakeCluster(t, 1).withScheduler(h), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	mustRun(t, newController(t, fc, rm, true, clk))
	if fc.patchCalls() != 0 {
		t.Fatalf("added a CPU metric to an HPA that scales on something else")
	}
	if tgt := rm.hpaTargets()[schedHPA]; tgt.Result != "blocked" || !strings.Contains(tgt.Error, "CPU utilization metric") ||
		tgt.Observed.GetTargetCpuPercent() != 0 {
		t.Fatalf("report %+v", tgt)
	}
}

func TestSchedulerHPAReportPrunedWhenItsHPAIsGone(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(true)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	c := newController(t, fc, rm, true, clk)
	mustRun(t, c)
	if len(rm.assignment.SchedulerAutoscaling.Hpas) != 1 {
		t.Fatalf("memory %+v", rm.assignment.SchedulerAutoscaling.Hpas)
	}
	if err := fc.kube.Tracker().Delete(hpasGVR, cpNS, schedHPA); err != nil {
		t.Fatal(err)
	}
	mustRun(t, c)
	last := rm.hpaReports[len(rm.hpaReports)-1]
	if !last.Complete || len(last.Targets) != 0 || len(rm.assignment.SchedulerAutoscaling.Hpas) != 0 {
		t.Fatalf("a deleted HPA's state was kept: %+v / %+v", last, rm.assignment.SchedulerAutoscaling.Hpas)
	}
}

func TestHPAPatchAddsMissingBehaviorWithoutTouchingTheRest(t *testing.T) {
	h := schedulerHPA(true)
	h.Spec.Behavior = nil
	from, cpu := hpaSettings(h)
	if from.ScaleDownStabilizationSeconds != platform.DefaultScaleDownStabilizationSeconds || cpu != 1 {
		t.Fatalf("defaults %+v cpu index %d", from, cpu)
	}
	to := from
	to.ScaleDownStabilizationSeconds = 900
	b, err := hpaPatch(h, cpu, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var ops []jsonPatchOp
	if err := json.Unmarshal(b, &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 4 || ops[3].Op != "add" || ops[3].Path != "/spec/behavior" ||
		mustJSON(t, ops[3].Value) != `{"scaleDown":{"stabilizationWindowSeconds":900}}` {
		t.Fatalf("ops %+v", ops)
	}
	h.Spec.Behavior = &autoscalingv2.HorizontalPodAutoscalerBehavior{}
	b, _ = hpaPatch(h, cpu, from, to)
	if !strings.Contains(string(b), `"path":"/spec/behavior/scaleDown","value":{"stabilizationWindowSeconds":900}`) {
		t.Fatalf("patch without scaleDown: %s", b)
	}
	h.UID = ""
	if _, err := hpaPatch(h, cpu, from, to); err == nil {
		t.Fatal("an unconditional patch (no uid) was built")
	}
}

func TestSchedulerHPAMetrics(t *testing.T) {
	clk := start()
	fc, rm := newFakeCluster(t, 1).withScheduler(schedulerHPA(false)), newFakeRM("apply")
	registerAll(fc, rm)
	withSettings(rm, 3, 10, 70, 600, 4)
	c := newController(t, fc, rm, true, clk)
	reg := metrics.NewRegistry()
	c.SetMetrics(NewMetrics(reg))

	mustRun(t, c)
	fams := metricstest.Scrape(t, reg.Handler(), "/metrics")
	if s := metricstest.Find(fams, "nexus_controller_scheduler_hpa_blocked", map[string]string{"hpa": schedHPA}); s == nil ||
		s.GetGauge().GetValue() != 1 {
		t.Fatalf("blocked gauge for an unowned, differing HPA = %v, want 1", s)
	}

	// Opted in: applied, no longer blocked.
	if err := fc.touchHPA(func(h *autoscalingv2.HorizontalPodAutoscaler) {
		h.Annotations = map[string]string{platform.HPAOwnerAnnotation: platform.HPAOwnerValue}
	}); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	mustRun(t, c)
	fams = metricstest.Scrape(t, reg.Handler(), "/metrics")
	if s := metricstest.Find(fams, "nexus_controller_scheduler_hpa_blocked", map[string]string{"hpa": schedHPA}); s == nil ||
		s.GetGauge().GetValue() != 0 {
		t.Fatalf("blocked gauge after apply = %v, want 0", s)
	}
	for action, want := range map[string]float64{"hpa_refused": 1, "hpa_update": 1} {
		if s := metricstest.Find(fams, "nexus_controller_scheduler_hpa_actions_total",
			map[string]string{"action": action, "mode": "apply"}); s == nil || s.GetCounter().GetValue() != want {
			t.Errorf("actions{%s} = %v, want %v", action, s, want)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
