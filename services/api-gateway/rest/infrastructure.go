// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/khuriwalgroup/kgf-nexus/internal/instances"
	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

// The live infrastructure views (ADR-0037): every control-plane process from
// the service-instance registry, and every in-flight attempt with where its
// workload runs. Both are reads of tables the gateway may read (ADR-0011),
// guarded like the platform-config routes beside them: infrastructure.view.

// ComponentTopology decides who sees the control plane's PLACEMENT -- pod,
// pod namespace, node, cluster and the component detail -- in GET
// .../platform/components (ADR-0037 §4). Component health (state, version,
// start and last-seen times, the readiness reason) is shown to every
// infrastructure.view holder either way.
type ComponentTopology string

const (
	// TopologyPlatformOperators shows placement only to platform operators
	// (platform_operators, docs/MULTITENANCY.md); every other caller sees each
	// instance under an opaque, stable label. The default: on a shared
	// installation the control plane's pods and nodes belong to the operator,
	// not to a tenant.
	TopologyPlatformOperators ComponentTopology = "platform-operators"
	// TopologyInfrastructureViewers shows placement to every
	// infrastructure.view holder: a single-organisation installation, where
	// whoever administers the namespace also runs the cluster.
	TopologyInfrastructureViewers ComponentTopology = "infrastructure-viewers"
)

// ParseComponentTopology reads NEXUS_COMPONENT_TOPOLOGY. Empty is the
// default; anything unrecognised is an error, so a typo cannot silently
// publish the topology or hide it.
func ParseComponentTopology(s string) (ComponentTopology, error) {
	switch t := ComponentTopology(strings.TrimSpace(s)); t {
	case "", TopologyPlatformOperators:
		return TopologyPlatformOperators, nil
	case TopologyInfrastructureViewers:
		return t, nil
	default:
		return "", fmt.Errorf("NEXUS_COMPONENT_TOPOLOGY=%q: want %s or %s", s,
			TopologyPlatformOperators, TopologyInfrastructureViewers)
	}
}

// WithComponentTopology sets who sees control-plane placement. Without it,
// only platform operators do.
func (a *API) WithComponentTopology(t ComponentTopology) *API {
	a.topology = t
	return a
}

// componentStaleAfterSeconds is reported with every listing so a client
// judges staleness by the server's threshold, not a copy of it.
var componentStaleAfterSeconds = int(instances.StaleAfter / time.Second)

type componentsResponse struct {
	GeneratedAt       time.Time `json:"generated_at"`
	StaleAfterSeconds int       `json:"stale_after_seconds"`
	// TopologyVisible says whether pod, node, cluster and detail are shown
	// (ADR-0037 §4), so a console can explain the masked form instead of
	// showing blanks without a reason.
	TopologyVisible bool            `json:"topology_visible"`
	Components      []componentView `json:"components"`
}

type componentView struct {
	Component string                `json:"component"`
	Summary   componentSummary      `json:"summary"`
	Instances []componentInstanceJS `json:"instances"`
}

type componentSummary struct {
	Ready int `json:"ready"`
	Total int `json:"total"`
	Stale int `json:"stale"`
}

type componentInstanceJS struct {
	Instance     string          `json:"instance"`
	PodNamespace string          `json:"pod_namespace"`
	Node         string          `json:"node"`
	Cluster      string          `json:"cluster"`
	Version      string          `json:"version"`
	StartedAt    time.Time       `json:"started_at"`
	LastSeenAt   time.Time       `json:"last_seen_at"`
	State        string          `json:"state"`
	Detail       json.RawMessage `json:"detail"`
}

// Instance states.
const (
	instanceReady    = "ready"
	instanceNotReady = "not_ready"
	instanceStale    = "stale"
)

// platformComponents serves GET /v1/namespaces/{ns}/platform/components.
//
// Every component is listed, in the fixed order, even with no rows: "no
// scheduler has written for a day" must read as zero schedulers, not as a
// component the console forgot. Rows of components this gateway does not know
// (a newer release's) are left out rather than guessed at.
func (a *API) platformComponents(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	at, rows, err := store.GatewayListServiceInstances(r.Context(), a.db, instances.StaleAfter)
	if err != nil {
		a.fail(w, r, "list service instances", err)
		return
	}
	full, err := a.topologyVisible(r, p)
	if err != nil {
		a.fail(w, r, "check platform operator", err)
		return
	}
	writeJSON(w, http.StatusOK, buildComponents(at, rows, full))
}

// topologyVisible applies ADR-0037 §4 to the caller.
func (a *API) topologyVisible(r *http.Request, p authn.Principal) (bool, error) {
	if a.topology == TopologyInfrastructureViewers {
		return true, nil
	}
	return a.isPlatformOperator(r, p)
}

func buildComponents(at time.Time, rows []store.ServiceInstanceView, full bool) componentsResponse {
	byComponent := map[string][]store.ServiceInstanceView{}
	for _, row := range rows {
		byComponent[row.Component] = append(byComponent[row.Component], row)
	}
	out := componentsResponse{
		GeneratedAt:       at,
		StaleAfterSeconds: componentStaleAfterSeconds,
		TopologyVisible:   full,
		Components:        make([]componentView, 0, len(instances.Components())),
	}
	for _, name := range instances.Components() {
		cv := componentView{Component: name, Instances: []componentInstanceJS{}}
		for _, row := range byComponent[name] {
			state := instanceNotReady
			switch {
			case row.Stale:
				state = instanceStale
				cv.Summary.Stale++
			case row.Ready:
				state = instanceReady
				cv.Summary.Ready++
			}
			cv.Summary.Total++
			inst := componentInstanceJS{
				Instance: row.Instance, PodNamespace: row.PodNamespace, Node: row.Node,
				Cluster: row.Cluster, Version: row.Version, StartedAt: row.StartedAt,
				LastSeenAt: row.LastSeenAt, State: state, Detail: row.Detail,
			}
			if !full {
				inst = maskInstance(row, inst)
			}
			if len(inst.Detail) == 0 {
				inst.Detail = json.RawMessage(`{}`)
			}
			cv.Instances = append(cv.Instances, inst)
		}
		out.Components = append(out.Components, cv)
	}
	return out
}

// maskedDetailKeys are the detail keys that describe the instance's HEALTH
// rather than its placement or the platform's activity, and so are shown to
// every infrastructure.view holder.
var maskedDetailKeys = []string{
	instances.DetailReadiness, instances.DetailShuttingDown, instances.DetailIntervalSeconds,
}

// maskInstance is the component-health view of an instance (ADR-0037 §4).
//
// The instance is named by a label derived from its full identity --
// "<component>-<12 hex>" of a SHA-256 -- so it is stable across polls and
// across gateway replicas (a console can follow one replica through a
// restart) and distinct per instance, while saying nothing about pod or
// node names. Pod namespace, node and cluster are emptied, and the detail is
// cut to the health keys: the scheduler's counters are the whole platform's
// activity, every tenant's, not this namespace's.
func maskInstance(row store.ServiceInstanceView, inst componentInstanceJS) componentInstanceJS {
	sum := sha256.Sum256([]byte("nexus-service-instance\x00" + row.Component + "\x00" +
		row.Cluster + "\x00" + row.PodNamespace + "\x00" + row.Instance))
	inst.Instance = row.Component + "-" + hex.EncodeToString(sum[:6])
	inst.PodNamespace, inst.Node, inst.Cluster = "", "", ""

	var full map[string]json.RawMessage
	kept := map[string]json.RawMessage{}
	if json.Unmarshal(row.Detail, &full) == nil {
		for _, k := range maskedDetailKeys {
			if v, ok := full[k]; ok {
				kept[k] = v
			}
		}
	}
	b, err := json.Marshal(kept)
	if err != nil {
		b = []byte(`{}`)
	}
	inst.Detail = b
	return inst
}

type taskRunnersResponse struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Summary     taskRunnersSummary `json:"summary"`
	Runners     []taskRunnerJS     `json:"runners"`
}

type taskRunnersSummary struct {
	Running    int            `json:"running"`
	ByExecutor map[string]int `json:"by_executor"`
	ByCluster  map[string]int `json:"by_cluster"`
}

type taskRunnerJS struct {
	AttemptID           string     `json:"attempt_id"`
	RunID               string     `json:"run_id"`
	NodeID              string     `json:"node_id"`
	Workflow            string     `json:"workflow"`
	WorkerID            string     `json:"worker_id"`
	WorkerHost          string     `json:"worker_host"`
	Executor            string     `json:"executor"`
	Cluster             string     `json:"cluster"`
	K8sNamespace        string     `json:"k8s_namespace"`
	Pod                 string     `json:"pod"`
	K8sNode             string     `json:"k8s_node"`
	State               string     `json:"state"`
	StartedAt           *time.Time `json:"started_at"`
	ProgressMessage     string     `json:"progress_message"`
	PlacementReportedAt *time.Time `json:"placement_reported_at"`
}

// platformTaskRunners serves GET /v1/namespaces/{ns}/platform/task-runners:
// the namespace's in-flight attempts (DISPATCHED and RUNNING), newest
// dispatch first, with where each one's workload runs as its agent reported
// it, and counts over all of them.
//
// Tenant data, scoped through each attempt's run exactly like the run and
// attempt reads; the placement is of the namespace's own workers, so nothing
// here is masked.
func (a *API) platformTaskRunners(w http.ResponseWriter, r *http.Request, p authn.Principal) {
	limit, ok := listLimit(w, r, store.DefaultTaskRunnerPage, store.MaxTaskRunnerPage)
	if !ok {
		return
	}
	at, sum, rows, err := store.GatewayListTaskRunners(r.Context(), a.db, p.Namespace, limit)
	if err != nil {
		a.fail(w, r, "list task runners", err)
		return
	}
	out := taskRunnersResponse{
		GeneratedAt: at,
		Summary: taskRunnersSummary{
			Running: sum.Running, ByExecutor: sum.ByExecutor, ByCluster: sum.ByCluster,
		},
		Runners: make([]taskRunnerJS, 0, len(rows)),
	}
	for _, t := range rows {
		out.Runners = append(out.Runners, taskRunnerJS{
			AttemptID: t.AttemptID, RunID: t.RunID, NodeID: t.NodeID, Workflow: t.Workflow,
			WorkerID: t.WorkerID, WorkerHost: t.WorkerHost,
			Executor: t.Executor, Cluster: t.Cluster, K8sNamespace: t.K8sNamespace,
			Pod: t.Pod, K8sNode: t.K8sNode,
			State: t.State, StartedAt: utcPtr(t.StartedAt), ProgressMessage: t.ProgressMessage,
			PlacementReportedAt: utcPtr(t.PlacementReportedAt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
