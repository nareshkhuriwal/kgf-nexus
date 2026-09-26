// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"net/http"
	"sort"
	"strconv"

	"github.com/khuriwalgroup/kgf-nexus/internal/store"
	"github.com/khuriwalgroup/kgf-nexus/services/api-gateway/authn"
)

func assetPayload(x store.Asset) map[string]any {
	return map[string]any{
		"uri": x.URI, "name": x.Name, "first_seen_at": x.FirstSeenAt, "last_declared_at": x.LastDeclaredAt,
		"last_materialized_at": x.LastMaterializedAt, "last_materialized_run_id": x.LastMaterializedRunID,
		"producers": x.Producers, "consumers": x.Consumers,
	}
}

func (a *API) listAssets(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := store.AssetList(r.Context(), a.db, r.PathValue("ns"), r.URL.Query().Get("q"), limit)
	if err != nil {
		a.failConfig(w, r, "list assets", err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, x := range rows {
		out = append(out, assetPayload(x))
	}
	writeJSON(w, http.StatusOK, map[string]any{"assets": out})
}

func tasksPayload(ts []store.AssetTask) []map[string]any {
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{"workflow": t.Workflow, "node_id": t.NodeID, "last_run_id": t.LastRunID,
			"last_recorded_at": t.LastRecorded, "runs": t.Runs})
	}
	return out
}

// getAsset answers "who produces this, who consumes it, when was it last built".
func (a *API) getAsset(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns, uri := r.PathValue("ns"), r.URL.Query().Get("uri")
	if uri == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "uri is required")
		return
	}
	x, err := store.AssetGet(r.Context(), a.db, ns, uri)
	if err != nil {
		a.failConfig(w, r, "get asset", err)
		return
	}
	producers, err := store.AssetTasks(r.Context(), a.db, ns, uri, "produces")
	if err != nil {
		a.failConfig(w, r, "asset producers", err)
		return
	}
	consumers, err := store.AssetTasks(r.Context(), a.db, ns, uri, "consumes")
	if err != nil {
		a.failConfig(w, r, "asset consumers", err)
		return
	}
	mats, err := store.AssetMaterializations(r.Context(), a.db, ns, uri, 20)
	if err != nil {
		a.failConfig(w, r, "asset materializations", err)
		return
	}
	matOut := make([]map[string]any, 0, len(mats))
	for _, m := range mats {
		matOut = append(matOut, map[string]any{"run_id": m.RunID, "node_id": m.NodeID, "workflow": m.Workflow,
			"materialized_at": m.MaterializedAt, "run_state": m.RunState})
	}
	body := assetPayload(x)
	body["producer_tasks"] = tasksPayload(producers)
	body["consumer_tasks"] = tasksPayload(consumers)
	body["materializations"] = matOut
	if len(matOut) > 0 {
		body["latest_materialization"] = matOut[0]
	} else {
		body["latest_materialization"] = nil
	}
	writeJSON(w, http.StatusOK, body)
}

// assetGraph returns the declared lineage graph: asset nodes and task nodes,
// task->asset for produces and asset->task for consumes. With ?uri= it is the
// neighbourhood of that asset to ?depth= hops (default 2, max 6).
func (a *API) assetGraph(w http.ResponseWriter, r *http.Request, _ authn.Principal) {
	if !a.eventsEnabled(w) {
		return
	}
	ns := r.PathValue("ns")
	edges, truncated, err := store.AssetLineageEdges(r.Context(), a.db, ns, 5000)
	if err != nil {
		a.failConfig(w, r, "lineage edges", err)
		return
	}
	type gnode struct {
		ID, Kind, Label, Workflow, NodeID, URI string
	}
	type gedge struct{ From, To string }
	nodes := map[string]gnode{}
	adj := map[string][]string{}
	var all []gedge
	for _, e := range edges {
		aid := "asset:" + e.AssetURI
		tid := "task:" + e.Workflow + "/" + e.NodeID
		label := e.AssetName
		if label == "" {
			label = e.AssetURI
		}
		if n, ok := nodes[aid]; !ok || n.Label == n.URI {
			nodes[aid] = gnode{ID: aid, Kind: "asset", Label: label, URI: e.AssetURI}
		}
		nodes[tid] = gnode{ID: tid, Kind: "task", Label: e.Workflow + " · " + e.NodeID, Workflow: e.Workflow, NodeID: e.NodeID}
		ge := gedge{From: tid, To: aid}
		if e.Direction == "consumes" {
			ge = gedge{From: aid, To: tid}
		}
		all = append(all, ge)
		adj[ge.From] = append(adj[ge.From], ge.To)
		adj[ge.To] = append(adj[ge.To], ge.From)
	}
	keep := map[string]bool{}
	if focus := r.URL.Query().Get("uri"); focus != "" {
		depth, _ := strconv.Atoi(r.URL.Query().Get("depth"))
		if depth <= 0 || depth > 6 {
			depth = 2
		}
		start := "asset:" + focus
		if _, ok := nodes[start]; !ok {
			writeError(w, http.StatusNotFound, "not_found", "no lineage is recorded for that asset")
			return
		}
		// depth counts asset-to-asset hops; each hop crosses a task node.
		frontier := []string{start}
		keep[start] = true
		for i := 0; i < depth*2 && len(frontier) > 0; i++ {
			var next []string
			for _, n := range frontier {
				for _, m := range adj[n] {
					if !keep[m] {
						keep[m] = true
						next = append(next, m)
					}
				}
			}
			frontier = next
		}
	} else {
		for id := range nodes {
			keep[id] = true
		}
	}
	outNodes := make([]map[string]any, 0, len(keep))
	for id := range keep {
		n := nodes[id]
		m := map[string]any{"id": n.ID, "kind": n.Kind, "label": n.Label}
		if n.Kind == "asset" {
			m["uri"] = n.URI
		} else {
			m["workflow"], m["node_id"] = n.Workflow, n.NodeID
		}
		outNodes = append(outNodes, m)
	}
	sort.Slice(outNodes, func(i, j int) bool { return outNodes[i]["id"].(string) < outNodes[j]["id"].(string) })
	outEdges := make([]map[string]any, 0)
	for _, e := range all {
		if keep[e.From] && keep[e.To] {
			outEdges = append(outEdges, map[string]any{"from": e.From, "to": e.To})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": outNodes, "edges": outEdges, "truncated": truncated})
}
