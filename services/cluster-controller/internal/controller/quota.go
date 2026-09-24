// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"fmt"
	"math"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// podRequests is the quota-relevant footprint of ONE pod from the template:
// max(sum of containers, largest init container) per resource, which is how
// the API server charges a pod against a ResourceQuota.
func podRequests(spec corev1.PodSpec) (requests, limits corev1.ResourceList) {
	requests, limits = corev1.ResourceList{}, corev1.ResourceList{}
	add := func(dst corev1.ResourceList, src corev1.ResourceList) {
		for k, v := range src {
			q := dst[k]
			q.Add(v)
			dst[k] = q
		}
	}
	for _, c := range spec.Containers {
		add(requests, c.Resources.Requests)
		add(limits, c.Resources.Limits)
	}
	maxInto := func(dst corev1.ResourceList, src corev1.ResourceList) {
		for k, v := range src {
			if q, ok := dst[k]; !ok || v.Cmp(q) > 0 {
				dst[k] = v.DeepCopy()
			}
		}
	}
	for _, c := range spec.InitContainers {
		maxInto(requests, c.Resources.Requests)
		maxInto(limits, c.Resources.Limits)
	}
	return requests, limits
}

// perPodCharge maps a quota resource name to what one more pod would consume.
// ok=false means the quota constrains something this calculation does not
// model per pod (services, secrets, ...), which a worker scale-out does not use.
func perPodCharge(name corev1.ResourceName, req, lim corev1.ResourceList) (q resource.Quantity, ok bool) {
	n := string(name)
	switch {
	case n == "pods" || n == "count/pods":
		return resource.MustParse("1"), true
	case strings.HasPrefix(n, "requests."):
		return req[corev1.ResourceName(strings.TrimPrefix(n, "requests."))], true
	case strings.HasPrefix(n, "limits."):
		return lim[corev1.ResourceName(strings.TrimPrefix(n, "limits."))], true
	case n == "cpu" || n == "memory" || n == "ephemeral-storage":
		// Bare names in a quota mean requests.
		return req[name], true
	}
	return resource.Quantity{}, false
}

// QuotaHeadroom returns how many more pods of this template the namespace's
// ResourceQuotas admit, and which quota binds.
//
// Enforced here rather than left to the API server because an over-quota
// scale-out does not fail: the Deployment accepts the replica count and the
// ReplicaSet then fails to create pods, forever, with the reason buried in
// ReplicaSet events. Bounding the request makes the limit a visible decision.
//
// Quota scopes are ignored, deliberately: treating every quota as applicable
// can only under-scale, never over-commit.
func QuotaHeadroom(quotas []corev1.ResourceQuota, tmpl corev1.PodSpec) (int, string) {
	req, lim := podRequests(tmpl)
	headroom, why := math.MaxInt32, ""
	for _, rq := range quotas {
		for name, hard := range rq.Spec.Hard {
			charge, ok := perPodCharge(name, req, lim)
			if !ok {
				continue
			}
			used, known := rq.Status.Used[name]
			if !known {
				// The quota controller has not computed usage yet (a quota
				// created moments ago). Unknown usage is not zero usage.
				return 0, fmt.Sprintf("ResourceQuota %s usage for %s is not yet calculated", rq.Name, name)
			}
			avail := hard.DeepCopy()
			avail.Sub(used)
			if charge.IsZero() {
				// A quota on a compute resource the template does not declare
				// makes the API server reject every new pod. Report it as zero
				// headroom rather than letting the ReplicaSet fail silently.
				if n := string(name); n != "pods" && n != "count/pods" {
					return 0, fmt.Sprintf("ResourceQuota %s limits %s but the worker pod template declares none, so new pods would be rejected", rq.Name, name)
				}
				continue
			}
			var n int64
			if avail.Sign() > 0 {
				n = avail.MilliValue() / charge.MilliValue()
			}
			if int(n) < headroom {
				headroom = int(n)
				why = fmt.Sprintf("ResourceQuota %s: %s used %s of %s (one worker needs %s)", rq.Name, name, used.String(), hard.String(), charge.String())
			}
		}
	}
	return headroom, why
}
