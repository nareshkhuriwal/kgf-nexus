// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package executor

// PodPlacement is where Kubernetes put an attempt's pod (ADR-0037). Node is
// empty until the scheduler binds the pod.
type PodPlacement struct {
	Namespace string
	Pod       string
	Node      string
}

// PlacementSource is implemented by executors that report their workload's
// placement themselves, through TaskSpec.OnPlacement, because the workload
// runs somewhere the agent does not (a pod the cluster schedules onto any
// node). For every other executor the workload runs beside the agent, and the
// runner reports the agent's own pod and node instead.
type PlacementSource interface {
	ReportsPlacement() bool
}
