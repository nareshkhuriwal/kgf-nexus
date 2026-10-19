// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0
package platform

import "fmt"

// PodTemplate contains only administrator-approved settings. No host namespaces,
// hostPath, privileged containers, cloud keys, or user-supplied pod YAML.
func PodTemplate(p PodProfile) map[string]any {
	resources := map[string]string{"cpu": fmt.Sprintf("%dm", p.CPU), "memory": fmt.Sprintf("%dMi", p.Memory), "ephemeral-storage": fmt.Sprintf("%dMi", p.EphemeralStorage)}
	if p.GPU > 0 {
		resources["nvidia.com/gpu"] = fmt.Sprint(p.GPU)
	}
	labels := map[string]string{"nexus.io/profile": p.Name}
	if p.Cloud == "azure" {
		labels["azure.workload.identity/use"] = "true"
	}
	tolerations := []map[string]string{}
	for _, t := range p.Tolerations {
		tolerations = append(tolerations, map[string]string{"key": t.Key, "operator": "Equal", "value": t.Value, "effect": t.Effect})
	}
	return map[string]any{"metadata": map[string]any{"labels": labels}, "spec": map[string]any{
		"serviceAccountName": p.ServiceAccount, "automountServiceAccountToken": false, "restartPolicy": "Never",
		"terminationGracePeriodSeconds": p.GraceSeconds, "nodeSelector": p.NodeSelector, "tolerations": tolerations,
		"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001, "fsGroup": 10001, "seccompProfile": map[string]string{"type": "RuntimeDefault"}},
		"containers": []any{map[string]any{"name": "task", "image": p.Image, "resources": map[string]any{"requests": resources, "limits": resources},
			"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
			"volumeMounts":    []any{map[string]any{"name": "scratch", "mountPath": "/tmp"}}}},
		"volumes": []any{map[string]any{"name": "scratch", "emptyDir": map[string]any{"sizeLimit": fmt.Sprintf("%dMi", p.EphemeralStorage)}}},
	}}
}

// JobTemplate is a reviewable execution template, not an already submitted task.
// Nexus owns retries; Kubernetes must not create extra retry attempts.
func JobTemplate(p PodProfile) map[string]any {
	return map[string]any{"apiVersion": "batch/v1", "kind": "Job", "metadata": map[string]any{"generateName": "nexus-task-", "namespace": p.KubernetesNamespace}, "spec": map[string]any{"backoffLimit": 0, "activeDeadlineSeconds": p.DeadlineSeconds, "ttlSecondsAfterFinished": 3600, "template": PodTemplate(p)}}
}

// SchedulerHPA is a scheduler Deployment's HorizontalPodAutoscaler built from
// the saved policy: the manifest an operator can apply instead of the chart's.
// It carries the labels the cluster controller finds schedulers by and the
// ownership opt-in (HPAOwnerAnnotation), so the controller keeps it at the
// saved settings in apply mode (ADR-0036); remove the annotation to keep it
// hand-managed.
func SchedulerHPA(s Scheduler, namespace, deployment string) map[string]any {
	return map[string]any{"apiVersion": "autoscaling/v2", "kind": "HorizontalPodAutoscaler", "metadata": map[string]any{"name": deployment, "namespace": namespace,
		"labels":      map[string]string{"app.kubernetes.io/part-of": "kgf-nexus", "app.kubernetes.io/component": "scheduler"},
		"annotations": map[string]string{HPAOwnerAnnotation: HPAOwnerValue}}, "spec": map[string]any{"scaleTargetRef": map[string]string{"apiVersion": "apps/v1", "kind": "Deployment", "name": deployment}, "minReplicas": s.MinReplicas, "maxReplicas": s.MaxReplicas, "metrics": []any{map[string]any{"type": "Resource", "resource": map[string]any{"name": "cpu", "target": map[string]any{"type": "Utilization", "averageUtilization": s.TargetCPU}}}}, "behavior": map[string]any{"scaleDown": map[string]any{"stabilizationWindowSeconds": s.ScaleDownSeconds, "policies": []any{map[string]any{"type": "Pods", "value": 1, "periodSeconds": 60}}}}}}
}
